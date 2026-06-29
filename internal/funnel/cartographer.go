package funnel

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/store"
	"github.com/dpoage/bugbot/internal/util"
	"golang.org/x/sync/singleflight"
)

// cartographySystemPrompt is the cartographer's terse system prompt. The
// model is asked to summarize one package in <=120 words covering purpose,
// invariants, and assumptions about callers. The prompt explicitly forbids
// the format patterns the cartographer's model has historically drifted
// into: markdown heading lines, bold "**Purpose:**"-style labels, and any
// other preamble. Those shapes were once tolerated (the old free-form
// prompt let them appear nondeterministically); the post-processor
// (normalizeSummary) is the byte-uniform guarantee, and the prompt forbids
// the same shapes as cheap belt-and-suspenders.
const cartographySystemPrompt = `Summarize this package in <=120 words covering purpose, key invariants it maintains, and what it assumes of callers. Output requirements: a single paragraph, no markdown heading, no bold label, no preamble, no list. Plain prose only.`

// cartography holds the per-run package summaries and the package-importer
// graph used to inject "this package + its direct dependents" context into
// finder tasks. A nil cartography represents "feature off" — every
// downstream caller (contextFor, the hypothesize plumbing) handles a nil
// receiver and injects nothing.
//
// In lazy mode (newCartographer path), summaries is populated on demand by
// ensureContextFor rather than up-front. The mu guard + sf singleflight group
// ensure concurrent finder units spanning the same un-summarized package
// generate the summary exactly once and share the result.
//
// Lazy-mode transport breaker (bugbot-1r9): against an unreachable provider
// every summarizePackage call exhausts the retry policy (~3.5s,
// llm.APIError with StatusCode==0) and there are many packages, so the
// scan would grind through cartography burning the retry budget
// package-by-package. The breaker mirrors the finder breaker (bugbot-2uz):
// transportFailures counts transport-class generation failures observed
// while anySuccess is still false; once it reaches breakerThreshold and
// anySuccess has never been set, the breaker trips and every subsequent
// lazy generation short-circuits to ("", false). anySuccess is set ONLY on
// a successful GENERATION — never on a memo/store hit — so a cached
// summary from a prior run does not disarm the breaker for the current
// one (the provider may be unreachable NOW). Concurrency: atomics +
// CompareAndSwap only; c.mu still guards ONLY the summaries map.
type cartography struct {
	summaries map[string]string   // pkgDir -> summary text (memo; guarded by mu)
	importers map[string][]string // pkgDir -> direct importer pkgDirs (built eagerly, pure-Go)

	// Lazy-generation state. Populated by newCartographer; zero in legacy path.
	mu       sync.Mutex
	sf       singleflight.Group
	funnel   *Funnel
	client   llm.Client
	packages map[string][]string // pkgDir -> sorted member files (from packagesSpanned)
	pkgFps   map[string]string   // pkgDir -> fingerprint
	fps      map[string]string   // file -> content fingerprint
	budget   *budgetState
	enabled  bool    // mirrors f.opts.Features.Cartographer; false -> nil behavior
	result   *Result // run Result; used solely by the breaker to append a trip note (bugbot-1r9)

	// Transport breaker (bugbot-1r9). See struct doc above.
	transportFailures atomic.Int32 // transport-class generation failures while !anySuccess
	anySuccess        atomic.Bool  // true once any generate() produced a summary; permanent disarm
	breakerTripped    atomic.Bool  // true after the breaker stopped new generations
}

// contextFor renders the injection block for a finder unit's files using the
// current in-memory summaries memo. It is the legacy (eager-pass) entry point
// that passes the lock-held summaries snapshot directly to renderContext. A nil
// receiver or no matching summaries returns "" (feature off, or no summary
// matched the unit).
func (c *cartography) contextFor(files []string) string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	sums := c.summaries
	c.mu.Unlock()
	return c.renderContext(files, sums)
}

// breakerThreshold returns the lazy-mode transport breaker's trip threshold:
// max(3, MaxParallel). At least 3 so a single transient blip never trips it,
// and at least the configured concurrency so a parallel batch of transport
// failures trips within one generation. Mirrors the finder breaker
// (bugbot-2uz). Nil-safe (used only on enabled, non-nil cartography).
func (c *cartography) breakerThreshold() int32 {
	t := c.funnel.opts.Limits.MaxParallel
	if t < 3 {
		t = 3
	}
	return int32(t)
}

