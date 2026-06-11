package funnel

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/store"
)

// Sweep runs the funnel over the entire current snapshot of the repository. It
// is the manual `bugbot scan` and periodic-sweep entrypoint.
//
// When heat ordering is enabled (the default, controlled by
// Options.DisableHeatOrdering), targets are sorted by churn-weighted recency
// heat before chunking so finder budget flows to recently-churned files first.
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
		heatOrdered bool
		heatFiles   int
	)
	if !f.opts.DisableHeatOrdering {
		heat, heatErr := ingest.ChurnHeat(ctx, f.repo.Root(), 0)
		if heatErr == nil && len(heat) > 0 {
			heatFiles = len(heat)
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

	result, err := f.run(ctx, store.ScanOneshot, snap, targets)
	if err != nil {
		return nil, err
	}
	result.Stats.HeatOrdered = heatOrdered
	result.Stats.HeatFiles = heatFiles
	return result, nil
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

	cacheWeight := f.opts.CacheReadBudgetWeight
	if cacheWeight == 0 {
		cacheWeight = DefaultCacheReadBudgetWeight
	}
	budget := newBudgetState(f.opts.TokenBudget, rec, cacheWeight)

	result := &Result{ScanRunID: scanRunID, Commit: snap.Commit}

	// Derive the finder/verifier persona from the snapshot's dominant language
	// mix so a non-Go repo is audited by an appropriately-described engineer
	// rather than a hardcoded "senior Go engineer". Computed once per run and
	// threaded into the per-unit prompt construction in hypothesize/verify.
	persona := ingest.Persona(snap)

	// Fingerprints anchor every persisted finding to the exact file content it
	// was found in, so the daemon can later detect when the code changed.
	fps, err := f.repo.Fingerprints(ctx, snap)
	if err != nil {
		return nil, fmt.Errorf("funnel: fingerprints: %w", err)
	}

	// Stage A — Hypothesize.
	progress.Emit(sink, progress.Event{Kind: progress.KindStageStarted, Stage: progress.StageHypothesize})
	candidates, err := f.hypothesize(ctx, scanRunID, finder, persona, targets, budget, result)
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
	verified, killed, orphaned, err := f.verify(ctx, verifier, persona, survivors, budget, result)
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
