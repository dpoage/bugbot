package funnel

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/store"
)

// Sweep runs the funnel over the entire current snapshot of the repository. It
// is the manual `bugbot scan` and periodic-sweep entrypoint.
func (f *Funnel) Sweep(ctx context.Context) (*Result, error) {
	snap, err := f.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	targets := make([]string, len(snap.Files))
	for i, file := range snap.Files {
		targets[i] = file.Path
	}
	return f.run(ctx, store.ScanOneshot, snap, targets)
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

	return f.run(ctx, store.ScanTargeted, snap, targets)
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
func (f *Funnel) run(ctx context.Context, kind store.ScanKind, snap *ingest.Snapshot, targets []string) (*Result, error) {
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

	budget := newBudgetState(f.opts.TokenBudget, rec)

	result := &Result{ScanRunID: scanRunID, Commit: snap.Commit}

	// Fingerprints anchor every persisted finding to the exact file content it
	// was found in, so the daemon can later detect when the code changed.
	fps, err := f.repo.Fingerprints(ctx, snap)
	if err != nil {
		return nil, fmt.Errorf("funnel: fingerprints: %w", err)
	}

	// Stage A — Hypothesize.
	progress.Emit(sink, progress.Event{Kind: progress.KindStageStarted, Stage: progress.StageHypothesize})
	candidates, err := f.hypothesize(ctx, scanRunID, finder, targets, budget, result)
	if err != nil {
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
		return nil, err
	}
	result.Stats.Triaged = len(survivors)
	progress.Emit(sink, progress.Event{
		Kind: progress.KindStageFinished, Stage: progress.StageTriage,
		Counts: &progress.Counts{Hypothesized: len(candidates), Triaged: len(survivors)},
	})

	// Stage C — Verify.
	progress.Emit(sink, progress.Event{Kind: progress.KindStageStarted, Stage: progress.StageVerify})
	verified, killed, orphaned, err := f.verify(ctx, verifier, survivors, budget, result)
	if err != nil {
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
		return nil, err
	}
	// Budget-orphaned candidates (verification skipped or cut short at the hard
	// stop) persist as Tier 3 suspected so they are not silently dropped: a human
	// can review them and re-run verification with more budget.
	suspected, err := f.persistSuspected(ctx, orphaned, snap.Commit, fps)
	if err != nil {
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

	// Finalize the scan run with the stats blob.
	statsJSON, err := json.Marshal(result.Stats)
	if err != nil {
		return nil, fmt.Errorf("funnel: marshal stats: %w", err)
	}
	if err := f.store.FinishScanRun(ctx, scanRunID, string(statsJSON)); err != nil {
		return nil, fmt.Errorf("funnel: finish scan run: %w", err)
	}

	return result, nil
}