// generate is the shared lazy-mode generation path used by BOTH
// ensureContextFor and getSummary. It is the single place that owns
// singleflight + summarizePackage + persist so the two call sites cannot
// diverge, and the single place that owns the lazy-mode transport
// breaker. Callers must have already filtered memo hits, store hits,
// and the budget-gate before invoking generate; only NEW LLM generations
// flow through here, and only those are subject to the breaker. The
// breaker short-circuit returns ("", false) without invoking summarizePackage,
// so a confirmed-unreachable provider does not burn the retry budget
// package-by-package.
//
// Outcome classification inside the singleflight closure mirrors the
// finder breaker (bugbot-2uz):
//
//   - summarizePackage success: set anySuccess (permanent disarm), persist
//     with a cancel-detached context, memoize, return (summary, true).
//   - transport-class failure (isTransportError) while !anySuccess: count
//     toward breakerThreshold; on the threshold-th failure
//     CompareAndSwap breakerTripped false→true so exactly one goroutine
//     trips. Non-transport failures and empty summaries do NOT arm the
//     counter — those have their own classification (rate-limit, parse
//     failure) and must not be conflated with a systemic outage.
//
// Concurrency: the singleflight serialises one closure per pkg; multiple
// packages run concurrently. Atomically-counted transport failures and a
// CompareAndSwap trip guarantee the breaker fires exactly once regardless
// of how many goroutines reach the threshold simultaneously.
func (c *cartography) generate(ctx context.Context, pkg string, members []string, fp string) (string, bool) {
	if c.breakerTripped.Load() {
		return "", false
	}
	result, _, _ := c.sf.Do(pkg, func() (interface{}, error) {
		summary, err := c.funnel.summarizePackage(ctx, c.client, c.budget, pkg, members, c.fps)
		if err != nil || summary == "" {
			// Transport-class failure while the breaker is still armed.
			// Mirror the finder breaker: count transport failures toward
			// the threshold; non-transport failures and empty summaries
			// do NOT arm the counter. CompareAndSwap guarantees exactly
			// one goroutine trips regardless of how many reach the
			// threshold concurrently. The trip-note is best-effort; a
			// missing c.result (e.g. a degenerate newCartographer call)
			// simply skips the note.
			if !c.anySuccess.Load() && isTransportError(err) {
				thresh := c.breakerThreshold()
				if n := c.transportFailures.Add(1); n >= thresh && c.breakerTripped.CompareAndSwap(false, true) {
					if c.result != nil {
						c.funnel.note(c.result, fmt.Sprintf("cartographer circuit breaker tripped: %d transport failures with zero successes (threshold %d) — aborting further cartographer generations", n, thresh))
					}
				}
			}
			return "", nil // degrade silently (same as eager pass)
		}
		// Successful generation — disarm permanently.
		c.anySuccess.Store(true)
		// Persist immediately with a cancel-detached context so an
		// interruption does not discard an already-produced summary.
		pCtx, pCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		_ = c.funnel.store.UpsertPackageSummaries(pCtx, []store.PackageSummary{
			{Pkg: pkg, Fingerprint: fp, Summary: summary},
		})
		pCancel()
		return summary, nil
	})
	if s, ok := result.(string); ok && s != "" {
		c.mu.Lock()
		c.summaries[pkg] = s
		c.mu.Unlock()
		return s, true
	}
	return "", false
}

// ensureContextFor is the lazy entry point used by the scan path. For each
// package spanned by files (own packages + their direct dependents from
// importers), it materialises the summary on demand:
//
//  1. Memo hit (mu-protected in-memory map) → use immediately.
//  2. Store hit (GetPackageSummaries, fingerprint match) → populate memo, use.
//  3. Miss → generate() (singleflight.Do + summarizePackage + persist +
//     transport breaker) → memo.
//
// Budget gate: if budget.finderOverHard() skip generation and render only
// cached/memoized (same degradation as the old eager pass when budget trips
// mid-cartographer-run).
//
// A nil receiver (feature off) returns "" without acquiring any lock.
func (c *cartography) ensureContextFor(ctx context.Context, files []string) string {
	if c == nil || !c.enabled {
		return ""
	}

	// Compute the set of packages we need: own + direct dependents.
	ownSet := make(map[string]struct{})
	for _, f := range files {
		d := path.Dir(f)
		if d == "." {
			d = ""
		}
		ownSet[d] = struct{}{}
	}
	depSet := make(map[string]struct{})
	for own := range ownSet {
		for _, dep := range c.importers[own] {
			depSet[dep] = struct{}{}
		}
	}
	delete(depSet, "")

	needed := make([]string, 0, len(ownSet)+len(depSet))
	for pkg := range ownSet {
		if pkg != "" {
			needed = append(needed, pkg)
		}
	}
	for pkg := range depSet {
		needed = append(needed, pkg)
	}

	// Phase 1: collect memo hits and build the miss list.
	c.mu.Lock()
	var miss []string
	for _, pkg := range needed {
		if _, ok := c.summaries[pkg]; !ok {
			miss = append(miss, pkg)
		}
	}
	c.mu.Unlock()

	if len(miss) > 0 {
		budgetHard := c.budget != nil && c.budget.finderOverHard()

		// Phase 2: store lookup for misses (one batched query).
		var storeHit map[string]store.PackageSummary
		if !budgetHard {
			storeHit, _ = c.funnel.store.GetPackageSummaries(ctx, miss)
		}

		for _, pkg := range miss {
			members := c.packages[pkg]
			fp := c.pkgFps[pkg]

			// Store hit with fresh fingerprint → populate memo, skip generation.
			if row, ok := storeHit[pkg]; ok && row.Fingerprint == fp && row.Summary != "" {
				c.mu.Lock()
				c.summaries[pkg] = row.Summary
				c.mu.Unlock()
				continue
			}

			// Budget hard → skip generation entirely; render whatever is cached.
			if budgetHard {
				continue
			}

			// Generate via the shared lazy-mode path. generate() owns the
			// singleflight + persist + transport breaker; memo hits, store
			// hits, and the budget-gate have already been handled above so a
			// tripped breaker only short-circuits fresh LLM generations here.
			_, _ = c.generate(ctx, pkg, members, fp)
		}
	}

	// Snapshot the memo under lock and render.
	c.mu.Lock()
	sums := make(map[string]string, len(c.summaries))
	for k, v := range c.summaries {
		sums[k] = v
	}
	c.mu.Unlock()
	return c.renderContext(files, sums)
}

