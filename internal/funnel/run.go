package funnel

import (
	"context"
	"fmt"
	"sort"

	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/store"
)

// epochSentinelParsed is the parsed form of the epoch sentinel written by
// RefreshContentHashes for never-scanned rows. It is package-level so
// applySweepOrder (and tests in coverage_test.go) can reference it without
// importing the store package's internal epoch constant.
var epochSentinelParsed = store.EpochSentinelTime()

// runMode controls what the shared run() pipeline produces.
type runMode int

const (
	// modeFull is the normal sweep/targeted mode: hypothesize runs the
	// finder/cartographer to produce fresh candidates in addition to replaying
	// any pending WAL candidates.
	modeFull runMode = iota
	// modeVerifyDrain skips the finder/cartographer stages entirely. candCh
	// carries only the replayed WAL candidates from ListPendingCandidates.
	// This is the mode used by VerifyDrain.
	modeVerifyDrain
	// modeReverify skips the finder/cartographer like modeVerifyDrain, but
	// replays candidates reconstructed from open Tier-3 suspected findings
	// (ReverifySuspected) instead of pending_candidates.
	modeReverify
	// modeRevalidate skips the finder/cartographer like modeReverify, but
	// replays candidates reconstructed from open Tier-2 findings whose
	// genuine-verdict count is below MinReviewerValidation
	// (ReverifyUnderValidated) — survivors of budget-degraded panels that
	// too few reviewer seats actually judged.
	modeRevalidate
)

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

	// Order the targets exactly as Funnel.EstimateScan does, via the shared
	// orderSweepTargets helper. The finder-unit count depends on the
	// post-ordering chunk packing (chunkByLanguage packs per-language tails into
	// mixed chunks in input order), so the real run and `scan --estimate` MUST
	// order identically or the estimate would drift on polyglot repos.
	//
	// Fingerprints feed both the ordering (hash-changed detection) and run()'s
	// finding anchoring; run() calls Fingerprints again. The duplication is an
	// accepted trade-off (content hashing is cheap relative to LLM calls).
	fps, fpsErr := f.repo.Fingerprints(ctx, snap)
	heat, heatFiles, neverScanned, changedSinceScan, heatOrdered := f.orderSweepTargets(ctx, targets, fps, fpsErr)
	if heatOrdered {
		progress.Emit(f.opts.Progress, progress.Event{
			Kind:  progress.KindHeatOrdered,
			Count: heatFiles,
			Label: heatTop5(targets, heat),
		})
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
	result, err := f.run(ctx, store.ScanOneshot, snap, targets, fps, true, modeFull)
	if err != nil {
		return nil, err
	}
	result.Stats.HeatOrdered = heatOrdered
	result.Stats.HeatFiles = heatFiles
	result.Stats.SweepNeverScanned = neverScanned
	result.Stats.SweepChangedSinceScan = changedSinceScan

	return result, nil
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
	return f.run(ctx, store.ScanTargeted, snap, targets, nil, false, modeFull)
}

// snapshot builds the current snapshot through the configured scan filter
// (Options.Filter, mapped from config.Scan by the CLI/daemon). Found the hard
// way: this used to pass an empty filter, so include/exclude globs were
// silently ignored and a "scoped" calibration scan swept the whole repo.
func (f *Funnel) snapshot(ctx context.Context) (*ingest.Snapshot, error) {
	snap, err := f.repo.Snapshot(ctx, f.opts.Discovery.Filter)
	if err != nil {
		return nil, fmt.Errorf("funnel: snapshot: %w", err)
	}
	return snap, nil
}
