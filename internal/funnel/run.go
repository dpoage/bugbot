package funnel

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/store"
)

// epochSentinelParsed is the parsed form of the epoch sentinel written by
// RefreshContentHashes for never-scanned rows. It is package-level so
// applySweepOrder (and tests in coverage_test.go) can reference it without
// importing the store package's internal epoch constant.
var epochSentinelParsed = store.EpochSentinelTime()

// Sweep runs the funnel over the entire current snapshot of the repository. It
// is the manual `bugbot scan` and periodic-sweep entrypoint.
//
// Ordering: when heat ordering is enabled (the default), Sweep uses a
// two-group anti-starvation scheme via applySweepOrder:
//
//   - Group 1 (never-scanned or epoch-sentinel): heat-ordered within the group.
//   - Group 2 (previously scanned): stalest-first (ascending last_scanned_at).
//
// This prevents cold-tail starvation when the per-cycle token budget truncates
// the run: files not covered in sweep N land in group 2 (or stay in group 1
// on the next sweep), and recently-scanned files move to the back of group 2.
//
// Targeted scans are always alphabetical; see [Funnel.Targeted].
func (f *Funnel) Sweep(ctx context.Context) (*Result, error) {
	snap, err := f.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	targets := make([]string, len(snap.Files))
	for i, file := range snap.Files {
		targets[i] = file.Path
	}

	var (
		heatOrdered      bool
		heatFiles        int
		neverScanned     int
		changedSinceScan int
	)

	// Fingerprints are needed for ordering (hash-changed detection), for
	// recording truthful coverage hashes after the run, AND for finding
	// anchoring in run(). We call Fingerprints here; run() calls it again for
	// anchoring. The duplication is an accepted trade-off: Fingerprints is
	// content-hashing (cheap relative to LLM calls) and the call sites serve
	// different purposes.
	fps, fpsErr := f.repo.Fingerprints(ctx, snap)

	if !f.opts.DisableHeatOrdering {
		heat, heatErr := ingest.ChurnHeat(ctx, f.repo.Root(), 0)

		// watermarks is a best-effort read; fall back to pure heat if it fails.
		var watermarks map[string]store.Watermark
		if fpsErr == nil {
			paths := make([]string, 0, len(fps))
			for p := range fps {
				paths = append(paths, p)
			}
			watermarks, _ = f.store.ScanWatermarks(ctx, paths)
		}

		if heatErr == nil && len(heat) > 0 {
			heatFiles = len(heat)
		}

		if fpsErr == nil && watermarks != nil {
			var heatReordered bool
			neverScanned, changedSinceScan, heatReordered = applySweepOrder(targets, heat, fps, watermarks)
			heatOrdered = heatReordered
			if heatReordered {
				progress.Emit(f.opts.Progress, progress.Event{
					Kind:  progress.KindHeatOrdered,
					Count: heatFiles,
					Label: heatTop5(targets, heat),
				})
			}
		} else if heatErr == nil && len(heat) > 0 {
			// Fall back: no store data, use pure heat ordering.
			heatOrdered = applyHeatOrder(targets, heat)
			if heatOrdered {
				progress.Emit(f.opts.Progress, progress.Event{
					Kind:  progress.KindHeatOrdered,
					Count: heatFiles,
					Label: heatTop5(targets, heat),
				})
			}
		}
	}

	// Emit the sweep summary BEFORE the scan starts so renderers can show
	// context about the upcoming run (how many files are new vs stale).
	progress.Emit(f.opts.Progress, progress.Event{
		Kind:    progress.KindSweepSummary,
		Count:   len(targets),
		Message: fmt.Sprintf("sweep: %d targets, %d never-scanned, %d changed-since-scan", len(targets), neverScanned, changedSinceScan),
	})

	// touchCoverage=true: sweeps stamp per-unit coverage as each finderOK unit
	// completes (incremental durability). Targeted scans do NOT touch coverage —
	// sweeps are the coverage source of truth. See the Deliberate Asymmetry note
	// in the hypothesize docstring and the design comment in run().
	result, err := f.run(ctx, store.ScanOneshot, snap, targets, fps, true)
	if err != nil {
		return nil, err
	}
	result.Stats.HeatOrdered = heatOrdered
	result.Stats.HeatFiles = heatFiles
	result.Stats.SweepNeverScanned = neverScanned
	result.Stats.SweepChangedSinceScan = changedSinceScan

	return result, nil
}