// renderContext builds the injection block given a pre-materialised summaries
// snapshot. It is called by both contextFor (legacy) and ensureContextFor
// (lazy) so the truncation/ordering logic lives in exactly one place.
// A nil/empty summaries map returns "".
func (c *cartography) renderContext(files []string, summaries map[string]string) string {
	if len(summaries) == 0 {
		return ""
	}
	// Collect unique own packages of the unit's files.
	ownSet := make(map[string]struct{})
	for _, f := range files {
		d := path.Dir(f)
		if d == "." {
			d = ""
		}
		ownSet[d] = struct{}{}
	}
	// Collect direct dependents (from importers) for each own package.
	depSet := make(map[string]struct{})
	for own := range ownSet {
		for _, dep := range c.importers[own] {
			depSet[dep] = struct{}{}
		}
	}
	delete(depSet, "") // never inject the root package; it has no usable summary

	// Stable ordering: own packages first, then dependents, both
	// alphabetical. This keeps the injection block byte-deterministic
	// across runs with the same file set, which matters for prompt-cache
	// prefix stability.
	var ownList, depList []string
	for own := range ownSet {
		if own == "" {
			continue
		}
		if s, ok := summaries[own]; ok && s != "" {
			ownList = append(ownList, own)
		}
	}
	for dep := range depSet {
		if s, ok := summaries[dep]; ok && s != "" {
			depList = append(depList, dep)
		}
	}
	sort.Strings(ownList)
	sort.Strings(depList)
	ordered := append(ownList, depList...)

	if len(ordered) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("REPO CONTEXT — package summaries (verify against the code before relying on them):\n")
	wrote := 0
	for i, pkg := range ordered {
		if wrote >= cartographyInjectMaxPkgs {
			b.WriteString("  [truncated: package limit reached]\n")
			break
		}
		label := pkg
		if i >= len(ownList) {
			label = pkg + " (dependency)"
		}
		// "+ <label>: <summary>\n" — the leading 2-space indent matches
		// the TARGET FILES list in finderTask so the model sees a
		// consistent visual rhythm. Newlines in the summary are collapsed
		// to spaces to keep the block single-line-per-package; multi-line
		// summaries would confuse the truncation math.
		line := "  " + label + ": " + util.CollapseWhitespace(summaries[pkg]) + "\n"
		if b.Len()+len(line) > cartographyInjectMaxBytes {
			b.WriteString("  [truncated: byte limit reached]\n")
			break
		}
		b.WriteString(line)
		wrote++
	}
	return b.String()
}

// QueryGraph returns the importers and/or imports of pkg from the importer
// graph already held in the cartography struct. pkg must be a resolved
// package directory (call packagesSpanned or path.Dir on a file path first).
//
// The importers field stores pkgDir -> []importerPkgDir (who imports pkgDir).
// The "imports" direction is derived by inversion (find all Y where pkg ∈
// importers[Y]). Both returned slices are sorted and capped at
// packageGraphMaxEntries (defined in internal/agent — callers must cap
// themselves; this method does NOT apply the cap so tests can inspect raw
// counts; the funnel callback wraps this and the tool applies the cap via
// writeList).
//
// A nil receiver (feature off) returns empty slices and no error.
// An unknown package also returns empty slices and no error (the model gets
// an "empty" result and should fall back to reading source).
func (c *cartography) QueryGraph(pkg, direction string) (importerList, importList []string) {
	if c == nil {
		return nil, nil
	}

	if direction == "importers" || direction == "both" {
		importerList = append(importerList, c.importers[pkg]...)
		sort.Strings(importerList)
	}

	if direction == "imports" || direction == "both" {
		// Invert the importers map: find every package Y where pkg ∈ importers[Y].
		for candidate, imps := range c.importers {
			for _, imp := range imps {
				if imp == pkg {
					importList = append(importList, candidate)
					break
				}
			}
		}
		sort.Strings(importList)
	}

	return importerList, importList
}

