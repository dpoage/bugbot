package funnel

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

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
		Lenses:              []string{lens},
		ChunkSize:           1, // one file per unit → 5 units for 5 files
		MaxParallel:         1, // sequential: only one goroutine active at a time
		DisableHeatOrdering: true,
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
// Mechanics: 1 file, 1 lens, ChunkSize=1 → exactly one finder unit → one
// candidate. The finder is ungated (it completes and persists the candidate);
// the VERIFIER is gated to block on its first refuter call. A watchdog cancels
// the sweep once the verifier blocks, so the candidate is in-flight in verify
// (forwarded by triage, refuter blocked) when the interrupt lands — its WAL row
// survives. A second run on the SAME store, with an empty finder and an allowing
// verifier, replays the row and verifies it.
func TestSweep_InterruptThenResume_PendingCandidates(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t) // shared store + repo across both runs

	const lens = "nil-safety/error-handling"

	// --- Phase 1: interrupt mid-verification ---
	finder1 := newScriptedClient().onSystemContains(lens, candJSON(realCand))
	finder1.fallback = emptyCandidates

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
		Lenses:              []string{lens},
		ChunkSize:           1,
		MaxParallel:         1,
		DisableHeatOrdering: true,
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
		Lenses:              []string{lens},
		ChunkSize:           1,
		MaxParallel:         1,
		DisableHeatOrdering: true,
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
	if got.Tier != tierVerified {
		t.Errorf("finding tier = %d, want %d (verified)", got.Tier, tierVerified)
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