// applySweepOrder reorders targets in-place using the anti-starvation two-group
// scheme:
//
//   - Group 1 (needs-scan): files absent from watermarks, files whose
//     timestamp equals the epoch sentinel (never actually scanned), and files
//     whose current fingerprint differs from the stored content hash (changed
//     since their last scan). Group 1 is heat-ordered within the group, so a
//     fresh commit's churned files still lead the sweep.
//   - Group 2 (clean): all other files (previously scanned, content
//     unchanged). Sorted by last_scanned_at ascending (stalest first) so the
//     run always picks up the files that were scanned longest ago.
//
// Group 1 precedes Group 2 in the output. Convergence property: a
// budget-truncated sweep covers group 1 plus the head of group 2; covered
// files get a fresh last_scanned_at and rotate to the back of group 2 next
// sweep, so repeated truncated sweeps over an unchanged repo rotate through
// the full set instead of fixating on a hot head.
//
// Returns (neverScanned, changedSinceScan, heatActuallyReordered):
//   - neverScanned: count of group-1 files with no row / epoch timestamp.
//   - changedSinceScan: count of group-1 files admitted by the hash mismatch
//     (scanned before, content changed since).
//   - heatActuallyReordered: true if the heat map produced a non-trivial
//     reordering within group 1.
func applySweepOrder(targets []string, heat map[string]float64, fps map[string]string, watermarks map[string]store.Watermark) (neverScanned, changedSinceScan int, heatActuallyReordered bool) {
	var group1, group2 []string
	for _, t := range targets {
		wm, ok := watermarks[t]
		switch {
		case !ok || wm.LastScannedAt.Equal(epochSentinelParsed):
			neverScanned++
			group1 = append(group1, t)
		case fps[t] != wm.ContentHash:
			changedSinceScan++
			group1 = append(group1, t)
		default:
			group2 = append(group2, t)
		}
	}

	// Group 1: heat-ordered (highest heat first; alphabetical tiebreak).
	g1Before := make([]string, len(group1))
	copy(g1Before, group1)
	sort.SliceStable(group1, func(i, j int) bool {
		hi, hj := heat[group1[i]], heat[group1[j]]
		if hi != hj {
			return hi > hj
		}
		return group1[i] < group1[j]
	})
	for i := range group1 {
		if group1[i] != g1Before[i] {
			heatActuallyReordered = true
			break
		}
	}

	// Group 2: stalest first (ascending last_scanned_at).
	sort.SliceStable(group2, func(i, j int) bool {
		ti, tj := watermarks[group2[i]].LastScannedAt, watermarks[group2[j]].LastScannedAt
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return group2[i] < group2[j]
	})

	// Write the two groups back into targets in-place.
	copy(targets, group1)
	copy(targets[len(group1):], group2)

	return neverScanned, changedSinceScan, heatActuallyReordered
}

// applyHeatOrder sorts targets in-place by heat score descending, with
// equal-heat (including zero-heat) files sorted alphabetically as a tiebreak.
// It returns true if the ordering differs from the input (meaning the heat map
// actually reordered something), so callers can decide whether to log.
func applyHeatOrder(targets []string, heat map[string]float64) bool {
	// Snapshot the original order to detect actual reordering.
	original := make([]string, len(targets))
	copy(original, targets)

	sort.SliceStable(targets, func(i, j int) bool {
		hi, hj := heat[targets[i]], heat[targets[j]]
		if hi != hj {
			return hi > hj // higher heat first
		}
		return targets[i] < targets[j] // alphabetical tiebreak
	})

	for i := range targets {
		if targets[i] != original[i] {
			return true
		}
	}
	return false
}

// heatTop5 returns a human-readable summary of the top 5 hottest targets,
// formatted as "path:score" pairs joined by spaces, for use in progress events.
func heatTop5(targets []string, heat map[string]float64) string {
	n := 5
	if len(targets) < n {
		n = len(targets)
	}
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(' ')
		}
		p := targets[i]
		fmt.Fprintf(&b, "%s:%.3f", p, heat[p])
	}
	return b.String()
}