// newCartographer constructs a lazy cartography provider. Unlike cartograph it
// does NOT summarize anything up front: it builds only the importer graph
// (pure-Go, needed across all finder units for cross-package dependents) and
// stores the client/fps/budget for on-demand use by ensureContextFor.
//
// Returns nil when f.opts.Features.Cartographer is false (byte-identical off
// path). Returns a non-nil cartography with an empty summaries memo and a
// pre-built importers graph when enabled — even when client/snap/targets are
// nil/empty (same degenerate-but-non-nil contract as cartograph).
func (f *Funnel) newCartographer(ctx context.Context, result *Result, client llm.Client, snap *ingest.Snapshot, targets []string, fps map[string]string, budget *budgetState) *cartography {
	if !f.opts.Features.Cartographer {
		return nil
	}
	if client == nil || snap == nil || len(targets) == 0 {
		return &cartography{
			summaries: map[string]string{},
			importers: map[string][]string{},
			enabled:   true,
			funnel:    f,
			client:    client,
			packages:  map[string][]string{},
			pkgFps:    map[string]string{},
			fps:       fps,
			budget:    budget,
			result:    result,
		}
	}

	packages := packagesSpanned(targets)
	pkgFps := make(map[string]string, len(packages))
	for pkg, members := range packages {
		pkgFps[pkg] = packageFingerprint(pkg, members, fps)
	}

	// Build the importer graph eagerly (pure-Go, O(spanned packages), no LLM).
	spannedDirs := make(map[string]bool, len(packages))
	for pkg := range packages {
		spannedDirs[pkg] = true
	}
	inScope := make(map[string]bool)
	for _, file := range snap.Files {
		if spannedDirs[path.Dir(file.Path)] {
			inScope[file.Path] = true
		}
	}
	importers, impErr := f.repo.PackageImportersScoped(ctx, snap, inScope)
	if impErr != nil || importers == nil {
		importers = map[string][]string{}
	}

	return &cartography{
		summaries: map[string]string{},
		importers: importers,
		enabled:   true,
		funnel:    f,
		client:    client,
		packages:  packages,
		pkgFps:    pkgFps,
		fps:       fps,
		budget:    budget,
		result:    result,
	}
}

// getSummary returns the summary for a single package, generating it on demand
// if needed (same memo/singleflight/store logic as ensureContextFor, without
// the rendering step). Returns ("", false) when the package is unknown or
// generation was budget-skipped. A nil receiver returns ("", false).
func (c *cartography) getSummary(ctx context.Context, pkg string) (string, bool) {
	if c == nil || !c.enabled || pkg == "" {
		return "", false
	}

	// Memo hit.
	c.mu.Lock()
	if s, ok := c.summaries[pkg]; ok {
		c.mu.Unlock()
		return s, s != ""
	}
	c.mu.Unlock()

	budgetHard := c.budget != nil && c.budget.finderOverHard()

	// Store hit.
	if !budgetHard {
		if rows, err := c.funnel.store.GetPackageSummaries(ctx, []string{pkg}); err == nil {
			if row, ok := rows[pkg]; ok && row.Fingerprint == c.pkgFps[pkg] && row.Summary != "" {
				c.mu.Lock()
				c.summaries[pkg] = row.Summary
				c.mu.Unlock()
				return row.Summary, true
			}
		}
	}
	members := c.packages[pkg]

	if len(members) == 0 || budgetHard {
		return "", false
	}
	fp := c.pkgFps[pkg]

	// Generate via the shared lazy-mode path. generate() owns the
	// singleflight + persist + transport breaker; memo hits, store hits,
	// and the budget-gate have already been handled above so a tripped
	// breaker only short-circuits fresh LLM generations here.
	return c.generate(ctx, pkg, members, fp)
}

// regenResult is one package's outcome from regenSummaries. err == nil means the
// summary was produced AND persisted; otherwise stage names the failed step
// ("summarize" or "persist").
type regenResult struct {
	pkg     string
	summary string
	err     error
	stage   string
}

