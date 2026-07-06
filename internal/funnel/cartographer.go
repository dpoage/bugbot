package funnel

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/llm"
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