// Targeted runs the funnel over the blast radius of changedFiles, intersected
// with the current snapshot. It is the commit-triggered entrypoint: only files
// that are in scope (tracked, text, not excluded) are scanned, but the blast
// radius pulls in their direct dependents so a change's ripple is covered.
func (f *Funnel) Targeted(ctx context.Context, changedFiles []string) (*Result, error) {
	snap, err := f.snapshot(ctx)
	if err != nil {
		return nil, err
	}

	radius, err := f.repo.BlastRadius(ctx, snap, changedFiles)
	if err != nil {
		return nil, fmt.Errorf("funnel: blast radius: %w", err)
	}

	// Intersect the radius with the snapshot: BlastRadius may surface paths that
	// are not in our in-scope file set (e.g. excluded by the scan filter), and we
	// only audit files we actually have in the snapshot.
	inScope := make(map[string]bool, len(snap.Files))
	for _, file := range snap.Files {
		inScope[file.Path] = true
	}
	var targets []string
	for _, p := range radius {
		if inScope[p] {
			targets = append(targets, p)
		}
	}
	sort.Strings(targets)

	// touchCoverage=false: targeted scans do not stamp coverage. See Sweep.
	return f.run(ctx, store.ScanTargeted, snap, targets, nil, false)
}

// snapshot builds the current snapshot through the configured scan filter
// (Options.Filter, mapped from config.Scan by the CLI/daemon). Found the hard
// way: this used to pass an empty filter, so include/exclude globs were
// silently ignored and a "scoped" calibration scan swept the whole repo.
func (f *Funnel) snapshot(ctx context.Context) (*ingest.Snapshot, error) {
	snap, err := f.repo.Snapshot(ctx, f.opts.Filter)
	if err != nil {
		return nil, fmt.Errorf("funnel: snapshot: %w", err)
	}
	return snap, nil
}