// regenSummaries summarizes each package in toRegen CONCURRENTLY — bounded by the
// funnel-wide slot pool (slotLow, the same class and bound as finder agents; the
// cartographer runs before the finder stage so the pool is free) — and persists
// EACH summary the instant it is produced, one row per package, never batched.
// An interrupted or budget-stopped pass therefore keeps every summary already
// written instead of discarding the whole run's work.
//
// budget may be nil (no gating); when non-nil the loop stops launching and each
// in-flight goroutine bails the moment the finder hard budget trips, mirroring
// the finder stage. Launch order follows toRegen (callers sort it for a
// reproducible prefix under a budget cutoff); completion order is unspecified.
//
// onResult is invoked once per package, SERIALIZED, so callers need no extra
// locking. Persistence uses a cancel-detached, time-bounded context so a summary
// produced just before ctx cancellation is still saved.
func (f *Funnel) regenSummaries(
	ctx context.Context,
	client llm.Client,
	packages map[string][]string,
	pkgFingerprints map[string]string,
	fps map[string]string,
	toRegen []string,
	budget *budgetState,
	onResult func(regenResult),
) {
	persistParent := context.WithoutCancel(ctx)
	var mu sync.Mutex
	// The fanout owns the per-package goroutine, the slotLow acquire/release, the
	// WaitGroup, and a derived cancellable runCtx so a goroutine blocked waiting
	// for a slot unblocks on cancellation; the caller's ctx is never cancelled by
	// us.
	fo := f.newFanout(ctx, slotLow)

	report := func(r regenResult) {
		if onResult == nil {
			return
		}
		mu.Lock()
		onResult(r)
		mu.Unlock()
	}

	for _, pkg := range toRegen {
		// Pre-launch break: stop admitting once the run is cancelled or the budget
		// is exhausted. ctx is the fanout's parent and regenSummaries never calls
		// fo.stop, so ctx.Err() coincides with the workers' runCtx.Err().
		if ctx.Err() != nil {
			break
		}
		if budget != nil && budget.finderOverHard() {
			break
		}
		pkg := pkg
		if !fo.spawnSerial(func(runCtx context.Context) {
			// Re-check now that we hold a slot: earlier units may have tripped the
			// budget, or the run may have been cancelled, while we waited. The slot
			// pool's fast path can hand out a slot on an already-cancelled ctx, so
			// this recheck is the cartographer's own guard, not the primitive's.
			if runCtx.Err() != nil {
				return
			}
			if budget != nil && budget.finderOverHard() {
				return
			}
			summary, sErr := f.summarizePackage(runCtx, client, budget, pkg, packages[pkg], fps)
			if sErr != nil || summary == "" {
				if sErr == nil {
					sErr = errors.New("cartograph: empty summary")
				}
				report(regenResult{pkg: pkg, err: sErr, stage: "summarize"})
				return
			}
			row := store.PackageSummary{Pkg: pkg, Fingerprint: pkgFingerprints[pkg], Summary: summary}
			// Persist on the fly with a cancel-detached, time-bounded context so an
			// interruption cannot discard a summary that was already produced.
			pCtx, pCancel := context.WithTimeout(persistParent, 30*time.Second)
			pErr := f.store.UpsertPackageSummaries(pCtx, []store.PackageSummary{row})
			pCancel()
			if pErr != nil {
				report(regenResult{pkg: pkg, summary: summary, err: pErr, stage: "persist"})
				return
			}
			report(regenResult{pkg: pkg, summary: summary})
		}) {
			break // ctx cancelled while waiting for a slot
		}
	}
	fo.wait()
}

// cartographySummarySchema constrains the package-summary completion to a
// single {"summary": string} object. Routing the summary through RunJSON with
// this schema (instead of a bare client.Complete) gives the cartographer the
// same guarantees every other agent has: shape validation, a one-shot repair
// round-trip when the first completion is malformed (so a bad summary is fixed,
// not silently dropped), and think-block stripping via stripBody.
var cartographySummarySchema = json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string","minLength":1,"description":"<=120 word package summary"}},"required":["summary"],"additionalProperties":false}`)

// cartographySummaryMaxWords is the post-processed word cap applied to every
// package summary before it is persisted or returned. It matches the 120-word
// limit the system prompt requests. The normalizer enforces it
// deterministically (truncating to the cap and appending a single ellipsis
// character when the model overshoots), independent of model behavior.
const cartographySummaryMaxWords = 120

// cartographySummaryHeadingRE matches ONE leading markdown heading line and any
// optional label it carries, e.g. "# Package Summary", "## Summary", "#
// Package: <name>", "### Anything". normalizeSummary applies it in a loop to
// strip a stacked heading block. Anchored at the start (no multiline flag) so
// it only ever strips the leading line, never a mid-text '#'. A summary that is
// nothing but a hash line collapses to "" and the package is dropped from the
// regen batch — acceptable since the prompt forbids markdown headings.
var cartographySummaryHeadingRE = regexp.MustCompile(`^\s*(?:#+\s*).*?(?:\n|$)`)

