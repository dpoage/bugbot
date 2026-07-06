package funnel

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dpoage/bugbot/internal/domain"
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

// orderSweepTargets applies the Sweep anti-starvation ordering to targets IN
// PLACE (heat-ordered when enabled and data is available; unchanged otherwise)
// and returns the bookkeeping the caller needs for its progress summary. It is
// shared by Funnel.Sweep and Funnel.EstimateScan so both order identically: the
// finder-unit count depends on the post-ordering chunk packing, so any
// divergence would make the estimate drift from the real run on polyglot repos.
func (f *Funnel) orderSweepTargets(ctx context.Context, targets []string, fps map[string]string, fpsErr error) (heat map[string]float64, heatFiles, neverScanned, changedSinceScan int, heatOrdered bool) {
	if f.opts.Features.DisableHeatOrdering {
		return nil, 0, 0, 0, false
	}
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
		neverScanned, changedSinceScan, heatOrdered = applySweepOrder(targets, heat, fps, watermarks)
	} else if heatErr == nil && len(heat) > 0 {
		heatOrdered = applyHeatOrder(targets, heat)
	}
	return heat, heatFiles, neverScanned, changedSinceScan, heatOrdered
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

// run is the shared staged core. It opens a scan run, wires per-role spend
// recording into the clients, runs the streaming pipeline, finalizes the scan
// run with stats, and returns the ranked Result. targets is the (already
// scoped) list of repo-relative files to audit.
//
// fps is the per-file fingerprint map computed by the caller (Sweep already
// has it; Targeted may pass nil since Targeted does not touch coverage).
// touchCoverage enables per-unit coverage stamping (true for sweeps, false for
// targeted). When true, the hypothesize goroutines call TouchScanCoverage for
// each finderOK unit immediately on completion so coverage is durable across
// interruptions.
//
// # Streaming Topology
//
// Rather than hard barriers (every finder → batch triage → batch verify →
// batch persist), candidates now flow through a live pipeline:
//
//  1. hypothesize emits candidates one at a time via candCh as each unit
//     completes. Hypothesize blocks until all units finish; run() closes
//     candCh after hypothesize returns.
//  2. A single triage consumer goroutine receives from candCh, applies
//     steps 1-4 (confidence/scope/fingerprint/suppression) per candidate,
//     and performs INCREMENTAL CLUSTERING: the first member of a cluster
//     becomes the primary and is forwarded to verify immediately; later
//     members are staged as corroboration. Triage drains candCh and closes
//     verCh when done.
//  3. A verify dispatcher spawns one goroutine per forwarded primary. Each
//     goroutine acquires a HIGH-priority slot, runs the refuter panel +
//     arbiter (extracted into verifyCandidateBody), and immediately persists
//     survivors (and orphans). Results are collected in a findings slice.
//  4. persist happens immediately on panel completion inside the verify goroutine.
//
// Staged corroboration: when a later cluster member arrives AFTER its primary
// has already been persisted, AddCorroboratingLenses updates the stored finding.
//
// # Interrupt-safe finalization
//
// run() seals the scan_runs row (FinishScanRun) on EVERY exit path — normal
// completion, internal error, or context cancellation — using a deferred
// finalize step. ALREADY-PERSISTED findings survive interruption (durable).
//
// The finalize write uses a short detached context (context.WithTimeout over
// context.Background()) rather than the run's ctx, because the run ctx is
// already cancelled on the interruption path.
func (f *Funnel) run(ctx context.Context, kind store.ScanKind, snap *ingest.Snapshot, targets []string, fps map[string]string, touchCoverage bool, mode runMode) (*Result, error) {
	scanRunID, err := f.store.BeginScanRun(ctx, kind, snap.Commit)
	if err != nil {
		return nil, fmt.Errorf("funnel: begin scan run: %w", err)
	}

	sink := f.opts.Progress
	progress.Emit(sink, progress.Event{
		Kind: progress.KindScanStarted, ScanKind: string(kind), Commit: snap.Commit,
	})

	// Per-run spend recorder, wired into both role clients so every completion is
	// ledgered to this scan run and counted toward the budget.
	rec := &spendRecorder{ctx: ctx, store: f.store, scanRunID: scanRunID}
	if sink != nil {
		rec.onRecord = func(in, out, cached int64) {
			progress.Emit(sink, progress.Event{
				Kind: progress.KindSpendTick, InputTokens: in, OutputTokens: out,
				CacheReadTokens: cached,
			})
		}
	}
	finderClient := llm.WithRecorder(f.clients.Finder, rec, roleFinder, "", "")
	verifierClient := llm.WithRecorder(f.clients.Verifier, rec, roleVerifier, "", "")
	// Cartographer client: configurable via the optional [roles.cartographer]
	// mapping (falls back to the finder's model when unset; see config.roleModel).
	// Tagged roleCartographer so its spend is a distinct ledger line yet still
	// charged to the finder pool (see spendRecorder.Record).
	cartographerBase := f.clients.Cartographer
	if cartographerBase == nil {
		cartographerBase = f.clients.Finder
	}
	cartographerClient := llm.WithRecorder(cartographerBase, rec, roleCartographer, "", "")
	// Arbiter client: configurable via the optional [roles.arbiter] mapping
	// (falls back to the verifier's client when unset — preserve today's
	// behavior where the split-verdict arbiter reuses the verifier provider).
	// Tagged roleArbiter so its spend is its own ledger line; the recorder's
	// default branch in Record routes anything-not-finder-or-cartographer (i.e.
	// arbiter) to the VERIFY sub-pool — which is correct because the arbiter is
	// a verify-stage agent that draws from the verify pool on split verdicts.
	arbiterBase := f.clients.Arbiter
	if arbiterBase == nil {
		arbiterBase = f.clients.Verifier
	}
	arbiterClient := llm.WithRecorder(arbiterBase, rec, roleArbiter, "", "")

	// Capability-driven scaling: a small-context model (e.g. 8k local LLM
	// behind an openai-compatible endpoint) silently overflows with one-size
	// finder defaults. Scale chunk size, per-read caps, and the (opt-in)
	// history-compaction threshold from the finder's declared context
	// window. f.opts holds the resolved copy built in New(); the helper
	// preserves explicit non-default Options values and is a strict no-op
	// for unknown or large windows, so all existing behavior is stable.
	if window := f.clients.Finder.Capabilities().ContextWindow; window > 0 {
		f.opts = scaleFinderForContext(f.opts, window)
	}
	cacheWeight := f.opts.Budget.CacheReadBudgetWeight
	budget := newBudgetState(f.opts.Budget.TokenBudget, rec, cacheWeight)
	// Per-task token claims: each finder / verifier agent run is capped at its
	// role's claim so one breadth-heavy run cannot be granted a whole stage's
	// reserve at launch; the unspent remainder of a claim stays in the pool for
	// sibling runs (the claimant system, bugbot-8mj).
	budget.finderClaim = f.opts.Budget.FinderTokenClaim
	budget.verifyClaim = f.opts.Budget.VerifierTokenClaim
	budget.arbiterClaim = f.opts.Budget.ArbiterTokenClaim
	// Reserve a slice of the per-run budget for downstream verification so the
	// breadth-heavy finder stage cannot drain the whole pool and orphan every
	// candidate before it is verified (bugbot-8mj / bugbot-3lt). A no-op when the
	// budget is unlimited or the share disables the reservation.
	// Reserve a slice of the per-run budget for downstream verification so the
	// breadth-heavy finder stage cannot drain the whole pool and orphan every
	// candidate before it is verified (bugbot-8mj / bugbot-3lt). A no-op when the
	// budget is unlimited or the share disables the reservation.
	// In modeVerifyDrain the finder stage never runs, so skip the reservation:
	// without this guard verifyPool is capped at 30% of TokenBudget, orphaning
	// every candidate as T3 while 70% sits idle in the never-charged finder pool.
	if mode == modeFull {
		budget.reserveForDownstream(f.opts.Budget.FinderBudgetShare)
	}

	result := &Result{ScanRunID: scanRunID, Commit: snap.Commit}
	// Persist whether the cartographer pass was active, on every exit path
	// (set before the finalize defer below), so the valid-findings-per-token
	// time series can be sliced by on/off.
	result.Stats.CartographerEnabled = f.opts.Features.Cartographer

	// Interrupt-safe finalization: seal the scan_runs row on every exit path.
	var finalize = func(s *Stats) error {
		statsJSON, merr := json.Marshal(s)
		if merr != nil {
			statsJSON = []byte(`{"aborted":true}`)
		}
		fCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if ferr := f.store.FinishScanRun(fCtx, scanRunID, string(statsJSON)); ferr != nil {
			f.note(result, fmt.Sprintf("funnel: FinishScanRun failed on finalize: %v", ferr))
			return ferr
		}
		return nil
	}
	finalized := false
	defer func() {
		if !finalized {
			_ = finalize(&result.Stats)
		}
	}()

	persona := ingest.Persona(snap)
	langs := ingest.DominantLanguages(snap)
	f.hasGoDepSource = f.goDepSourceFor(langs)

	if fps == nil {
		var fpsErr error
		fps, fpsErr = f.repo.Fingerprints(ctx, snap)
		if fpsErr != nil {
			result.Stats.Aborted = true
			return nil, fmt.Errorf("funnel: fingerprints: %w", fpsErr)
		}
	}

	// candCh is the channel from hypothesize → triage. A buffer of 64 lets
	// finder units emit without blocking on a slow triage consumer, while
	// bounding memory to ~64 Candidates.
	candCh := make(chan Candidate, 64)

	// verCh is the channel from triage → verify dispatcher.
	verCh := make(chan Candidate, 64)

	// reproQ.ch is the channel from verify goroutines → repro consumer goroutines.
	// Buffered to 16 so a single slow repro attempt does not stall verification.
	// Non-blocking enqueue: verify goroutines MUST NOT block on the repro queue (that
	// would let a slow repro backlog stall verification). When the buffer is full,
	// the finding is appended to overflowFindings (under overflowMu) for a
	// catch-up pass after the queue is drained. The claim mechanism (ReproPath /
	// NeedsHuman check before invoking the hook) ensures no finding is attempted
	// twice even if it appears in both the channel and the catch-up slice.
	//
	// The repro consumer goroutine (launched below) reads from reproQ.ch and
	// spawns one worker per finding. It runs CONCURRENTLY with verify (and
	// hypothesize): a finding verified early in a long scan gets its repro
	// attempt while discovery continues. The consumer exits when reproQ.ch is
	// closed (after verifyWg.Wait()); reproWg tracks all spawned workers.
	var reproQ *reproQueue
	var reproWg sync.WaitGroup
	// reproConsumerDone is closed when the consumer goroutine (not the workers)
	// finishes spawning all goroutines, so we can join the workers via reproWg.
	var reproConsumerDone chan struct{}
	if f.opts.Repro != nil {
		reproQ = newReproQueue(16)
		reproConsumerDone = make(chan struct{})
	}

	// clusterReg is shared between triage (which registers primaries and stages
	// corroborating lenses) and verify goroutines (which drain staged lenses and
	// signal persistence).
	ts, clusterReg := newTriageState(snap)

	// findingsMu protects allFindings, verifyKilled, and the verifier stats
	// fields on result.Stats that the concurrent verify goroutines update.
	var (
		findingsMu   sync.Mutex
		allFindings  []store.Finding
		verifyKilled int
		verifyErr    error
		verifyErrMu  sync.Mutex
	)
	setVerifyErr := func(e error) {
		verifyErrMu.Lock()
		if verifyErr == nil {
			verifyErr = e
		}
		verifyErrMu.Unlock()
	}

	// Shared sandbox counters across all verify goroutines.
	var sbExecs atomic.Int32
	var sbMillis atomic.Int64

	// triageErr captures a fatal triage-consumer error (store I/O / ctx cancel).
	var triageErr error

	// hypothesizeErr captures a fatal hypothesize error.
	var hypothesizeErr error

	// Resume: load the candidates to replay into THIS run's triage/verify
	// pipeline, before fresh hypothesize, so prior work is not lost. The source
	// depends on mode:
	//
	//   - modeVerifyDrain: ListPendingCandidates (the WAL of an interrupted run).
	//   - modeReverify:    ListFindings open Tier-3 suspected (durable findings
	//                      orphaned by a budget/verify-fail stop; the WAL row is
	//                      already gone, so we replay from the findings table).
	//   - modeFull:        ListPendingCandidates (same as VerifyDrain; the fresh
	//                      hypothesize stage ALSO runs and produces new candidates).
	//
	// Best-effort: a load failure degrades to "no resume" (the rows stay for a
	// later run) rather than aborting the scan.
	var replay []Candidate
	switch mode {
	case modeReverify:
		susp, listErr := f.store.ListFindings(ctx, store.FindingFilter{Status: store.StatusOpen, Tier: int(domain.TierSuspected)})
		if listErr != nil {
			f.note(result, fmt.Sprintf("resume: ListFindings(open T3) failed: %v", listErr))
			susp = nil
		}
		replay = make([]Candidate, 0, len(susp))
		for _, fi := range susp {
			replay = append(replay, findingToCandidate(fi))
		}
	default:
		pending, listErr := f.store.ListPendingCandidates(ctx)
		if listErr != nil {
			f.note(result, fmt.Sprintf("resume: ListPendingCandidates failed: %v", listErr))
			pending = nil
		}
		replay = make([]Candidate, 0, len(pending))
		for _, pc := range pending {
			replay = append(replay, pendingToCandidate(pc))
		}
	}

	// ---- Stage A: Hypothesize ----
	if mode == modeFull {
		progress.Emit(sink, progress.Event{Kind: progress.KindStageStarted, Stage: progress.StageHypothesize})
	}

	// Launch hypothesize in a goroutine so the triage consumer can run
	// concurrently. The emit callback sends to candCh.
	// candCh is closed exactly once from this goroutine's exit path.
	var hypothesizeWg sync.WaitGroup
	hypothesizeWg.Add(1)
	var hypothesizedCount int
	go func() {
		defer hypothesizeWg.Done()
		emit := func(c Candidate) {
			select {
			case candCh <- c:
			case <-ctx.Done():
				// Drop candidate on cancellation; triage will also exit.
			}
		}
		// Replay resumed candidates FIRST so the prior run's unfinished work is
		// verified before fresh discovery starts. WAL-replayed candidates carry
		// their original PendingID, so the terminal-fate handlers delete that
		// row; they are NOT re-inserted (the existing row IS their WAL entry).
		// Reverify-replayed candidates have no WAL row (PendingID==""); the
		// verify kill path detects Reverify==true and dismisses the durable
		// finding instead of trying to delete a missing WAL row (deletePending
		// is a no-op on ""). Triage re-anchors all replays to the current
		// snapshot (scope/dedup/suppression checks) and the verifier re-judges
		// them against current code, so a stale candidate is correctly dropped
		// or killed — precision is preserved.
		for _, c := range replay {
			emit(c)
		}
		result.Stats.Resumed = len(replay)
		// In modeVerifyDrain / modeReverify the finder/cartographer are skipped:
		// candCh carries only the replayed candidates. Everything downstream is
		// identical.
		if mode == modeFull {
			// Enumerate cross-language seams once per run: the boundary lens's
			// unit of work is one seam, and the count populates Stats.SeamsFound
			// for observability. Computation is O(files) over the snapshot and
			// runs in the hypothesize goroutine so the per-stage progression
			// reports (which are emitted before this call) are unaffected.
			seams := ingest.EnumerateSeams(snap)
			result.Stats.SeamsFound = len(seams)
			// Cartographer pass: one-shot, runs once per scan BEFORE the
			// finder stage. When Options.Cartographer is false (default) it
			// returns nil and the injection below yields "". A nil cart
			// is the documented off-state; the finder's task message is
			// byte-identical to the pre-cartographer build.
			cart := f.newCartographer(ctx, result, cartographerClient, snap, targets, fps, budget)
			n, err := f.hypothesize(ctx, scanRunID, finderClient, persona, kind,
				f.opts.Discovery.ChangeContext, langs, targets, seams, budget, result, fps, touchCoverage, cart, emit)
			hypothesizedCount = n
			hypothesizeErr = err
			// Emit StageFinished(hypothesize) at FINDER DRAIN time, not after the
			// whole pipeline settles: the status snapshot resets LiveCandidates on
			// this event, and with verify running concurrently the live finder
			// counter would otherwise never reset until end-of-run. FinderFailures
			// was folded into result.Stats under the stage's own mutex before
			// hypothesize returned, so the read here is ordered.
			if err == nil {
				progress.Emit(sink, progress.Event{
					Kind: progress.KindStageFinished, Stage: progress.StageHypothesize,
					Counts: &progress.Counts{
						Hypothesized:   n,
						FinderFailures: result.Stats.FinderFailures,
					},
				})
			}
		}
		close(candCh)
	}()

	// ---- Stage B: Triage (streaming consumer) ----
	triageStarted := false
	var triageStartOnce sync.Once
	emitTriageStart := func() {
		triageStartOnce.Do(func() {
			progress.Emit(sink, progress.Event{Kind: progress.KindStageStarted, Stage: progress.StageTriage})
			triageStarted = true
		})
	}

	var triageWg sync.WaitGroup
	triageWg.Add(1)
	var triagedCount int

	go func() {
		defer triageWg.Done()
		defer close(verCh)

		for c := range candCh {
			emitTriageStart()
			if err := ts.process(ctx, f.store, &result.Stats, c); err != nil {
				triageErr = err
				for range candCh { // drain so hypothesize emit doesn't block
				}
				return
			}
			for _, primary := range ts.popReady() {
				select {
				case verCh <- primary:
					progress.Emit(sink, progress.Event{Kind: progress.KindCandidateTriaged, Title: primary.Title})
				case <-ctx.Done():
					for range candCh {
					}
					return
				}
			}
		}
		// candCh closed: hypothesize done. flush() is a no-op in streaming model.
		ts.flush()
		for _, primary := range ts.popReady() {
			select {
			case verCh <- primary:
				progress.Emit(sink, progress.Event{Kind: progress.KindCandidateTriaged, Title: primary.Title})
			case <-ctx.Done():
			}
		}
		triagedCount = ts.survivorCount
	}()

	// ---- Stage E: In-run repro consumer (concurrent with verify + discovery) ----
	// The consumer goroutine reads from reproQ.ch and spawns one worker per
	// finding. Parked workers are O(findings), bounded by the idle slot pool
	// for EXECUTION but not for goroutine count — acceptable for a
	// precision-first funnel (small finding counts), revisit if finding
	// volumes grow by orders of magnitude.
	// finding. It runs concurrently with the verify dispatcher (and hypothesize),
	// so a finding verified early in a long scan gets its repro attempt while
	// discovery continues — the streaming-repro guarantee.
	//
	// The consumer exits when reproQ.ch is closed (after verifyWg.Wait()). The
	// workers are tracked by reproWg; we wait on reproWg after close to ensure
	// all attempts complete before finalize.
	if reproQ != nil {
		go func() {
			defer close(reproConsumerDone)
			for fi := range reproQ.ch {
				fi := fi
				// Add before spawning: Wait() called after consumer finishes
				// must not return before all workers are added.
				reproWg.Add(1)
				go func() {
					defer reproWg.Done()
					f.runReproAttempt(ctx, fi, scanRunID)
				}()
			}
		}()
	}

	// ---- Stage C + D: Verify + immediate persist (dispatcher) ----
	// Fan out one verify unit per forwarded primary on the HIGH-priority slot
	// class (verifyFanout owns the goroutine + slot + WaitGroup), so a candidate
	// arriving mid-discovery can start verifying immediately.
	var verifyStarted atomic.Bool
	var verifyStartOnce sync.Once

	var candIdx int
	verifyFanout := f.newFanout(ctx, slotHigh)
	for primary := range verCh {
		verifyStartOnce.Do(func() {
			progress.Emit(sink, progress.Event{Kind: progress.KindStageStarted, Stage: progress.StageVerify})
			verifyStarted.Store(true)
		})

		c := primary
		idx := candIdx
		candIdx++
		verifyFanout.spawn(func(runCtx context.Context) {
			f.runVerifyAndPersist(runCtx, verifierClient, arbiterClient, persona, c, idx,
				snap.Commit, fps, budget, result, clusterReg,
				&findingsMu, &allFindings, &verifyKilled,
				&sbExecs, &sbMillis, setVerifyErr,
				reproQ)
		})
	}

	// Wait for all verify goroutines to finish.
	verifyFanout.wait()

	// Close reproQ.ch after all verify goroutines finish. The consumer goroutine
	// exits its range loop and closes reproConsumerDone.
	if reproQ != nil {
		close(reproQ.ch)
		// Wait for the consumer to finish spawning all worker goroutines.
		<-reproConsumerDone
		// Wait for all worker goroutines to complete. Drain the overflow slice
		// next so the overflow pass only starts after all channel-path attempts
		// are done — avoiding confusion if the same finding appears in both.
		reproWg.Wait()

		// Drain the overflow slice: findings that couldn't fit into reproQ.ch
		// during the verify phase (non-blocking enqueue fell back to overflow).
		// runReproAttempt's claim check prevents double-attempts: if a finding
		// was already attempted via the channel path, the overflow drain is a
		// cheap no-op for that finding.
		for _, fi := range reproQ.drainOverflow() {
			// Sequential: overflow is typically empty; no goroutine overhead needed.
			f.runReproAttempt(ctx, fi, scanRunID)
		}
	}

	// Wait for triage goroutine to finish (it closed verCh which unblocked us).
	triageWg.Wait()

	// Wait for hypothesize goroutine to finish.
	hypothesizeWg.Wait()

	// --- Error classification ---
	// Check errors in pipeline order: hypothesize → triage → verify.
	// ctx cancellation takes precedence (Interrupted); other errors are Aborted.
	if hypothesizeErr != nil {
		if ctx.Err() != nil {
			result.Stats.Interrupted = true
			return nil, ctx.Err()
		}
		result.Stats.Aborted = true
		return nil, hypothesizeErr
	}
	if triageErr != nil {
		if ctx.Err() != nil {
			result.Stats.Interrupted = true
			return nil, ctx.Err()
		}
		result.Stats.Aborted = true
		return nil, triageErr
	}
	verifyErrMu.Lock()
	capturedVerifyErr := verifyErr
	verifyErrMu.Unlock()
	if capturedVerifyErr != nil {
		if ctx.Err() != nil {
			result.Stats.Interrupted = true
			return nil, ctx.Err()
		}
		result.Stats.Aborted = true
		return nil, capturedVerifyErr
	}
	// ctx cancelled but no stage error: we completed all stages despite cancellation
	// (all goroutines drained), but must still classify as Interrupted.
	if ctx.Err() != nil {
		result.Stats.Interrupted = true
		return nil, ctx.Err()
	}

	// --- Stats fold ---
	result.Stats.Hypothesized = hypothesizedCount
	if !triageStarted {
		// Emit triage start/finish even if no candidates arrived (zero-candidate run).
		progress.Emit(sink, progress.Event{Kind: progress.KindStageStarted, Stage: progress.StageTriage})
	}
	result.Stats.Triaged = triagedCount
	progress.Emit(sink, progress.Event{
		Kind: progress.KindStageFinished, Stage: progress.StageTriage,
		Counts: &progress.Counts{Hypothesized: hypothesizedCount, Triaged: triagedCount},
	})
	if !verifyStarted.Load() {
		progress.Emit(sink, progress.Event{Kind: progress.KindStageStarted, Stage: progress.StageVerify})
	}

	findingsMu.Lock()
	findings := allFindings
	killed := verifyKilled
	// result.Stats.VerifierRuns / Failures / ArbiterRuns / Kills / Failures were
	// folded under findingsMu by each verify goroutine.
	findingsMu.Unlock()

	// Fold late corroboration into the IN-MEMORY findings. A corroborating
	// lens that arrived after its primary persisted was written to the store
	// row at attach time (by triage or the verify goroutine's late path), but
	// the copy captured in allFindings predates it. All consumers have drained
	// here, so the registry read cannot race. This keeps Result.Findings
	// equal to the store regardless of arrival timing — the cluster-level
	// equivalence invariant.
	for i := range findings {
		late := clusterReg.AttachedLenses(findings[i].Fingerprint)
		if len(late) == 0 {
			continue
		}
		merged := dedupLenses(append(findings[i].CorroboratingLenses, late...))
		added := merged[:0:0]
		for _, l := range merged {
			already := false
			for _, have := range findings[i].CorroboratingLenses {
				if have == l {
					already = true
					break
				}
			}
			if !already {
				added = append(added, l)
			}
		}
		findings[i].CorroboratingLenses = merged
		findings[i].Reasoning = appendCorroboration(findings[i].Reasoning, added)
	}

	// Fold late sites into the IN-MEMORY findings and the store row. A site
	// staged by a root-cause-merged member that arrived after its primary's
	// verify goroutine completed DrainStagedSites ends up in the registry's
	// lateSites (via SignalPersisted's TOCTOU move or AddStagedSite's
	// post-persist path). All triage processing has finished by now
	// (triageWg.Wait above), so the registry is stable and this read cannot race.
	for i := range findings {
		late := clusterReg.DrainLateSites(findings[i].Fingerprint)
		if len(late) == 0 {
			continue
		}
		if err := f.store.AppendFindingSites(ctx, findings[i].Fingerprint, late); err != nil {
			// Best-effort: a failure here loses these extra sites but doesn't
			// abort the run. The primary finding still stands.
			continue
		}
		// Merge into the in-memory finding, deduplicating by (file,line).
		type siteKey struct {
			f string
			l int
		}
		have := make(map[siteKey]bool, len(findings[i].Sites))
		for _, s := range findings[i].Sites {
			have[siteKey{s.File, s.Line}] = true
		}
		for _, s := range late {
			if !have[siteKey{s.File, s.Line}] {
				findings[i].Sites = append(findings[i].Sites, s)
			}
		}
	}

	result.Stats.SandboxExecs = int(sbExecs.Load())
	result.Stats.SandboxExecMillis = sbMillis.Load()

	result.Stats.Verified = 0
	for _, fi := range findings {
		if fi.Tier == domain.TierVerified {
			result.Stats.Verified++
		}
	}
	result.Stats.Killed = killed
	result.Stats.Suspected = 0
	for _, fi := range findings {
		if fi.Tier == domain.TierSuspected {
			result.Stats.Suspected++
		}
	}

	progress.Emit(sink, progress.Event{
		Kind: progress.KindStageFinished, Stage: progress.StageVerify,
		Counts: &progress.Counts{
			Hypothesized: hypothesizedCount, Triaged: triagedCount,
			Verified: result.Stats.Verified, Killed: killed,
		},
	})
	progress.Emit(sink, progress.Event{Kind: progress.KindStageStarted, Stage: progress.StagePersist})
	progress.Emit(sink, progress.Event{Kind: progress.KindStageFinished, Stage: progress.StagePersist})

	sortFindings(findings)
	result.Findings = findings

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

	finalized = true
	if err := finalize(&result.Stats); err != nil {
		return nil, fmt.Errorf("funnel: finish scan run: %w", err)
	}

	if _, err := f.store.PruneAgentUnits(ctx, keepRuns); err != nil {
		f.note(result, fmt.Sprintf("observability: PruneAgentUnits failed: %v", err))
	}
	if _, err := f.store.PruneDeadHypotheses(ctx, keepRuns); err != nil {
		f.note(result, fmt.Sprintf("observability: PruneDeadHypotheses failed: %v", err))
	}

	return result, nil
}

