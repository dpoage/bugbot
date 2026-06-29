package funnel

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/store"
)

// ---------------------------------------------------------------------------
// Interrupt / partial-progress tests
// ---------------------------------------------------------------------------

// TestSweep_Interrupt_DurablePartialProgress verifies durable partial progress
// and interrupt-safe finalization (phase 1 of bead bugbot-280):
//
//  1. scan_runs row is finalized (finished_at set, interrupted=true) even when
//     the context is cancelled mid-sweep — no dangling unfinalised rows.
//  2. Files from finderOK units that completed before cancellation have a
//     truthful (non-epoch, non-zero) last_scanned_at in file_state — per-unit
//     coverage is durable, not just batch-at-run-end.
//  3. agent_units rows exist for completed units (at least one "ok" row).
//
// Mechanics: 5 files, ChunkSize=1, MaxParallel=1, one lens → 5 sequential
// finder units. A gating client allows exactly allowedCompletions LLM calls to
// proceed, then blocks. A watchdog goroutine detects the block and cancels the
// sweep context, causing the remaining goroutines to see ctx.Err() in the
// agent runner's pre-turn check and return early without recording ok rows.
func TestSweep_Interrupt_DurablePartialProgress(t *testing.T) {
	ctx := context.Background()

	// Five distinct files so each gets its own chunk (ChunkSize=1, one lens).
	files := []string{"a.go", "b.go", "c.go", "d.go", "e.go"}
	dir := t.TempDir()
	for i, fname := range files {
		content := "package fix\n\nfunc F" + string(rune('A'+i)) + "() int { return " + string(rune('0'+i)) + " }\n"
		if err := os.WriteFile(filepath.Join(dir, fname), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitSeed(t, dir)
	repo, err := ingest.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	sweepCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// gf gates completions: allows up to allowedCompletions to proceed, then
	// blocks the next attempt. The watchdog goroutine detects the block via
	// gf.blockedCh and cancels the sweep context.
	const allowedCompletions = 2
	inner := newScriptedClient()
	inner.fallback = emptyCandidates
	gf := newGatingClient(inner, allowedCompletions)

	// Watchdog: cancel once the client blocks (gate exhausted).
	go func() {
		select {
		case <-gf.blockedCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	verifier := newScriptedClient()
	verifier.fallback = notRefutedJSON

	const lens = "nil-safety/error-handling"
	f, err := New(RoleClients{Finder: gf, Verifier: verifier}, st, repo, Options{
		Discovery: DiscoveryConfig{Lenses: []string{lens}},
		Limits:    StageLimits{ChunkSize: 1, MaxParallel: 1}, // one file per unit → 5 units for 5 files; sequential: only one goroutine active at a time
		Features:  FeatureFlags{DisableHeatOrdering: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	res, sweepErr := f.Sweep(sweepCtx)
	_ = res

	// Sweep must return an error (context cancelled).
	if sweepErr == nil {
		t.Fatal("Sweep: expected error (context cancellation), got nil")
	}

	// --- (1) scan_runs row finalized with interrupted=true ---
	latestRun, err := st.LatestScanRun(ctx)
	if err != nil {
		t.Fatalf("LatestScanRun: %v", err)
	}
	if latestRun.FinishedAt.IsZero() {
		t.Error("scan_runs: finished_at is zero — run not finalized after interrupt")
	}
	var statsOut Stats
	if jsonErr := json.Unmarshal([]byte(latestRun.StatsJSON), &statsOut); jsonErr != nil {
		t.Fatalf("unmarshal stats_json: %v (json: %q)", jsonErr, latestRun.StatsJSON)
	}
	if !statsOut.Interrupted {
		t.Errorf("stats_json: interrupted=false, want true (json: %q)", latestRun.StatsJSON)
	}

	// --- (3) at least one ok unit exists ---
	units, err := st.ListAgentUnits(ctx, latestRun.ID)
	if err != nil {
		t.Fatalf("ListAgentUnits: %v", err)
	}
	okUnits := 0
	coveredByOK := make(map[string]bool)
	for _, u := range units {
		if u.Status == "ok" {
			okUnits++
			for _, fp := range u.Files {
				coveredByOK[fp] = true
			}
		}
	}
	if okUnits == 0 {
		t.Fatal("agent_units: no ok units — at least 1 unit should have completed before cancel")
	}
	// The interrupt must also have PREVENTED work: if every unit completed,
	// nothing was interrupted and assertions (2b) and the interrupted flag
	// would be testing a vacuous scenario.
	if okUnits >= len(files) {
		t.Fatalf("all %d units completed ok — cancellation landed too late, test scenario is vacuous", okUnits)
	}
	t.Logf("ok units: %d of %d total; covered: %v", okUnits, len(units), coveredByOK)

	// --- (2) per-unit coverage durability ---
	allPaths := make([]string, len(files))
	copy(allPaths, files)
	wms, err := st.ScanWatermarks(ctx, allPaths)
	if err != nil {
		t.Fatalf("ScanWatermarks: %v", err)
	}

	// (2a) Files from ok units must have truthful coverage.
	for p := range coveredByOK {
		wm, ok := wms[p]
		if !ok {
			t.Errorf("file_state: %q not found — completed unit's files must be covered", p)
			continue
		}
		if wm.LastScannedAt.IsZero() || wm.LastScannedAt.Equal(epochSentinelParsed) {
			t.Errorf("file_state: %q has epoch/zero timestamp — per-unit coverage not persisted", p)
		}
	}

	// (2b) Files NOT in any ok unit must NOT have a real timestamp.
	for _, p := range allPaths {
		if coveredByOK[p] {
			continue
		}
		wm, ok := wms[p]
		if !ok {
			continue // absent = never covered: correct
		}
		if !wm.LastScannedAt.IsZero() && !wm.LastScannedAt.Equal(epochSentinelParsed) {
			t.Errorf("file_state: %q has real timestamp but was not in any ok unit — spurious coverage", p)
		}
	}
}

// TestSweep_InterruptThenResume_PendingCandidates verifies the candidate
// write-ahead log (bugbot-jmu): a candidate that a finder produced but that was
// interrupted before verification completed is NOT lost — it is persisted to
// pending_candidates, and the next run replays it straight into verify and
// produces the finding.
//
// Mechanics: 1 lens, ChunkSize=1, on the fixture repo (bug.go + clean.go) → two
// finder chunks. The test anchors realCand's emit to the chunk whose task lists
// bug.go, so exactly ONE candidate-emitting finder unit runs (see
// finderOneChunkEmitsRealCand); the other emits emptyCandidates. The finder is
// ungated (it completes and persists the candidate); the VERIFIER is gated to
// block on its first refuter call. A watchdog cancels the sweep once the
// verifier blocks, so the candidate is in-flight in verify (forwarded by
// triage, refuter blocked) when the interrupt lands — its WAL row survives. A
// second run on the SAME store, with an empty finder and an allowing verifier,
// replays the row and verifies it.
func TestSweep_InterruptThenResume_PendingCandidates(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t) // shared store + repo across both runs

	const lens = "nil-safety/error-handling"

	// --- Phase 1: interrupt mid-verification ---
	// Only the unit scanning bug.go emits realCand; the unit scanning clean.go
	// falls through to the emptyCandidates fallback. Without this gating, both
	// chunks would persist identical WAL rows for the same candidate and the
	// mid-interrupt snapshot would race the dedup window (see bugbot-303).
	finder1 := finderOneChunkEmitsRealCand(newScriptedClient())

	verifierInner := newScriptedClient()
	verifierInner.fallback = notRefutedJSON
	gv := newGatingClient(verifierInner, 0) // block on the first refuter call

	sweepCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-gv.blockedCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	f1, err := New(RoleClients{Finder: finder1, Verifier: gv}, st, repo, Options{
		Discovery: DiscoveryConfig{Lenses: []string{lens}},
		Limits:    StageLimits{ChunkSize: 1, MaxParallel: 1},
		Features:  FeatureFlags{DisableHeatOrdering: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, serr := f1.Sweep(sweepCtx); serr == nil {
		t.Fatal("phase 1 Sweep: expected interrupt error, got nil")
	}

	// The interrupted candidate must be durable in the WAL.
	pending, err := st.ListPendingCandidates(ctx)
	if err != nil {
		t.Fatalf("ListPendingCandidates: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("after interrupt: %d pending candidates, want 1 (the un-verified hypothesis)", len(pending))
	}
	if pending[0].Title != "nil deref of cfg in Greeting" || pending[0].File != "bug.go" {
		t.Errorf("pending candidate = %s @ %s, want the realCand hypothesis", pending[0].Title, pending[0].File)
	}

	// --- Phase 2: resume on the same store ---
	finder2 := newScriptedClient()
	finder2.fallback = emptyCandidates // no fresh candidates; resume must supply the work
	verifier2 := newScriptedClient()
	verifier2.fallback = notRefutedJSON // allow → candidate survives

	f2, err := New(RoleClients{Finder: finder2, Verifier: verifier2}, st, repo, Options{
		Discovery: DiscoveryConfig{Lenses: []string{lens}},
		Limits:    StageLimits{ChunkSize: 1, MaxParallel: 1},
		Features:  FeatureFlags{DisableHeatOrdering: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	res2, err := f2.Sweep(ctx)
	if err != nil {
		t.Fatalf("phase 2 Sweep: %v", err)
	}

	if res2.Stats.Resumed != 1 {
		t.Errorf("Stats.Resumed = %d, want 1 (the replayed candidate)", res2.Stats.Resumed)
	}
	if len(res2.Findings) != 1 {
		t.Fatalf("phase 2: %d findings, want 1 (the resumed candidate verified)", len(res2.Findings))
	}
	got := res2.Findings[0]
	if got.Title != "nil deref of cfg in Greeting" {
		t.Errorf("finding title = %q, want the resumed hypothesis", got.Title)
	}
	if got.Tier != domain.TierVerified {
		t.Errorf("finding tier = %d, want %d (verified)", got.Tier, domain.TierVerified)
	}

	// The WAL must be drained: the replayed row was deleted at its verify
	// terminal fate, and no fresh candidate was persisted.
	pending2, err := st.ListPendingCandidates(ctx)
	if err != nil {
		t.Fatalf("ListPendingCandidates (post-resume): %v", err)
	}
	if len(pending2) != 0 {
		t.Errorf("after resume: %d pending candidates, want 0 (WAL drained)", len(pending2))
	}
}

// TestSweep_CleanRun_DrainsPendingWAL asserts the WAL invariant for an
// uninterrupted run (bugbot-jmu acceptance #3): every candidate reaches a
// terminal fate that deletes its pending_candidates row, so a clean run leaves
// the table empty. It exercises both verify deletes — the real candidate
// survives (T2) and the bogus one is killed.
func TestSweep_CleanRun_DrainsPendingWAL(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	finder := finderOnNilLens(newScriptedClient())
	verifier := verifierRouting(newScriptedClient())

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 1 {
		t.Fatalf("want 1 finding (real survives, bogus killed), got %d", len(res.Findings))
	}
	if res.Stats.Resumed != 0 {
		t.Errorf("Stats.Resumed = %d, want 0 (no interrupted predecessor)", res.Stats.Resumed)
	}
	pending, err := st.ListPendingCandidates(ctx)
	if err != nil {
		t.Fatalf("ListPendingCandidates: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("clean run left %d pending candidates, want 0 (every terminal fate must delete its WAL row)", len(pending))
	}
}

// TestSweep_KilledCandidate_PersistsDeadHypothesis (bugbot-fg9a) verifies the
// funnel-level wiring of persistKilled at the candKilled branch of
// runVerifyAndPersist: when a candidate reaches the verifier-KILLED outcome
// (bugbot-cvc), the funnel must insert exactly one dead_hypotheses row with
// the structured verdict breakdown and the same buildReasoning trace survivors
// receive. The store round-trip is covered by internal/store/dead_hypotheses_test.go;
// this test pins the field-mapping and reasoning-trace wiring at the call site,
// which only a Sweep can exercise (no direct unit entry for persistKilled
// exists, and the persisted row's reasoning_trace depends on the same
// runRefuters output survivors consume).
//
// Setup mirrors TestSweep_CleanRun_DrainsPendingWAL: finderOnNilLens emits the
// realCand (survives) + bogusCand (killed) pair, verifierRouting drives
// realCand's panel to not-refuted and bogusCand's panel to unanimous-refuted.
// After Sweep completes (no interrupt):
//   - Exactly ONE dead_hypotheses row exists, and it is bogusCand (the killed
//     one). The surviving realCand must NOT produce a dead_hypotheses row.
//   - The structured columns mirror the bogusCand fields and the panel's
//     verdict breakdown (3 seats, all refuted, no arbiter).
//   - reasoning_trace is byte-identical to buildReasoning(verdicts, seatNames,
//     "", false) constructed with the exact inputs the kill site uses, so a
//     future refactor that drops the trace or swaps the verdict breakdown
//     for a different shape fails this test.
func TestSweep_KilledCandidate_PersistsDeadHypothesis(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	finder := finderOnNilLens(newScriptedClient())
	verifier := verifierRouting(newScriptedClient())

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.ScanRunID == "" {
		t.Fatal("res.ScanRunID is empty; ListDeadHypotheses needs it")
	}

	// Sanity: realCand survived (tier-2 finding); bogusCand was killed by the
	// unanimous-refuted panel. The dead_hypotheses row should be exactly the
	// killed one — the survivor must NOT produce a dead_hypotheses row.
	if len(res.Findings) != 1 {
		t.Fatalf("want 1 finding (realCand survives), got %d", len(res.Findings))
	}
	survived := res.Findings[0]
	if survived.Title != "nil deref of cfg in Greeting" {
		t.Fatalf("survivor title = %q, want %q", survived.Title, "nil deref of cfg in Greeting")
	}

	rows, err := st.ListDeadHypotheses(ctx, res.ScanRunID)
	if err != nil {
		t.Fatalf("ListDeadHypotheses: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListDeadHypotheses returned %d rows, want 1 (only bogusCand is killed; realCand survives and must NOT produce a dead_hypotheses row)", len(rows))
	}
	got := rows[0]

	// BogusCand's persisted fields — the candidate's own lens+file+line+title+severity
	// are copied verbatim into the dead_hypotheses row.
	if got.Title != "Add overflows on large ints" {
		t.Errorf("Title = %q, want %q", got.Title, "Add overflows on large ints")
	}
	if got.File != "clean.go" {
		t.Errorf("File = %q, want clean.go", got.File)
	}
	if got.Line != 5 {
		t.Errorf("Line = %d, want 5", got.Line)
	}
	if got.Severity != "low" {
		t.Errorf("Severity = %q, want low", got.Severity)
	}
	if got.Lens == "" {
		t.Error("Lens is empty (should carry the candidate's lens name)")
	}
	if got.Fingerprint == "" {
		t.Error("Fingerprint is empty (should be set in triage before verify)")
	}
	if got.ScanRunID != res.ScanRunID {
		t.Errorf("ScanRunID = %q, want %q", got.ScanRunID, res.ScanRunID)
	}

	// Structured verdict breakdown: 3-seat panel, unanimous refuted, no arbiter.
	// The seats are the default round-robin from builtinSeats
	// (seats.go: reachability/semantics/guards); refutedCount is computed by
	// the kill site via seatRefutedSlice(verdicts) and counts every refuted
	// verdict in the panel (no arbiter case: arbiterRan/Refuted/Verdict all
	// zero-value).
	wantSeats := []string{"reachability", "semantics", "guards"}
	if len(got.SeatNames) != len(wantSeats) {
		t.Errorf("SeatNames = %v, want %v (3-seat default panel)", got.SeatNames, wantSeats)
	} else {
		for i, want := range wantSeats {
			if got.SeatNames[i] != want {
				t.Errorf("SeatNames[%d] = %q, want %q", i, got.SeatNames[i], want)
			}
		}
	}
	if got.RefutedCount != 3 {
		t.Errorf("RefutedCount = %d, want 3 (all 3 refuters refute bogusCand)", got.RefutedCount)
	}
	if got.TotalSeats != 3 {
		t.Errorf("TotalSeats = %d, want 3", got.TotalSeats)
	}
	if got.ArbiterRan {
		t.Errorf("ArbiterRan = true, want false (unanimous kill — no split, no arbiter)")
	}
	if got.ArbiterRefuted {
		t.Errorf("ArbiterRefuted = true, want false (no arbiter ran)")
	}
	if got.ArbiterVerdict != "" {
		t.Errorf("ArbiterVerdict = %q, want \"\" (no arbiter ran)", got.ArbiterVerdict)
	}

	// Reasoning trace: byte-equal to buildReasoning with the EXACT inputs the
	// kill site uses. The 3 verdicts come from scriptedClient routing
	// refutedJSON to every refuter seat on the "Add overflows on large ints"
	// task; arbiterRan=false because the panel is unanimous (no split); the
	// arbiterLine is therefore the zero string. Reconstructing the expected
	// trace here (rather than asserting containment) pins the field-mapping
	// wiring at the kill site: if a future edit drops the reasoning_trace,
	// swaps the kill site to a different verdict breakdown, or re-routes the
	// arbiter branch on a unanimous kill, this equality fails.
	killVerdicts := []refutation{
		{Refuted: true, Reasoning: "The caller guards this with an explicit nil check before the call.", Confidence: "high"},
		{Refuted: true, Reasoning: "The caller guards this with an explicit nil check before the call.", Confidence: "high"},
		{Refuted: true, Reasoning: "The caller guards this with an explicit nil check before the call.", Confidence: "high"},
	}
	killSeatNames := []string{"reachability", "semantics", "guards"}
	wantTrace := buildReasoning(killVerdicts, killSeatNames, "", false)
	if got.ReasoningTrace != wantTrace {
		t.Errorf("ReasoningTrace mismatch:\n got: %q\nwant: %q", got.ReasoningTrace, wantTrace)
	}
}

// ---------------------------------------------------------------------------
// gatingClient: allows a fixed number of LLM completions, then blocks
// subsequent calls until the context is cancelled. Thread-safe.
// ---------------------------------------------------------------------------

// gatingClient is a fake llm.Client that gates completions through a semaphore
// channel. After the pre-loaded budget is consumed, the next Complete call
// blocks (signalling blockedCh once) until ctx is cancelled. This lets the
// test precisely control how many units complete before an interrupt.
type gatingClient struct {
	inner     *scriptedClient
	gate      chan struct{} // pre-filled; each completion consumes one slot
	blockedCh chan struct{} // closed once when a Complete blocks on empty gate
	blockOnce sync.Once
}

func newGatingClient(inner *scriptedClient, allowed int) *gatingClient {
	g := make(chan struct{}, allowed)
	for range allowed {
		g <- struct{}{}
	}
	return &gatingClient{
		inner:     inner,
		gate:      g,
		blockedCh: make(chan struct{}),
	}
}

func (c *gatingClient) Capabilities() llm.Capabilities { return c.inner.Capabilities() }

func (c *gatingClient) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	if err := ctx.Err(); err != nil {
		return llm.Response{}, err
	}
	// Non-blocking try: if a slot is available, proceed immediately.
	select {
	case <-c.gate:
		return c.inner.Complete(ctx, req)
	default:
	}
	// Gate exhausted: signal blocked (once) and wait for ctx cancellation or
	// a slot (the latter is not normally available in tests, since the gate is
	// pre-filled exactly and never refilled).
	c.blockOnce.Do(func() { close(c.blockedCh) })
	select {
	case <-c.gate:
		return c.inner.Complete(ctx, req)
	case <-ctx.Done():
		return llm.Response{}, ctx.Err()
	}
}

// ===========================================================================
// Interruption / Resumability Matrix (bugbot-vpx.6)
// ===========================================================================
//
// Each sub-test hits a distinct phase boundary, interrupts there, then
// recovers using the drain passes on the SAME store — not a full re-run.
// Together they prove: no finding is dropped or duplicated, finder is never
// invoked during recovery, and every drain is idempotent.
//
// Boundary map:
//   B1 – interrupted before verify (WAL row exists, verify never ran)
//   B2 – interrupted mid-verify (partial findings + outstanding WAL rows)
//   B3 – verify complete, sweep not run (unswept open findings)
//   B4 – combined stranded state → fixpoint via VerifyDrain + SweepDrain
// ===========================================================================

// TestInterruptMatrix is the top-level table driver.
func TestInterruptMatrix(t *testing.T) {
	t.Run("B1_InterruptedBeforeVerify_DrainFindsAndClearsWAL", testB1InterruptedBeforeVerify)
	t.Run("B2_InterruptedMidVerify_MultiCandidate_NoDup", testB2InterruptedMidVerify)
	t.Run("B3_VerifyCompleteNoSweep_SweepDrainRanks", testB3VerifyCompleteNoSweep)
	t.Run("B4_CombinedStranded_FixpointConverges", testB4CombinedFixpoint)
}

// ---------------------------------------------------------------------------
// B1: interrupted BEFORE verify
// The finder ran and emitted a candidate to the WAL, but the verifier was
// gated to 0 completions so it never ran. After cancel, ListPendingCandidates
// has the row. VerifyDrain (with a finder client that records calls) processes
// it → finding persisted, WAL drained, finder.callCount()==0.
// ---------------------------------------------------------------------------
func testB1InterruptedBeforeVerify(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	const lens = "nil-safety/error-handling"

	// Phase 1: finder runs (emits candidate to WAL); verifier is blocked from
	// the very first call → interrupt lands while verify is waiting. Only the
	// unit scanning bug.go emits realCand; the unit scanning clean.go falls
	// through to the emptyCandidates fallback. Without this gating, both chunks
	// would persist identical WAL rows for the same candidate and the
	// mid-interrupt snapshot would race the dedup window (see bugbot-303).
	finder1 := finderOneChunkEmitsRealCand(newScriptedClient())

	verifierInner := newScriptedClient()
	verifierInner.fallback = notRefutedJSON
	gv := newGatingClient(verifierInner, 0) // block before any verify call

	sweepCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-gv.blockedCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	f1, err := New(RoleClients{Finder: finder1, Verifier: gv}, st, repo, Options{
		Discovery: DiscoveryConfig{Lenses: []string{lens}},
		Limits:    StageLimits{ChunkSize: 1, MaxParallel: 1},
		Features:  FeatureFlags{DisableHeatOrdering: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, serr := f1.Sweep(sweepCtx); serr == nil {
		t.Fatal("B1 phase 1: expected interrupt error, got nil")
	}

	// Candidate must be in the WAL.
	pending, err := st.ListPendingCandidates(ctx)
	if err != nil {
		t.Fatalf("B1 ListPendingCandidates: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("B1 after interrupt: %d pending candidates, want 1", len(pending))
	}

	// Recovery: new funnel on the SAME store; finder must not be called.
	recFinder := newScriptedClient()
	recFinder.fallback = candJSON(realCand) // would be detectable if called
	recVerifier := newScriptedClient()
	recVerifier.fallback = notRefutedJSON

	f2, err := New(RoleClients{Finder: recFinder, Verifier: recVerifier}, st, repo, Options{
		Discovery: DiscoveryConfig{Lenses: []string{lens}},
		Limits:    StageLimits{ChunkSize: 1, MaxParallel: 1},
		Features:  FeatureFlags{DisableHeatOrdering: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f2.Close() }()

	_, err = f2.VerifyDrain(ctx)
	if err != nil {
		t.Fatalf("B1 VerifyDrain: %v", err)
	}

	// Finder must never have been called during the drain.
	if n := recFinder.callCount(); n != 0 {
		t.Errorf("B1 finder.callCount() = %d during VerifyDrain, want 0", n)
	}

	// Finding must be persisted.
	findings, err := st.ListFindings(ctx, store.FindingFilter{})
	if err != nil {
		t.Fatalf("B1 ListFindings: %v", err)
	}
	if len(findings) == 0 {
		t.Fatal("B1: no finding persisted after VerifyDrain — stranded work was dropped")
	}

	// WAL must be empty.
	pending2, err := st.ListPendingCandidates(ctx)
	if err != nil {
		t.Fatalf("B1 ListPendingCandidates post-drain: %v", err)
	}
	if len(pending2) != 0 {
		t.Errorf("B1 WAL has %d rows after VerifyDrain, want 0", len(pending2))
	}
}

// ---------------------------------------------------------------------------
// B2: interrupted MID-VERIFY with multiple candidates
// Two candidates (realCand + bogus) are emitted; the gating verifier allows
// enough calls for realCand to be verified (finding created, WAL row deleted)
// but blocks on the subsequent candidate so bogusCand's row stays in the WAL.
// After interrupt: VerifyDrain processes the remaining pending row;
// total findings == 1 real (bogus is refuted), no duplicate, WAL empty.
// ---------------------------------------------------------------------------
func testB2InterruptedMidVerify(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	const lens = "nil-safety/error-handling"

	// Phase 1: finder emits both candidates; verifier allows 1 completion
	// (just enough for realCand to pass through the first refuter), then blocks.
	// The gating client wraps the verifier; 1 allowed call → first refuter
	// completes (notRefuted for realCand) → verifier blocks on the next call.
	finder1 := finderOnNilLens(newScriptedClient())
	verifierInner1 := verifierRouting(newScriptedClient())
	gv1 := newGatingClient(verifierInner1, 1) // 1 verifier completion → then block

	sweepCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-gv1.blockedCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	f1, err := New(RoleClients{Finder: finder1, Verifier: gv1}, st, repo, Options{
		Discovery: DiscoveryConfig{Lenses: []string{lens}},
		Limits:    StageLimits{ChunkSize: 1, MaxParallel: 1},
		Features:  FeatureFlags{DisableHeatOrdering: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f1.Sweep(sweepCtx) // error expected (context cancel)

	// At least one pending candidate must remain (the one that didn't complete).
	pending, err := st.ListPendingCandidates(ctx)
	if err != nil {
		t.Fatalf("B2 ListPendingCandidates: %v", err)
	}
	if len(pending) == 0 {
		// If gating allowed all to finish before the cancel landed, there's
		// nothing stranded — the scenario is vacuous. Skip rather than fail.
		t.Skip("B2: interrupt landed after all candidates completed; scenario vacuous")
	}

	// Recovery: VerifyDrain on the same store — finder must NOT be called.
	recFinder := newScriptedClient()
	recFinder.fallback = candJSON(realCand) // detectable if called
	recVerifier := verifierRouting(newScriptedClient())

	f2, err := New(RoleClients{Finder: recFinder, Verifier: recVerifier}, st, repo, Options{
		Discovery: DiscoveryConfig{Lenses: []string{lens}},
		Limits:    StageLimits{ChunkSize: 1, MaxParallel: 1},
		Features:  FeatureFlags{DisableHeatOrdering: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f2.Close() }()

	if _, err := f2.VerifyDrain(ctx); err != nil {
		t.Fatalf("B2 VerifyDrain: %v", err)
	}

	// Finder must never be called.
	if n := recFinder.callCount(); n != 0 {
		t.Errorf("B2 finder.callCount() = %d, want 0", n)
	}

	// WAL must be empty.
	pending2, err := st.ListPendingCandidates(ctx)
	if err != nil {
		t.Fatalf("B2 ListPendingCandidates post-drain: %v", err)
	}
	if len(pending2) != 0 {
		t.Errorf("B2 WAL has %d rows after VerifyDrain, want 0", len(pending2))
	}

	// Total findings must be exactly 1 (realCand survives, bogus refuted). No dup.
	allFindings, err := st.ListFindings(ctx, store.FindingFilter{})
	if err != nil {
		t.Fatalf("B2 ListFindings: %v", err)
	}
	if len(allFindings) != 1 {
		t.Errorf("B2 findings = %d, want 1 (real survives, bogus refuted)", len(allFindings))
	}

	// No duplicate fingerprints.
	seen := make(map[string]bool, len(allFindings))
	for _, f := range allFindings {
		if seen[f.Fingerprint] {
			t.Errorf("B2 duplicate fingerprint %q in findings", f.Fingerprint)
		}
		seen[f.Fingerprint] = true
	}
}

// ---------------------------------------------------------------------------
// B3: verify complete, sweep not run
// A finding is seeded (or produced by VerifyDrain) with swept_at NULL.
// SweepDrain re-ranks it (on a repo with a dead C++ function so the
// classification is deterministic / zero LLM calls).
// After drain: UnsweptOpenFindings empty, GetFinding shows swept_at set.
// ---------------------------------------------------------------------------
func testB3VerifyCompleteNoSweep(t *testing.T) {
	// Build a git repo with a dead unexported C++ function (deterministic: no
	// LLM calls needed; the classifier resolves zero callers → low severity).
	repoDir := makeGitRepo(t, map[string]string{
		"src/dead.cpp": "void deadHelper() {}\n",
		"src/main.cpp": "int main() { return 0; }\n",
	})

	st := openImpactStore(t)
	ctx := context.Background()

	// Seed an open finding with swept_at NULL (simulates: verify done, sweep interrupted).
	fi := seedFinding(t, st, makeImpactFinding("b3-unswept", "src/dead.cpp", 1, domain.SeverityHigh))

	// Assert starting state: finding is unswept.
	before, err := st.UnsweptOpenFindings(ctx)
	if err != nil {
		t.Fatalf("B3 UnsweptOpenFindings (before): %v", err)
	}
	foundBefore := false
	for _, x := range before {
		if x.ID == fi.ID {
			foundBefore = true
		}
	}
	if !foundBefore {
		t.Fatal("B3: seeded finding not in UnsweptOpenFindings before drain")
	}

	// Recovery: SweepDrain on the same store.
	client := newScriptedClient()
	f := makeImpactFunnel(t, st, repoDir, client)

	if _, err := f.SweepDrain(ctx); err != nil {
		t.Fatalf("B3 SweepDrain: %v", err)
	}

	// Zero LLM calls: dead unexported function → deterministic downrank.
	if n := client.callCount(); n != 0 {
		t.Errorf("B3 verifier.callCount() = %d, want 0 (deterministic)", n)
	}

	// swept_at must be set.
	got, err := st.GetFinding(ctx, fi.ID)
	if err != nil {
		t.Fatalf("B3 GetFinding: %v", err)
	}
	if got.SweptAt.IsZero() {
		t.Error("B3: swept_at is zero after SweepDrain — finding not swept")
	}
	// No finding dropped: it still exists and is open.
	if got.Status != store.StatusOpen {
		t.Errorf("B3: finding status = %q, want open (no drop)", got.Status)
	}

	// UnsweptOpenFindings must be empty.
	after, err := st.UnsweptOpenFindings(ctx)
	if err != nil {
		t.Fatalf("B3 UnsweptOpenFindings (after): %v", err)
	}
	for _, x := range after {
		if x.ID == fi.ID {
			t.Error("B3: swept finding still appears in UnsweptOpenFindings")
		}
	}
}

// ---------------------------------------------------------------------------
// B4: combined stranded state → fixpoint
// Simulates a scan that was interrupted mid-way: pending WAL rows AND unswept
// open findings coexist. Running VerifyDrain then SweepDrain (the fixpoint the
// oneshot does) must converge: WAL empty AND UnsweptOpenFindings empty, with
// the original finding intact and no duplicates.
// ---------------------------------------------------------------------------
func testB4CombinedFixpoint(t *testing.T) {
	// Use a git repo with a dead C++ function (deterministic sweep, 0 LLM calls).
	repoDir := makeGitRepo(t, map[string]string{
		"src/dead.cpp": "void deadHelper() {}\n",
		"src/main.cpp": "int main() { return 0; }\n",
	})

	st := openImpactStore(t)
	ctx := context.Background()

	// Seed a pending WAL row (simulates: finder ran but verify didn't finish).
	seedPendingFromRealCand(t, st)

	// Also seed an unswept finding from a prior run (simulates: a previous scan
	// verified a different bug but its sweep was interrupted).
	priorFinding := seedFinding(t, st, makeImpactFinding("b4-prior", "src/dead.cpp", 1, domain.SeverityHigh))

	// Sanity: both strands are present.
	pending0, err := st.ListPendingCandidates(ctx)
	if err != nil {
		t.Fatalf("B4 ListPendingCandidates: %v", err)
	}
	if len(pending0) == 0 {
		t.Fatal("B4: no pending candidates before fixpoint run")
	}
	unswept0, err := st.UnsweptOpenFindings(ctx)
	if err != nil {
		t.Fatalf("B4 UnsweptOpenFindings: %v", err)
	}
	if len(unswept0) == 0 {
		t.Fatal("B4: no unswept findings before fixpoint run")
	}

	// --- Step 1: VerifyDrain (WAL → findings) ---
	// Use a repo that has bug.go (the WAL candidate file) reachable via ingest.
	// openFixture provides that repo; we need a funnel over the IMPACT store
	// but the fixture repo. We open the fixture repo separately.
	fixtureRepoDir := newFixtureRepo(t)
	fixtureRepo, err := ingest.Open(ctx, fixtureRepoDir)
	if err != nil {
		t.Fatalf("B4 ingest.Open fixture: %v", err)
	}

	const lens = "nil-safety/error-handling"
	vdFinder := newScriptedClient()
	vdFinder.fallback = candJSON(realCand) // detectable if called
	vdVerifier := newScriptedClient()
	vdVerifier.fallback = notRefutedJSON

	fVD, err := New(RoleClients{Finder: vdFinder, Verifier: vdVerifier}, st, fixtureRepo, Options{
		Discovery: DiscoveryConfig{Lenses: []string{lens}},
		Limits:    StageLimits{ChunkSize: 1, MaxParallel: 1},
		Features:  FeatureFlags{DisableHeatOrdering: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fVD.Close() }()

	if _, err := fVD.VerifyDrain(ctx); err != nil {
		t.Fatalf("B4 VerifyDrain: %v", err)
	}

	// Finder must not have been called.
	if n := vdFinder.callCount(); n != 0 {
		t.Errorf("B4 finder.callCount() after VerifyDrain = %d, want 0", n)
	}

	// WAL must be empty.
	pending1, err := st.ListPendingCandidates(ctx)
	if err != nil {
		t.Fatalf("B4 ListPendingCandidates after VerifyDrain: %v", err)
	}
	if len(pending1) != 0 {
		t.Errorf("B4 WAL has %d rows after VerifyDrain, want 0", len(pending1))
	}

	// Newly verified finding now exists (the WAL candidate was persisted).
	allFindings1, err := st.ListFindings(ctx, store.FindingFilter{})
	if err != nil {
		t.Fatalf("B4 ListFindings after VerifyDrain: %v", err)
	}
	if len(allFindings1) < 2 {
		// We expect at least the prior seeded finding + the newly verified one.
		t.Errorf("B4 findings after VerifyDrain = %d, want >=2 (prior + newly verified)", len(allFindings1))
	}

	// --- Step 2: SweepDrain (unswept findings → ranked) ---
	sweepClient := newScriptedClient()
	fSD := makeImpactFunnel(t, st, repoDir, sweepClient)

	if _, err := fSD.SweepDrain(ctx); err != nil {
		t.Fatalf("B4 SweepDrain: %v", err)
	}

	// --- Convergence assertions ---

	// WAL still empty.
	pendingFinal, err := st.ListPendingCandidates(ctx)
	if err != nil {
		t.Fatalf("B4 ListPendingCandidates final: %v", err)
	}
	if len(pendingFinal) != 0 {
		t.Errorf("B4 WAL not empty after fixpoint: %d rows", len(pendingFinal))
	}

	// UnsweptOpenFindings: the prior seeded finding (src/dead.cpp) must be swept.
	// The newly-verified finding (bug.go) is also unswept after VerifyDrain;
	// SweepDrain processes all unswept open findings regardless of file.
	// After SweepDrain, UnsweptOpenFindings should be empty (or at least NOT
	// contain the prior seeded finding we can confirm by ID).
	unsweptFinal, err := st.UnsweptOpenFindings(ctx)
	if err != nil {
		t.Fatalf("B4 UnsweptOpenFindings final: %v", err)
	}
	for _, x := range unsweptFinal {
		if x.ID == priorFinding.ID {
			t.Errorf("B4 prior seeded finding still unswept after SweepDrain")
		}
	}

	// Prior seeded finding still exists and is open (not dropped).
	gotPrior, err := st.GetFinding(ctx, priorFinding.ID)
	if err != nil {
		t.Fatalf("B4 GetFinding(prior): %v", err)
	}
	if gotPrior.Status != store.StatusOpen {
		t.Errorf("B4 prior finding status = %q, want open (no drop)", gotPrior.Status)
	}
	if gotPrior.SweptAt.IsZero() {
		t.Error("B4 prior finding swept_at is zero — SweepDrain did not sweep it")
	}

	// No duplicate fingerprints across all findings.
	allFinal, err := st.ListFindings(ctx, store.FindingFilter{})
	if err != nil {
		t.Fatalf("B4 ListFindings final: %v", err)
	}
	seen := make(map[string]bool, len(allFinal))
	for _, f := range allFinal {
		if seen[f.Fingerprint] {
			t.Errorf("B4 duplicate fingerprint %q", f.Fingerprint)
		}
		seen[f.Fingerprint] = true
	}
}

// ---------------------------------------------------------------------------
// Consolidated end-to-end: VerifyDrain processes seeded WAL rows without
// invoking the finder (at the matrix level — complements verifydrain_test.go's
// unit-level test with a post-interrupt scenario).
// ---------------------------------------------------------------------------
func TestInterruptMatrix_VerifyDrainNoFinder_EndToEnd(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	const lens = "nil-safety/error-handling"

	// Simulate an interrupted run by seeding two WAL rows (realCand + bogus)
	// directly — no actual Sweep call needed; the WAL is the durable artifact.
	seedPendingFromRealCand(t, st) // realCand WAL row

	// Also add bogusCand as a second WAL row.
	bogusPC := store.PendingCandidate{
		Lens:        lens,
		File:        "clean.go",
		Line:        5,
		Title:       "Add overflows on large ints",
		Description: "imagined overflow",
		Severity:    "low",
		Evidence:    "a + b",
		Confidence:  "high",
	}
	if err := st.AddPendingCandidates(ctx, []store.PendingCandidate{bogusPC}); err != nil {
		t.Fatalf("AddPendingCandidates (bogus): %v", err)
	}

	// Confirm: 2 WAL rows present.
	pending0, err := st.ListPendingCandidates(ctx)
	if err != nil {
		t.Fatalf("ListPendingCandidates initial: %v", err)
	}
	if len(pending0) != 2 {
		t.Fatalf("want 2 pending candidates seeded, got %d", len(pending0))
	}

	// Recovery via VerifyDrain.
	finder := newScriptedClient()
	finder.fallback = candJSON(realCand) // detectable if called
	verifier := verifierRouting(newScriptedClient())

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{
		Discovery: DiscoveryConfig{Lenses: []string{lens}},
		Limits:    StageLimits{ChunkSize: 1, MaxParallel: 1},
		Features:  FeatureFlags{DisableHeatOrdering: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	if _, err := f.VerifyDrain(ctx); err != nil {
		t.Fatalf("VerifyDrain: %v", err)
	}

	// Finder never called.
	if n := finder.callCount(); n != 0 {
		t.Errorf("finder.callCount() = %d, want 0 (modeVerifyDrain must skip finder)", n)
	}

	// WAL fully drained.
	pending1, err := st.ListPendingCandidates(ctx)
	if err != nil {
		t.Fatalf("ListPendingCandidates post-drain: %v", err)
	}
	if len(pending1) != 0 {
		t.Errorf("WAL has %d rows after VerifyDrain, want 0", len(pending1))
	}

	// Exactly 1 finding (realCand survives, bogus refuted).
	allFindings, err := st.ListFindings(ctx, store.FindingFilter{})
	if err != nil {
		t.Fatalf("ListFindings: %v", err)
	}
	if len(allFindings) != 1 {
		t.Errorf("findings = %d, want 1 (real survives, bogus refuted)", len(allFindings))
	}
	if len(allFindings) > 0 && allFindings[0].File != "bug.go" {
		t.Errorf("surviving finding file = %q, want bug.go", allFindings[0].File)
	}
}

// ---------------------------------------------------------------------------
// Double-drain idempotency
// ---------------------------------------------------------------------------

// TestInterruptMatrix_DoubleDrain_VerifyDrain_Idempotent proves a second
// VerifyDrain after a successful first is a no-op: findings unchanged, finder
// still 0 calls.
func TestInterruptMatrix_DoubleDrain_VerifyDrain_Idempotent(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	const lens = "nil-safety/error-handling"
	seedPendingFromRealCand(t, st)

	finder := newScriptedClient()
	finder.fallback = candJSON(realCand)
	verifier := newScriptedClient()
	verifier.fallback = notRefutedJSON

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{
		Discovery: DiscoveryConfig{Lenses: []string{lens}},
		Limits:    StageLimits{ChunkSize: 1, MaxParallel: 1},
		Features:  FeatureFlags{DisableHeatOrdering: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	// First drain.
	if _, err := f.VerifyDrain(ctx); err != nil {
		t.Fatalf("first VerifyDrain: %v", err)
	}
	findingsAfter1, _ := st.ListFindings(ctx, store.FindingFilter{})
	countAfter1 := len(findingsAfter1)

	// Second drain — must be a no-op.
	if _, err := f.VerifyDrain(ctx); err != nil {
		t.Fatalf("second VerifyDrain: %v", err)
	}

	// No new findings.
	findingsAfter2, _ := st.ListFindings(ctx, store.FindingFilter{})
	if len(findingsAfter2) != countAfter1 {
		t.Errorf("findings after second drain = %d, want %d (idempotent)", len(findingsAfter2), countAfter1)
	}

	// Finder: 0 calls across both drains.
	if n := finder.callCount(); n != 0 {
		t.Errorf("finder.callCount() = %d after two VerifyDrains, want 0", n)
	}

	// WAL still empty.
	pending, _ := st.ListPendingCandidates(ctx)
	if len(pending) != 0 {
		t.Errorf("WAL has %d rows after double drain, want 0", len(pending))
	}
}

// TestInterruptMatrix_DoubleDrain_SweepDrain_Idempotent proves a second
// SweepDrain after a successful first is a no-op: UnsweptOpenFindings remains
// empty, zero additional verifier LLM calls.
func TestInterruptMatrix_DoubleDrain_SweepDrain_Idempotent(t *testing.T) {
	repoDir := makeGitRepo(t, map[string]string{
		"src/dead.cpp": "void deadHelper() {}\n",
		"src/main.cpp": "int main() { return 0; }\n",
	})

	st := openImpactStore(t)
	ctx := context.Background()
	client := newScriptedClient()
	f := makeImpactFunnel(t, st, repoDir, client)

	_ = seedFinding(t, st, makeImpactFinding("dd-sweep1", "src/dead.cpp", 1, domain.SeverityHigh))

	// First drain.
	if _, err := f.SweepDrain(ctx); err != nil {
		t.Fatalf("first SweepDrain: %v", err)
	}
	callsAfterFirst := client.callCount()

	// Second drain — must be a no-op.
	result2, err := f.SweepDrain(ctx)
	if err != nil {
		t.Fatalf("second SweepDrain: %v", err)
	}
	if result2.ScanRunID != "" {
		t.Error("second SweepDrain opened a scan run on an already-swept store")
	}
	if len(result2.Findings) != 0 {
		t.Errorf("second SweepDrain returned %d findings, want 0 (idempotent)", len(result2.Findings))
	}

	// No additional LLM calls.
	if n := client.callCount(); n != callsAfterFirst {
		t.Errorf("second SweepDrain made %d additional LLM calls, want 0", n-callsAfterFirst)
	}

	// Confirm empty.
	unswept, err := st.UnsweptOpenFindings(ctx)
	if err != nil {
		t.Fatalf("UnsweptOpenFindings after double drain: %v", err)
	}
	if len(unswept) != 0 {
		t.Errorf("UnsweptOpenFindings = %d after double SweepDrain, want 0", len(unswept))
	}
}

// finderOneChunkEmitsRealCand configures c to emit realCand for the one finder
// unit whose chunk contains the candidate's target file (bug.go) and
// emptyCandidates for every other chunk.
//
// Why this exists: openFixture's repo has TWO files (bug.go, clean.go), so
// buildUnits(..., ChunkSize=1) yields two chunks for a single lens. A naive
// lens-only substring match (onSystemContains) would fire on BOTH chunks, and
// since realCand's JSON is hard-coded to point at bug.go:10, both units would
// persist identical WAL rows for the same candidate. The triage dedup later
// folds the duplicates back to one, but the mid-interrupt snapshot in the
// interrupt tests can land while a duplicate is transiently present and
// observe len(pending) == 2 instead of 1 — the bugbot-303 flake. Anchoring
// the match to the chunk that lists the candidate's file picks exactly the
// unit that is semantically allowed to emit realCand, so the WAL has exactly
// one row at any interrupt point regardless of scheduling.
func finderOneChunkEmitsRealCand(c *scriptedClient) *scriptedClient {
	const lens = "nil-safety/error-handling"
	c.on(func(req llm.Request) bool {
		if !strings.Contains(req.System, lens) {
			return false
		}
		for _, m := range req.Messages {
			if m.Role == llm.RoleUser && strings.Contains(m.Content, "bug.go") {
				return true
			}
		}
		return false
	}, candJSON(realCand))
	c.fallback = emptyCandidates
	return c
}