// cartographySummaryLabelRE matches a leading bold-label preamble, e.g.
// "**Purpose:**", "**Package Purpose:**", "**Purpose**:", "**Goal** -",
// "**Overview**:". The label word(s) and the optional colon (inside or
// outside the bold) are stripped; the remainder of the line is discarded so
// the first real sentence stands on its own.
var cartographySummaryLabelRE = regexp.MustCompile(`(?ims)^\s*\*\*[^*\n]*\*\*\s*[:\-]?\s*\n?`)

// normalizeSummary deterministically cleans a model-produced package summary
// into a single bounded paragraph regardless of the model's output shape.
// Steps, in order:
//
//  1. Strip every leading markdown heading line (a stacked "# X\n## Y" block
//     included), each with any optional label like "Package Summary" or
//     "Package: <name>". A heading must have whitespace after the hashes.
//  2. Strip a leading bold-label preamble ("**Purpose:**",
//     "**Package Purpose:**", "**Purpose**:", case-insensitive, with the
//     colon either inside or outside the bold).
//  3. Collapse every run of whitespace (spaces, tabs, newlines) into a
//     single space and trim the result.
//  4. Enforce a word cap (cartographySummaryMaxWords). If the model
//     overshoots, keep the first cap words and append a single ellipsis
//     character. Words are counted with strings.Fields, which already
//     operates on runes (so multibyte CJK / accented text is never split
//     mid-character and the resulting count matches what a human reader
//     would call "words").
//
// The two leading-shape regexes are precompiled at package scope: this
// function is called once per package per regen pass, so the cost is
// negligible, and a regex is both clearer and safer than hand-rolled byte
// scanning for the multi-character heading/label shapes we tolerate. All
// other work is plain strings (TrimSpace, Fields, Join) — no extra
// allocations from scanning once the regexes have matched.
func normalizeSummary(s string) string {
	// 1. Strip every leading markdown heading line. Looped because the regex
	//    is start-anchored: one ReplaceAllString removes only the first
	//    heading; re-running re-anchors ^ at the reduced string so a stacked
	//    "# Title\n## Subtitle" block is fully removed.
	for cartographySummaryHeadingRE.MatchString(s) {
		s = cartographySummaryHeadingRE.ReplaceAllString(s, "")
	}
	// 2. Bold-label preamble.
	s = cartographySummaryLabelRE.ReplaceAllString(s, "")
	// 3. Collapse whitespace. fields + Join is the idiomatic,
	//    allocation-light "split on any unicode whitespace, rejoin with
	//    single spaces" combo used elsewhere in this package (e.g.
	//    prompt.go, strategy.go).
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	if s == "" {
		return ""
	}
	// 4. Word cap.
	words := strings.Fields(s)
	if len(words) > cartographySummaryMaxWords {
		words = words[:cartographySummaryMaxWords]
		return strings.Join(words, " ") + " …"
	}
	return s
}