// pendingToCandidate rebuilds a funnel Candidate from a persisted
// pending_candidates row for replay. Fingerprint is intentionally left unset:
// the triage consumer recomputes it (store.Fingerprint) and re-runs the
// scope/dedup/suppression checks, so a replayed candidate is re-anchored to the
// current snapshot exactly like a fresh one. PendingID carries the WAL row's
// primary key so the terminal-fate handlers delete it.
func pendingToCandidate(pc store.PendingCandidate) Candidate {
	return Candidate{
		Lens:                pc.Lens,
		File:                pc.File,
		Line:                pc.Line,
		Title:               pc.Title,
		Description:         pc.Description,
		Severity:            normalizeSeverity(domain.Severity(pc.Severity)),
		Evidence:            pc.Evidence,
		Confidence:          normalizeConfidence(domain.Confidence(pc.Confidence)),
		CorroboratingLenses: pc.CorroboratingLenses,
		PendingID:           pc.ID,
	}
}

// findingToCandidate rebuilds a funnel Candidate from a durable OPEN Tier-3
// suspected finding for re-verification (ReverifySuspected). The candidate is
// routed through the same triage + verify pipeline as a WAL-replayed one, but
// is materially different: there is no pending_candidates WAL row (PendingID
// is ""), and the durable open finding row is the unit of state that must be
// transitioned when the verifier refutes the candidate (see Reverify and the
// verify-stream KILL region). PendingID is intentionally left ""; the
// triage consumer does not need to re-anchor the fingerprint (the finding
// already carries it), so Fingerprint is set from the finding to keep the
// verify-stage signals (e.g. SignalPersisted) consistent across the run.
//
// Confidence is forced to ConfidenceHigh: a low-confidence candidate is
// dropped at triage step 1 (triage_streaming.go:305), and a Tier-3 finding
// is by definition worth re-judging — its prior triage passed and only the
// verify stage was halted, so we deliberately keep it out of the low band.
// The Sites slice is converted from []store.Site to []Site to mirror the
// pendingToCandidate shape (Candidate uses the funnel package's Site type,
// pendingToCandidate's input already only carries locations as fields).
func findingToCandidate(fi store.Finding) Candidate {
	sites := make([]Site, len(fi.Sites))
	for i, s := range fi.Sites {
		sites[i] = Site{File: s.File, Line: s.Line}
	}
	return Candidate{
		Lens:                fi.Lens,
		File:                fi.File,
		Line:                fi.Line,
		Title:               fi.Title,
		Description:         fi.Description,
		Severity:            normalizeSeverity(fi.Severity),
		Evidence:            fi.Reasoning,
		Confidence:          domain.ConfidenceHigh,
		Fingerprint:         fi.Fingerprint,
		LocusKey:            fi.LocusKey, // recomputed in triage process(); carried for round-trip fidelity
		CorroboratingLenses: fi.CorroboratingLenses,
		Sites:               sites,
		Reverify:            true,
	}
}
