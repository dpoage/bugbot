package funnel

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/store"
)

// fakeReproHook is a thread-safe fake repro hook for testing.
// It records invocations and optionally returns an error.
type fakeReproHook struct {
	mu        sync.Mutex
	invoked   []store.Finding // findings the hook was called with, in order
	returnErr error           // error to return from the hook
}

func newFakeReproHook() *fakeReproHook {
	return &fakeReproHook{}
}

func (h *fakeReproHook) hook(_ context.Context, _ string, finding store.Finding) error {
	h.mu.Lock()
	h.invoked = append(h.invoked, finding)
	h.mu.Unlock()
	return h.returnErr
}

func (h *fakeReproHook) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.invoked)
}

func (h *fakeReproHook) findings() []store.Finding {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]store.Finding, len(h.invoked))
	copy(out, h.invoked)
	return out
}

// TestRepro_InRun_HookFired verifies that the repro hook is invoked for
// a Tier-2 finding that survives verification during a sweep. This is the
// basic wiring test: does the hook fire at all?
func TestRepro_InRun_HookFired(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	hook := newFakeReproHook()

	finder := finderOnNilLens(newScriptedClient())
	verifier := verifierRouting(newScriptedClient())

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{
		Repro: hook.hook,
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Exactly one Tier-2 finding.
	if len(res.Findings) != 1 {
		t.Fatalf("want 1 finding, got %d", len(res.Findings))
	}

	// Hook must have been invoked exactly once for the surviving finding.
	if got := hook.count(); got != 1 {
		t.Errorf("hook invocation count = %d, want 1", got)
	}
	invoked := hook.findings()
	if len(invoked) > 0 && invoked[0].Fingerprint != res.Findings[0].Fingerprint {
		t.Errorf("hook invoked for wrong finding: got fingerprint %q, want %q",
			invoked[0].Fingerprint, res.Findings[0].Fingerprint)
	}
}

// TestRepro_InRun_Streaming verifies acceptance criterion #1:
// the repro hook fires for an early Tier-2 finding BEFORE a blocked
// later finder unit releases (streaming repro — doesn't wait for all discovery).
//
// Setup: 3-chunk fixture with one fast lens that returns a real candidate
// immediately, and one slow lens (gating client) that blocks until the
// repro hook is seen. If repro waited for all discovery to finish, the
// hook would never fire before the block — deadlock.
func TestRepro_InRun_Streaming(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, repo := openFixture(t)

	// hookFiredCh is closed once the hook fires for the early finding.
	hookFiredCh := make(chan struct{})
	var hookFiredOnce sync.Once

	hook := func(hCtx context.Context, _ string, finding store.Finding) error {
		hookFiredOnce.Do(func() { close(hookFiredCh) })
		return nil
	}

	// slowFinder blocks until hookFiredCh is closed (repro hook fired).
	slowFinder := &blockingClient{
		inner: newScriptedClient(), // returns empty for its lens
		onCallStart: func() {
			select {
			case <-hookFiredCh:
				// Good: repro hook fired before we unblocked.
			case <-ctx.Done():
				// Test timeout: the hook never fired before discovery finished.
			}
		},
	}

	fastCand := candJSON(realCand)
	combinedFinder := &dispatchClient{
		routes: []dispatchRoute{
			{sub: "nil-safety/error-handling", client: newScriptedClient().onSystemContains("nil-safety/error-handling", fastCand)},
			{sub: "resource-leaks", client: slowFinder},
		},
		fallback: newScriptedClient(),
	}

	verifier := newScriptedClient()
	verifier.fallback = notRefutedJSON

	f, err := New(RoleClients{Finder: combinedFinder, Verifier: verifier}, st, repo, Options{
		Discovery: DiscoveryConfig{Lenses: []string{"nil-safety/error-handling", "resource-leaks"}},
		Limits:    StageLimits{MaxParallel: 3}, // enough for finder + verifier + repro concurrently
		Repro:     hook,
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Findings) != 1 {
		t.Errorf("want 1 finding, got %d", len(res.Findings))
	}

	// hookFiredCh must have been closed (slowFinder unblocked only after it was).
	select {
	case <-hookFiredCh:
		// Correct: hook fired during discovery.
	default:
		t.Error("repro hook did not fire before slow finder unblocked — not streaming")
	}
}

// TestRepro_NeverDisplace verifies acceptance criterion #2:
// with pool size 1, a repro hook waiting for a slot while a verifier
// candidate also waits → the verifier acquires first (high > idle priority).
//
// Observable via invocation ordering: we ensure the verifier call happens
// before the repro hook by checking the slot pool directly.
func TestRepro_NeverDisplace(t *testing.T) {
	// Use slotPool directly to test the priority assertion without a full funnel run.
	// Pool size 1: one slot total.
	p := newSlotPool(1)

	// Drain the one free slot (simulating an active slot holder).
	if err := p.acquire(context.Background(), slotLow); err != nil {
		t.Fatal(err)
	}

	var orderMu sync.Mutex
	var order []string

	type entry struct {
		name  string
		class slotClass
	}
	// Enqueue in order: repro (idle) first, then verifier (high).
	seq := []entry{
		{"repro", slotIdle},
		{"verifier", slotHigh},
	}

	var wg sync.WaitGroup
	startChs := make([]chan struct{}, len(seq))
	queuedChs := make([]chan struct{}, len(seq))
	for i := range seq {
		startChs[i] = make(chan struct{})
		queuedChs[i] = make(chan struct{})
	}

	for i, e := range seq {
		wg.Add(1)
		i, e := i, e
		go func() {
			defer wg.Done()
			<-startChs[i]
			close(queuedChs[i])
			if err := p.acquire(context.Background(), e.class); err != nil {
				t.Errorf("waiter %q: acquire failed: %v", e.name, err)
				return
			}
			orderMu.Lock()
			order = append(order, e.name)
			orderMu.Unlock()
			p.release()
		}()
	}

	queuedLen := func() int {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.waitHigh) + len(p.waitLow) + len(p.waitIdle)
	}
	for i := range seq {
		close(startChs[i])
		<-queuedChs[i]
		for queuedLen() < i+1 {
			// spin until the waiter is queued
		}
	}

	p.release()
	wg.Wait()

	orderMu.Lock()
	got := order
	orderMu.Unlock()

	if len(got) != 2 {
		t.Fatalf("want 2 completions, got %d: %v", len(got), got)
	}
	// verifier must precede repro (high > idle).
	if got[0] != "verifier" || got[1] != "repro" {
		t.Errorf("priority violated: order=%v; want [verifier repro] (repro must not displace verifier)", got)
	}
}

// TestRepro_NoDoubleAttempt verifies acceptance criterion #3:
// when a finding is promoted in-run (ReproPath set), the claim check in
// runReproAttempt prevents a second attempt. We test this by:
// 1. Running a sweep with a hook that promotes the finding.
// 2. Manually calling runReproAttempt on the same finding again.
// 3. Asserting the hook did NOT fire a second time (claim check returned early).
func TestRepro_NoDoubleAttempt(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	var invCount atomic.Int32
	// Hook that simulates promotion by setting ReproPath.
	var capturedFinding store.Finding
	hook := func(hCtx context.Context, _ string, finding store.Finding) error {
		invCount.Add(1)
		capturedFinding = finding
		// Promote the finding.
		current, err := st.GetFindingByFingerprint(hCtx, finding.Fingerprint)
		if err != nil {
			return err
		}
		current.ReproPath = "/fake/repro/path"
		_, err = st.UpsertFinding(hCtx, current)
		return err
	}

	finder := finderOnNilLens(newScriptedClient())
	verifier := verifierRouting(newScriptedClient())

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{
		Repro: hook,
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}

	firstCount := invCount.Load()
	if firstCount == 0 {
		t.Fatal("hook was never called — no Tier-2 finding to test")
	}
	if len(res.Findings) == 0 {
		t.Fatal("no findings returned by sweep")
	}

	// Now call runReproAttempt directly on the promoted finding.
	// The claim check (GetFindingByFingerprint → ReproPath != "") must prevent
	// the hook from firing a second time.
	f.runReproAttempt(ctx, capturedFinding, res.ScanRunID)

	secondCount := invCount.Load()
	if secondCount != firstCount {
		t.Errorf("hook invoked %d times after manual runReproAttempt on promoted finding; want %d (claim check must prevent re-attempt)",
			secondCount, firstCount)
	}
}

// TestRepro_AgentUnits verifies that a repro attempt produces one
// role='reproducer' agent_units row with correct fields.
func TestRepro_AgentUnits(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	// Hook that simulates a successful promotion.
	hook := func(hCtx context.Context, _ string, finding store.Finding) error {
		// Simulate promotion: set ReproPath.
		current, err := st.GetFindingByFingerprint(hCtx, finding.Fingerprint)
		if err != nil {
			return err
		}
		current.ReproPath = "/fake/repro"
		_, err = st.UpsertFinding(hCtx, current)
		return err
	}

	finder := finderOnNilLens(newScriptedClient())
	verifier := verifierRouting(newScriptedClient())

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{
		Repro: hook,
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Findings) == 0 {
		t.Fatal("no findings — cannot test reproducer agent_units row")
	}

	units, err := st.ListAgentUnits(ctx, res.ScanRunID)
	if err != nil {
		t.Fatalf("ListAgentUnits: %v", err)
	}

	var reproRows []store.AgentUnit
	for _, u := range units {
		if u.Role == "reproducer" {
			reproRows = append(reproRows, u)
		}
	}

	if len(reproRows) == 0 {
		t.Fatal("expected at least one reproducer agent_units row; found none")
	}

	row := reproRows[0]

	// Status must be a known vocabulary word.
	switch row.Status {
	case "reproduced", "exhausted", "infra_error":
		// valid
	default:
		t.Errorf("reproducer row: unexpected status %q", row.Status)
	}

	// Lens and file must be non-empty (derived from the finding).
	if row.Lens == "" {
		t.Error("reproducer row: lens is empty")
	}
	if len(row.Files) == 0 || row.Files[0] == "" {
		t.Error("reproducer row: files_json is empty")
	}

	// StartedAt and FinishedAt must be non-zero (the attempt ran).
	if row.StartedAt.IsZero() {
		t.Error("reproducer row: started_at is zero")
	}
	if row.FinishedAt.IsZero() {
		t.Error("reproducer row: finished_at is zero")
	}

	// ScanRunID must match the run (not empty for in-run attempts).
	if row.ScanRunID != res.ScanRunID {
		t.Errorf("reproducer row: scan_run_id = %q, want %q", row.ScanRunID, res.ScanRunID)
	}

	// Detail must be non-empty (we record elapsed_ms at minimum).
	if row.Detail == "" {
		t.Error("reproducer row: detail is empty")
	}

	// Candidates = 1 when reproduced (promotion succeeded), 0 otherwise.
	if row.Status == "reproduced" && row.Candidates != 1 {
		t.Errorf("reproduced row: candidates = %d, want 1", row.Candidates)
	}
	if row.Status != "reproduced" && row.Candidates != 0 {
		t.Errorf("non-reproduced row: candidates = %d, want 0", row.Candidates)
	}
}

// TestRepro_HookNilSkipsRepro verifies that when Repro is nil, no repro logic
// runs and the funnel behaves identically to before this feature was added.
func TestRepro_HookNilSkipsRepro(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	finder := finderOnNilLens(newScriptedClient())
	verifier := verifierRouting(newScriptedClient())

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{
		// Repro: nil — default
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Behavior unchanged: 1 verified finding.
	if len(res.Findings) != 1 {
		t.Fatalf("want 1 finding, got %d", len(res.Findings))
	}

	// No reproducer agent_units rows (hook was nil).
	units, err := st.ListAgentUnits(ctx, res.ScanRunID)
	if err != nil {
		t.Fatalf("ListAgentUnits: %v", err)
	}
	for _, u := range units {
		if u.Role == "reproducer" {
			t.Errorf("found unexpected reproducer agent_units row when hook is nil: %+v", u)
		}
	}
}

// TestRepro_HookErrorBestEffort verifies that a hook that returns an error
// does NOT abort the scan — the result is still returned and findings are
// persisted, with the error captured in the agent_units row.
func TestRepro_HookErrorBestEffort(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	hook := func(ctx context.Context, _ string, finding store.Finding) error {
		return context.DeadlineExceeded // simulate an infra error
	}

	finder := finderOnNilLens(newScriptedClient())
	verifier := verifierRouting(newScriptedClient())

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{
		Repro: hook,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Sweep must not return an error even though the hook fails.
	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep returned error despite hook failure (should be best-effort): %v", err)
	}

	// Finding still persisted.
	if len(res.Findings) != 1 {
		t.Errorf("want 1 finding, got %d", len(res.Findings))
	}

	// Agent units row for reproducer should have status=infra_error.
	units, err := st.ListAgentUnits(ctx, res.ScanRunID)
	if err != nil {
		t.Fatalf("ListAgentUnits: %v", err)
	}
	var reproRows []store.AgentUnit
	for _, u := range units {
		if u.Role == "reproducer" {
			reproRows = append(reproRows, u)
		}
	}
	if len(reproRows) == 0 {
		t.Fatal("expected reproducer agent_units row for error case")
	}
	if reproRows[0].Status != "infra_error" {
		t.Errorf("status = %q, want infra_error", reproRows[0].Status)
	}
}

// TestReproQueue_OverflowExactlyOnce tests the defensive overflow machinery
// DIRECTLY (the spawn-only consumer drains the channel faster than verify can
// fill it in practice, so integration timing cannot reach this path honestly):
// with no consumer attached, enqueues beyond the buffer land in the overflow
// slice; the union of channel + overflow holds every finding exactly once; a
// second drain returns nothing.
func TestReproQueue_OverflowExactlyOnce(t *testing.T) {
	q := newReproQueue(2)
	for i := 0; i < 5; i++ {
		q.enqueue(store.Finding{Fingerprint: itoa(i)})
	}
	got := make(map[string]int)
	for len(q.ch) > 0 {
		got[(<-q.ch).Fingerprint]++
	}
	chCount := len(got)
	for _, f := range q.drainOverflow() {
		got[f.Fingerprint]++
	}
	if chCount != 2 {
		t.Errorf("channel held %d findings, want 2 (buffer size)", chCount)
	}
	if len(got) != 5 {
		t.Fatalf("delivered %d distinct findings, want 5: %v", len(got), got)
	}
	for fp, n := range got {
		if n != 1 {
			t.Errorf("finding %s delivered %d times, want exactly once", fp, n)
		}
	}
	if extra := q.drainOverflow(); len(extra) != 0 {
		t.Errorf("second drain returned %d findings, want 0 (exactly-once drain)", len(extra))
	}
}

// TestRepro_Burst_ExactlyOncePerFinding drives 12 distinct survived findings
// through the full streaming pipeline and asserts the in-run hook fires
// exactly once per finding, whichever delivery path each took.
func TestRepro_Burst_ExactlyOncePerFinding(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	const n = 12
	cands := make([]string, n)
	for i := 0; i < n; i++ {
		// Distinct lines far beyond the merge window and distinct descriptions
		// so triage clustering forwards every candidate as its own primary.
		cands[i] = fmt.Sprintf(`{"file": "bug.go", "line": %d, "title": "burst bug %d",
			"description": "unique defect token%d alpha%d beta%d", "severity": "high",
			"evidence": "line", "confidence": "high"}`, 100+i*(DefaultMergeWindow*3), i, i, i, i)
	}
	finder := newScriptedClient().onSystemContains("nil-safety/error-handling", candJSON(cands...))
	verifier := newScriptedClient()
	verifier.fallback = notRefutedJSON

	hook := newFakeReproHook()
	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{
		Discovery: DiscoveryConfig{Lenses: []string{"nil-safety/error-handling"}},
		Limits:    StageLimits{MaxParallel: 4},
		Repro:     hook.hook,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	seen := make(map[string]int)
	for _, fi := range hook.findings() {
		seen[fi.Fingerprint]++
	}
	if len(seen) != n {
		t.Fatalf("hook saw %d distinct findings, want %d", len(seen), n)
	}
	for fp, c := range seen {
		if c != 1 {
			t.Errorf("finding %s attempted %d times, want exactly once", fp, c)
		}
	}
}