// summarizePackage builds the bounded input for one package's summary and runs
// a zero-tool agent.Runner via RunJSON to produce it. The input is the
// package's member files head-truncated to DefaultCartographerHeadLines, the
// whole package capped at DefaultCartographerInputBytes. The runner shares the
// finder budget pool (via budget.finderRunnerLimits) so an in-flight summary
// respects the run-wide token budget; budget may be nil (no pool gating). The
// output is the schema's "summary" field, deterministically normalized to a
// single bounded paragraph (see normalizeSummary) before being returned; an
// empty result is reported as an error so the caller drops the package from
// the regen batch.
func (f *Funnel) summarizePackage(ctx context.Context, client llm.Client, budget *budgetState, pkg string, members []string, fps map[string]string) (string, error) {
	if len(members) == 0 {
		return "", errors.New("cartograph: empty members for package")
	}
	root := f.repo.Root()

	// Bound the member set: at most DefaultCartographerMaxFiles. Pick the
	// first N (members are already deterministic-sorted by
	// packagesSpanned) so the chosen set is reproducible.
	if len(members) > DefaultCartographerMaxFiles {
		members = members[:DefaultCartographerMaxFiles]
	}

	// Compose the user message: a brief preamble then a per-file block.
	var body strings.Builder
	body.WriteString("Package: ")
	body.WriteString(pkg)
	body.WriteString("\n\nFiles:\n")
	const perFileHead = DefaultCartographerInputBytes / 4 // soft budget: each file gets a quarter of the cap
	for _, rel := range members {
		if body.Len() >= DefaultCartographerInputBytes {
			body.WriteString("  [additional files omitted to fit budget]\n")
			break
		}
		abs := filepath.Join(root, filepath.FromSlash(rel))
		content, err := readFileHead(abs, DefaultCartographerHeadLines, perFileHead)
		if err != nil {
			// Unreadable file (deleted, race): skip with a one-liner so
			// the model knows the file was once here.
			fmt.Fprintf(&body, "--- %s ---\n  (unreadable: %v)\n", rel, err)
			continue
		}
		// Stop writing more files once the running total exceeds the
		// cap. The per-file head was sized so a typical file fits
		// comfortably, but a single oversized file is truncated at its
		// own cap rather than spilling the rest.
		projected := body.Len() + len(content) + len(rel) + 8
		if projected > DefaultCartographerInputBytes {
			body.WriteString("  [additional files omitted to fit budget]\n")
			break
		}
		body.WriteString("--- ")
		body.WriteString(rel)
		body.WriteString(" ---\n")
		body.WriteString(content)
		if !strings.HasSuffix(content, "\n") {
			body.WriteString("\n")
		}
	}

	limits := f.opts.Limits.FinderLimits
	if budget != nil {
		// The cartographer shares the finder pool, so its per-run limits and
		// per-turn budget check come from finderRunnerLimits — the same hook
		// finders use to stop mid-run when the shared budget is exhausted.
		limits = budget.finderRunnerLimits(f.opts.Limits.FinderLimits)
	}
	// Surface this per-package summary as an in-flight agent in `bugbot status`
	// and the live pane via the shared AgentScope seam. The cartographer drives
	// a single tool-less completion, so there is no per-turn tool-call activity;
	// the started/finished bracket is what shows the operator the cartograph
	// stage is doing work (and on which package).
	progress.NewAgentScope(f.opts.Progress, progress.RoleCartographer, pkg).Start()
	runner := f.newAgentRunner(client, nil, cartographySystemPrompt, limits)
	var out struct {
		Summary string `json:"summary"`
	}
	start := time.Now()
	outcome, err := runner.RunJSON(ctx, body.String(), cartographySummarySchema, &out)
	emitAgentFinished(f.opts.Progress, progress.RoleCartographer, pkg, outcome, start, err)
	if err != nil {
		return "", err
	}
	// Deterministic post-process: strip any leading heading/label the
	// model added, collapse to one paragraph, enforce the word cap. This
	// is the byte-uniform guarantee on the summary regardless of model
	// behavior; the system prompt forbids the same shapes as cheap
	// belt-and-suspenders but cannot enforce them.
	summary := normalizeSummary(out.Summary)
	if summary == "" {
		return "", errors.New("cartograph: empty summary from LLM")
	}
	return summary, nil
}

// readFileHead returns the first maxLines lines of abs, capped at
// maxBytes. Used by the cartographer to bound each member file's
// contribution to the summary input without reading it whole.
//
// Lines are read with bufio.Scanner, which counts newlines correctly on
// every supported OS. The byte cap is a soft second constraint applied
// AFTER line-capping: a file with 5 long lines stops at the first newline
// that pushes the running total over maxBytes.
func readFileHead(abs string, maxLines, maxBytes int) (string, error) {
	f, err := os.Open(abs)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	// Allow long lines (e.g. minified blobs) without breaking the scan.
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var b strings.Builder
	line := 0
	for sc.Scan() {
		b.Write(sc.Bytes())
		b.WriteByte('\n')
		line++
		if line >= maxLines || b.Len() >= maxBytes {
			break
		}
	}
	if err := sc.Err(); err != nil {
		return b.String(), err
	}
	return b.String(), nil
}

// packagesSpanned groups targets by their parent package directory. The
// returned map is keyed by package (repo-relative dir) and its value is the
// sorted list of member files. Repo-root files (path.Dir == ".") are SKIPPED:
// the root holds build/config/doc files rather than a coherent code package,
// its empty key cannot be persisted (UpsertPackageSummaries rejects an empty
// Pkg, and as one transaction a single such row would poison the whole batch),
// and contextFor never injects it. Sort is by path so the fingerprint and the
// DefaultCartographerMaxFiles subset are deterministic.
func packagesSpanned(targets []string) map[string][]string {
	pkgs := make(map[string][]string)
	for _, t := range targets {
		d := path.Dir(t)
		if d == "." {
			continue // repo-root file: not a storable package (see doc)
		}
		pkgs[d] = append(pkgs[d], t)
	}
	for d := range pkgs {
		sort.Strings(pkgs[d])
	}
	return pkgs
}

