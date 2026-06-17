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
	cacheWeight := f.opts.CacheReadBudgetWeight
	if cacheWeight == 0 {
		cacheWeight = DefaultCacheReadBudgetWeight
	}
	budget := newBudgetState(f.opts.TokenBudget, rec, cacheWeight)
	// Per-task token claims: each finder / verifier agent run is capped at its
	// role's claim so one breadth-heavy run cannot be granted a whole stage's
	// reserve at launch; the unspent remainder of a claim stays in the pool for
	// sibling runs (the claimant system, bugbot-8mj).
	budget.finderClaim = f.opts.FinderTokenClaim
	budget.verifyClaim = f.opts.VerifierTokenClaim
	// Reserve a slice of the per-run budget for downstream verification so the
	// breadth-heavy finder stage cannot drain the whole pool and orphan every
	// candidate before it is verified (bugbot-8mj / bugbot-3lt). A no-op when the
	// budget is unlimited or the share disables the reservation.
	budget.reserveForDownstream(f.opts.FinderBudgetShare)

	result := &Result{ScanRunID: scanRunID, Commit: snap.Commit}

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
		verifyWg     sync.WaitGroup
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

	// ---- Stage A: Hypothesize ----
	progress.Emit(sink, progress.Event{Kind: progress.KindStageStarted, Stage: progress.StageHypothesize})

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
		cart := f.cartograph(ctx, finderClient, snap, targets, fps, budget)
		n, err := f.hypothesize(ctx, scanRunID, finderClient, persona, kind,
			f.opts.ChangeContext, langs, targets, seams, budget, result, fps, touchCoverage, cart, emit)
		hypothesizedCount = n
		hypothesizeErr = err
		close(candCh)
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
	// Spawn a goroutine per forwarded primary. HIGH-priority slot acquisition
	// means a candidate arriving mid-discovery can start verifying immediately.
	var verifyStarted atomic.Bool
	var verifyStartOnce sync.Once

	var candIdx int
	for primary := range verCh {
		verifyStartOnce.Do(func() {
			progress.Emit(sink, progress.Event{Kind: progress.KindStageStarted, Stage: progress.StageVerify})
			verifyStarted.Store(true)
		})

		c := primary
		idx := candIdx
		candIdx++
		verifyWg.Add(1)
		go func() {
			defer verifyWg.Done()
			f.runVerifyAndPersist(ctx, verifierClient, persona, c, idx,
				snap.Commit, fps, budget, result, clusterReg,
				&findingsMu, &allFindings, &verifyKilled,
				&sbExecs, &sbMillis, setVerifyErr,
				reproQ)
		}()
	}

	// Wait for all verify goroutines to finish.
	verifyWg.Wait()

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

	result.Stats.SandboxExecs = int(sbExecs.Load())
	result.Stats.SandboxExecMillis = sbMillis.Load()

	result.Stats.Verified = 0
	for _, fi := range findings {
		if fi.Tier == tierVerified {
			result.Stats.Verified++
		}
	}
	result.Stats.Killed = killed
	result.Stats.Suspected = 0
	for _, fi := range findings {
		if fi.Tier == tierSuspected {
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

	return result, nil
}