// run is the shared staged core. It opens a scan run, wires per-role spend
// recording into the clients, executes stages A-D, finalizes the scan run with
// stats, and returns the ranked Result. targets is the (already scoped) list of
// repo-relative files to audit.
//
// fps is the per-file fingerprint map computed by the caller (Sweep already
// has it; Targeted may pass nil since Targeted does not touch coverage).
// touchCoverage enables per-unit coverage stamping (true for sweeps, false for
// targeted). When true, the hypothesize goroutines call TouchScanCoverage for
// each finderOK unit immediately on completion so coverage is durable across
// interruptions. The old run-end batch call is gone: coverage is now incremental.
//
// # Interrupt-safe finalization
//
// run() seals the scan_runs row (FinishScanRun) on EVERY exit path — normal
// completion, internal error, or context cancellation — using a deferred
// finalize step. This prevents dangling never-finalized rows when a scan is
// killed or cancelled.
//
// The finalize write uses a short detached context (context.WithTimeout over
// context.Background()) rather than the run's ctx, because the run ctx is
// already cancelled on the interruption path. The run ctx must not be used for
// the finalize write, or the DB call would fail immediately.
func (f *Funnel) run(ctx context.Context, kind store.ScanKind, snap *ingest.Snapshot, targets []string, fps map[string]string, touchCoverage bool) (*Result, error) {
	scanRunID, err := f.store.BeginScanRun(ctx, kind, snap.Commit)
	if err != nil {
		return nil, fmt.Errorf("funnel: begin scan run: %w", err)
	}

	sink := f.opts.Progress
	progress.Emit(sink, progress.Event{
		Kind: progress.KindScanStarted, ScanKind: string(kind), Commit: snap.Commit,
	})

	// Per-run spend recorder, wired into both role clients so every completion is
	// ledgered to this scan run and counted toward the budget. The onRecord hook
	// emits a cumulative spend tick so live renderers can show a running total.
	rec := &spendRecorder{ctx: ctx, store: f.store, scanRunID: scanRunID}
	if sink != nil {
		rec.onRecord = func(in, out, cached int64) {
			progress.Emit(sink, progress.Event{
				Kind: progress.KindSpendTick, InputTokens: in, OutputTokens: out,
				CacheReadTokens: cached,
			})
		}
	}
	finder := llm.WithRecorder(f.clients.Finder, rec, "finder", "", "")
	verifier := llm.WithRecorder(f.clients.Verifier, rec, "verifier", "", "")

	cacheWeight := f.opts.CacheReadBudgetWeight
	if cacheWeight == 0 {
		cacheWeight = DefaultCacheReadBudgetWeight
	}
	budget := newBudgetState(f.opts.TokenBudget, rec, cacheWeight)

	result := &Result{ScanRunID: scanRunID, Commit: snap.Commit}

	// Interrupt-safe finalization: seal the scan_runs row on every exit path.
	// finalizeOnce ensures we finalize exactly once even if the deferred call
	// races a normal-path finalize (there is only one finalize site now, but
	// the once guard is cheap insurance).
	//
	// The detached context (5 s timeout over context.Background()) is critical:
	// on the cancellation path the run ctx is already dead, so any DB write on
	// it would fail immediately and leave the row dangling. We need a fresh
	// context that is still alive to write the seal. 5 s is generous for a
	// single SQLite UPDATE.
	var finalizeOnce = func(s *Stats) {
		statsJSON, merr := json.Marshal(s)
		if merr != nil {
			// JSON marshal of our own struct should never fail; log-and-continue.
			statsJSON = []byte(`{"aborted":true}`)
		}
		fCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if ferr := f.store.FinishScanRun(fCtx, scanRunID, string(statsJSON)); ferr != nil {
			// Best-effort: a finalize failure is logged via note on the result (if
			// available) but must not mask the original error or panic.
			f.note(result, fmt.Sprintf("funnel: FinishScanRun failed on finalize: %v", ferr))
		}
	}
	finalized := false
	defer func() {
		if !finalized {
			// Abnormal exit (panic recovery is not our job; this handles error/cancel).
			finalizeOnce(&result.Stats)
		}
	}()

	// Derive the finder/verifier persona from the snapshot's dominant language
	// mix so a non-Go repo is audited by an appropriately-described engineer
	// rather than a hardcoded "senior Go engineer". Computed once per run and
	// threaded into the per-unit prompt construction in hypothesize/verify.
	persona := ingest.Persona(snap)
	// The dominant-language mix also drives the per-run lens priority (effective
	// yields are per-language; see lensYields) and therefore which lenses budget
	// degradation sheds on this repo.
	langs := ingest.DominantLanguages(snap)

	// Fingerprints anchor every persisted finding to the exact file content it
	// was found in, so the daemon can later detect when the code changed.
	// NOTE: run() no longer calls Fingerprints here; the caller (Sweep) already
	// computed fps and passes it in. Targeted passes nil (it does not need
	// fingerprints for coverage). persist/persistSuspected still need fps for
	// finding anchoring, so we compute them from snap here when nil (targeted path).
	if fps == nil {
		var fpsErr error
		fps, fpsErr = f.repo.Fingerprints(ctx, snap)
		if fpsErr != nil {
			result.Stats.Aborted = true
			return nil, fmt.Errorf("funnel: fingerprints: %w", fpsErr)
		}
	}

	// Stage A — Hypothesize.
	progress.Emit(sink, progress.Event{Kind: progress.KindStageStarted, Stage: progress.StageHypothesize})
	candidates, err := f.hypothesize(ctx, scanRunID, finder, persona, kind, f.opts.ChangeContext, langs, targets, budget, result, fps, touchCoverage)
	if err != nil {
		if ctx.Err() != nil {
			result.Stats.Interrupted = true
			return nil, ctx.Err()
		}
		result.Stats.Aborted = true
		return nil, err
	}
	result.Stats.Hypothesized = len(candidates)
	progress.Emit(sink, progress.Event{
		Kind: progress.KindStageFinished, Stage: progress.StageHypothesize,
		Counts: &progress.Counts{
			Hypothesized:   len(candidates),
			FinderFailures: result.Stats.FinderFailures,
		},
	})

	// Stage B — Triage.
	progress.Emit(sink, progress.Event{Kind: progress.KindStageStarted, Stage: progress.StageTriage})
	survivors, err := f.triage(ctx, candidates, snap, &result.Stats)
	if err != nil {
		if ctx.Err() != nil {
			result.Stats.Interrupted = true
			return nil, ctx.Err()
		}
		result.Stats.Aborted = true
		return nil, err
	}
	result.Stats.Triaged = len(survivors)
	progress.Emit(sink, progress.Event{
		Kind: progress.KindStageFinished, Stage: progress.StageTriage,
		Counts: &progress.Counts{Hypothesized: len(candidates), Triaged: len(survivors)},
	})

	// Stage C — Verify.
	progress.Emit(sink, progress.Event{Kind: progress.KindStageStarted, Stage: progress.StageVerify})
	verified, killed, orphaned, err := f.verify(ctx, verifier, persona, survivors, budget, result)
	if err != nil {
		if ctx.Err() != nil {
			result.Stats.Interrupted = true
			return nil, ctx.Err()
		}
		result.Stats.Aborted = true
		return nil, err
	}
	result.Stats.Verified = len(verified)
	result.Stats.Killed = killed
	progress.Emit(sink, progress.Event{
		Kind: progress.KindStageFinished, Stage: progress.StageVerify,
		Counts: &progress.Counts{
			Hypothesized: len(candidates), Triaged: len(survivors),
			Verified: len(verified), Killed: killed,
		},
	})

	// Stage D — Persist + rank.
	progress.Emit(sink, progress.Event{Kind: progress.KindStageStarted, Stage: progress.StagePersist})
	findings, err := f.persist(ctx, verified, snap.Commit, fps)
	if err != nil {
		if ctx.Err() != nil {
			result.Stats.Interrupted = true
			return nil, ctx.Err()
		}
		result.Stats.Aborted = true
		return nil, err
	}
	// Budget-orphaned candidates (verification skipped or cut short at the hard
	// stop) persist as Tier 3 suspected so they are not silently dropped: a human
	// can review them and re-run verification with more budget.
	suspected, err := f.persistSuspected(ctx, orphaned, snap.Commit, fps)
	if err != nil {
		if ctx.Err() != nil {
			result.Stats.Interrupted = true
			return nil, ctx.Err()
		}
		result.Stats.Aborted = true
		return nil, err
	}
	findings = append(findings, suspected...)
	result.Stats.Suspected = len(suspected)
	sortFindings(findings)
	result.Findings = findings
	progress.Emit(sink, progress.Event{Kind: progress.KindStageFinished, Stage: progress.StagePersist})

	result.Degraded = budget.degraded.Load()
	result.Stopped = budget.stopped.Load()
	in, out, cacheRead, cacheCreated := rec.totals()
	result.Stats.InputTokens = in
	result.Stats.OutputTokens = out
	result.Stats.CacheReadTokens = cacheRead
	result.Stats.CacheCreationTokens = cacheCreated

	progress.Emit(sink, progress.Event{
		Kind: progress.KindScanFinished, ScanKind: string(kind), Commit: snap.Commit,
		Counts: &progress.Counts{
			Hypothesized: result.Stats.Hypothesized, Triaged: result.Stats.Triaged,
			Verified: result.Stats.Verified, Killed: result.Stats.Killed,
			FinderFailures: result.Stats.FinderFailures,
		},
		InputTokens: in, OutputTokens: out,
		CacheReadTokens: cacheRead, CacheCreationTokens: cacheCreated,
	})

	// Normal-path finalize: seal the scan run row. Mark finalized so the deferred
	// finalize step does not double-call (which would be harmless but wasteful).
	finalized = true
	finalizeOnce(&result.Stats)

	// Prune agent_unit rows for old scan runs. Best-effort: a prune failure is
	// never fatal to the scan result. keepRuns is defined in observability.go.
	// Skipped on the interrupted/aborted paths (handled above via early return)
	// because those paths use a dead ctx and PruneAgentUnits would fail
	// immediately; the prune is cosmetic and will succeed on the next clean run.
	if _, err := f.store.PruneAgentUnits(ctx, keepRuns); err != nil {
		f.note(result, fmt.Sprintf("observability: PruneAgentUnits failed: %v", err))
	}

	return result, nil
}