// packageFingerprint is the deterministic package fingerprint used as the
// cache key. The recipe (per the contract): for each member p in sorted
// order, append p + NUL + fps[p] + LF; feed the result to
// ingest.HashBytes. Empty members or empty fps[p] are tolerated (the
// resulting fingerprint still changes when content does), but callers
// that want a strict "must have content" guarantee should pre-filter.
func packageFingerprint(pkg string, members []string, fps map[string]string) string {
	var b strings.Builder
	for _, m := range members {
		b.WriteString(m)
		b.WriteByte(0)
		b.WriteString(fps[m])
		b.WriteByte('\n')
	}
	_ = pkg // pkg name is implicit in the members (their path.Dir matches it)
	return ingest.HashBytes([]byte(b.String()))
}

// CartographyReport summarizes an out-of-band cartographer refresh.
type CartographyReport struct {
	ScanRunID    string // the scan_run (kind "cartography") this refresh ledgered to
	Packages     int    // packages spanned by the repo snapshot
	Summarized   int    // packages (re)generated and persisted this run
	Reused       int    // packages whose cached summary fingerprint still matched
	Failed       int    // packages whose summary generation returned empty/error
	InputTokens  int64  // total input tokens billed by the refresh
	OutputTokens int64  // total output tokens billed by the refresh
}

// RefreshCartography runs the cartographer pass out-of-band — no finder or
// verify stages — over the whole repo snapshot: it (re)summarizes every package
// whose content fingerprint changed and persists the results, exactly the
// fingerprint-cached summaries a scan's cartographer pass produces. Spend is
// ledgered to a fresh scan_run of kind "cartography" (so it shows in the
// metrics series, classified as a cartographer run). client is the unwrapped
// cartographer LLM client; it is recorder-wrapped internally. Unlike the
// in-scan pass this does NOT gate on a finder budget — a manual refresh runs to
// completion — and it returns counts so the caller can report what happened.
func (f *Funnel) RefreshCartography(ctx context.Context, client llm.Client) (CartographyReport, error) {
	var rep CartographyReport
	if client == nil {
		return rep, errors.New("cartographer: nil client")
	}
	snap, err := f.repo.Snapshot(ctx, f.opts.Discovery.Filter)
	if err != nil {
		return rep, fmt.Errorf("cartographer: snapshot: %w", err)
	}
	fps, err := f.repo.Fingerprints(ctx, snap)
	if err != nil {
		return rep, fmt.Errorf("cartographer: fingerprints: %w", err)
	}
	targets := make([]string, len(snap.Files))
	for i, file := range snap.Files {
		targets[i] = file.Path
	}
	packages := packagesSpanned(targets)
	rep.Packages = len(packages)
	if len(packages) == 0 {
		return rep, nil
	}

	runID, err := f.store.BeginScanRun(ctx, store.ScanCartography, snap.Commit)
	if err != nil {
		return rep, fmt.Errorf("cartographer: begin run: %w", err)
	}
	rep.ScanRunID = runID
	rec := &spendRecorder{ctx: ctx, store: f.store, scanRunID: runID}
	cc := llm.WithRecorder(client, rec, roleCartographer, "", "")

	pkgFingerprints := make(map[string]string, len(packages))
	for pkg, members := range packages {
		pkgFingerprints[pkg] = packageFingerprint(pkg, members, fps)
	}
	keys := util.SortedKeys(pkgFingerprints)
	cached, cErr := f.store.GetPackageSummaries(ctx, keys)
	if cErr != nil {
		cached = nil // degrade: regenerate everything
	}
	// Reused = cache hits; everything else goes on the regen list. keys is
	// already sorted (sortedKeys), so the launch order is reproducible.
	var toRegen []string
	for _, pkg := range keys {
		fp := pkgFingerprints[pkg]
		if row, ok := cached[pkg]; ok && row.Fingerprint == fp && row.Summary != "" {
			rep.Reused++
			continue
		}
		toRegen = append(toRegen, pkg)
	}

	// Summarize and persist each uncached package concurrently and on the fly,
	// so a manual refresh interrupted partway keeps every summary already
	// written. No finder-budget gating: a manual refresh runs to completion.
	var persistErr error
	f.regenSummaries(ctx, cc, packages, pkgFingerprints, fps, toRegen, nil,
		func(r regenResult) {
			switch {
			case r.err == nil:
				rep.Summarized++
			case r.stage == "persist":
				rep.Failed++
				persistErr = r.err
			default: // summarize failure
				rep.Failed++
			}
		})

	rep.InputTokens, rep.OutputTokens, _, _ = rec.totals()
	statsBlob, _ := json.Marshal(Stats{
		CartographerEnabled: true,
		InputTokens:         rep.InputTokens,
		OutputTokens:        rep.OutputTokens,
	})
	_ = f.store.FinishScanRun(ctx, runID, string(statsBlob))
	if persistErr != nil {
		return rep, fmt.Errorf("cartographer: persist: %w", persistErr)
	}
	return rep, nil
}
