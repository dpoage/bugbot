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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/store"
	"github.com/dpoage/bugbot/internal/util"
)

// cartographySystemPrompt is the cartographer's terse system prompt. The
// model is asked to summarize one package in <=120 words covering purpose,
// invariants, and assumptions about callers. Specific, terse, no preamble —
// the cartographer's value is in the FINDER having a one-shot context for
// unfamiliar code, not in a flowery intro. The wording is intentionally
// short so the model allocates its budget to the actual summary.
const cartographySystemPrompt = `Summarize this package in <=120 words: purpose, key invariants it maintains, what it assumes of callers. Specific, terse, no preamble.`

// cartography holds the per-run package summaries and the package-importer
// graph used to inject "this package + its direct dependents" context into
// finder tasks. A nil cartography represents "feature off" — every
// downstream caller (contextFor, the hypothesize plumbing) handles a nil
// receiver and injects nothing.
type cartography struct {
	summaries map[string]string   // pkgDir -> summary text
	importers map[string][]string // pkgDir -> direct importer pkgDirs
}

// contextFor renders the injection block for a finder unit's files:
// summaries for the files' own packages plus their direct dependents (from
// importers), bounded by cartographyInjectMaxPkgs /
// cartographyInjectMaxBytes. A nil receiver or no matching summaries
// returns "" (the feature is off, or no summary matched the unit).
//
// The block is structured so the model can tell at a glance what is its
// own package and what is a dependency. Dependency packages are marked
// "(dependency)" so the agent does not confuse them with the audit target
// when reasoning about invariants. The leading "verify against the code"
// note is deliberate: summaries are best-effort and may be stale or
// imprecise; the model is told to confirm against the actual code it reads
// (the existing system prompt already licenses this).
//
// Truncation: once either cap is hit the block is closed and a final
// "[truncated]" line is appended so the agent knows the context is partial
// rather than missing without explanation. Packages appear in
// (alphabetical, dependency-last) order, deterministic across runs.
func (c *cartography) contextFor(files []string) string {
	if c == nil || len(c.summaries) == 0 {
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
		if s, ok := c.summaries[own]; ok && s != "" {
			ownList = append(ownList, own)
		}
	}
	for dep := range depSet {
		if s, ok := c.summaries[dep]; ok && s != "" {
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
		line := "  " + label + ": " + util.CollapseWhitespace(c.summaries[pkg]) + "\n"
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

// cartograph runs the cartographer pass: fingerprint each package spanned
// by targets, reuse cached summaries whose stored fingerprint matches,
// regenerate the rest with ONE bounded client.Complete each (no tool loop),
// persist the regenerated set, and return the run's cartography. Returns
// nil when f.opts.Features.Cartographer is false. Degrades gracefully (returns
// whatever summaries it has, possibly nil) on store/LLM errors or when
// budget.finderOverHard() flips to true mid-pass.
//
// The pass runs CONCURRENTLY: each uncached package is summarized in its own
// goroutine bounded by the funnel slot pool (the same bound finders use), and
// each summary is persisted the moment it is produced (see regenSummaries). The
// cartographer runs before the finder stage, so the pool is free and the fan-out
// does not compete with finders. The cost bound (DefaultCartographerMaxFiles
// member files, head-capped, total bytes capped) keeps each completion cheap.
func (f *Funnel) cartograph(ctx context.Context, result *Result, client llm.Client, snap *ingest.Snapshot, targets []string, fps map[string]string, budget *budgetState) *cartography {
	if !f.opts.Features.Cartographer {
		return nil
	}
	if client == nil || snap == nil || len(targets) == 0 {
		// Without inputs the pass has nothing to do. Return a non-nil
		// empty cartography so contextFor still produces a well-defined
		// "no injection" answer; callers that branch on nil must NOT
		// also branch on len.
		return &cartography{summaries: map[string]string{}, importers: map[string][]string{}}
	}

	// Enumerate the packages spanned by targets and compute each
	// package's deterministic fingerprint (see the contract: members =
	// sorted repo-relative paths p with path.Dir(p) == pkg; fp =
	// ingest.HashBytes of "for each p: p + NUL + fps[p] + \n").
	packages := packagesSpanned(targets)
	if len(packages) == 0 {
		return &cartography{summaries: map[string]string{}, importers: map[string][]string{}}
	}
	pkgFingerprints := make(map[string]string, len(packages))
	for pkg, members := range packages {
		pkgFingerprints[pkg] = packageFingerprint(pkg, members, fps)
	}

	// Read whatever the store already has for these packages. Rows with a
	// matching fingerprint are reused verbatim; mismatches and absentees
	// go on the regen list.
	cached, err := f.store.GetPackageSummaries(ctx, util.SortedKeys(pkgFingerprints))
	if err != nil {
		// Store read failure: degrade to empty cartography. The scan
		// proceeds without injection — the byte-identical-off path is
		// still honored because Cartographer=true is the user's request
		// and a degraded pass is still better than a halted scan.
		return &cartography{summaries: map[string]string{}, importers: map[string][]string{}}
	}

	summaries := make(map[string]string, len(pkgFingerprints))
	for pkg, fp := range pkgFingerprints {
		if row, ok := cached[pkg]; ok && row.Fingerprint == fp && row.Summary != "" {
			summaries[pkg] = row.Summary
		}
	}

	// Build the regen list (sorted so the launch order — and therefore the
	// prefix that survives a budget cutoff — is reproducible). Generation and
	// persistence happen concurrently below; completion order is unspecified.
	var toRegen []string
	for pkg, fp := range pkgFingerprints {
		if _, ok := summaries[pkg]; ok {
			continue
		}
		if _, ok := packages[pkg]; !ok {
			continue
		}
		_ = fp
		toRegen = append(toRegen, pkg)
	}
	sort.Strings(toRegen)

	// Regenerate uncached summaries CONCURRENTLY (bounded by the funnel slot
	// pool, the same bound finders use) and persist EACH summary the instant it
	// is produced — never batched — so an interrupted or budget-stopped pass
	// keeps every summary already written instead of discarding the run's work.
	f.regenSummaries(ctx, client, packages, pkgFingerprints, fps, toRegen, budget,
		func(r regenResult) {
			switch {
			case r.err == nil:
				summaries[r.pkg] = r.summary
			case r.stage == "persist":
				// The silent-loss case (tokens spent, nothing stored): surface it
				// as a run note rather than swallowing it.
				f.note(result, fmt.Sprintf("cartographer: persisting summary for %q failed: %v", r.pkg, r.err))
			}
			// summarize failures stay silent, matching the pre-parallel behavior.
		})

	// Build the importer graph scoped to the SPANNED PACKAGES (every file of any
	// package that has a target), not just the target files. contextFor only
	// injects a dependent's summary when that summary exists, and summaries
	// exist exactly for the spanned packages (packagesSpanned). The import that
	// makes package D a dependent of package O may live in a file of D that is
	// not itself a target, so scoping to target files alone would drop that edge
	// and silently omit D's summary. Scoping to every file of the spanned
	// packages captures exactly the edges among summarized packages
	// (byte-identical injection) while staying O(spanned packages) — far below
	// O(snapshot) on a large repo. A failure returns an empty importers map
	// (dependency injection degrades to "own package only", not a failed scan).
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

	return &cartography{summaries: summaries, importers: importers}
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

// summarizePackage builds the bounded input for one package's summary and runs
// a zero-tool agent.Runner via RunJSON to produce it. The input is the
// package's member files head-truncated to DefaultCartographerHeadLines, the
// whole package capped at DefaultCartographerInputBytes. The runner shares the
// finder budget pool (via budget.finderRunnerLimits) so an in-flight summary
// respects the run-wide token budget; budget may be nil (no pool gating). The
// output is the schema's "summary" field trimmed of whitespace; an empty result
// is reported as an error so the caller drops the package from the regen batch.
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
	summary := strings.TrimSpace(out.Summary)
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
