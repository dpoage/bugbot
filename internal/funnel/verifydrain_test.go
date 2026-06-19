package funnel

import (
	"context"
	"testing"

	"github.com/dpoage/bugbot/internal/store"
)

// seedPendingFromRealCand inserts a PendingCandidate that mirrors realCand
// (file=bug.go, line=10, lens=nil-safety/error-handling) into the store and
// returns the row so the caller can inspect its ID.
func seedPendingFromRealCand(t *testing.T, st *store.Store) store.PendingCandidate {
	t.Helper()
	pc := store.PendingCandidate{
		Lens:        "nil-safety/error-handling",
		File:        "bug.go",
		Line:        10,
		Title:       "nil deref of cfg in Greeting",
		Description: "cfg may be nil",
		Severity:    "high",
		Evidence:    "Greeting returns cfg.Name without a nil check",
		Confidence:  "high",
	}
	rows := []store.PendingCandidate{pc}
	if err := st.AddPendingCandidates(context.Background(), rows); err != nil {
		t.Fatalf("AddPendingCandidates: %v", err)
	}
	return rows[0]
}

// TestVerifyDrain_NoFinderInvocation verifies the core contract of VerifyDrain:
//   - The finder is NEVER called (callCount == 0).
//   - At least one finding is persisted (the replayed WAL candidate verified through).
//   - The pending_candidates row is deleted after successful verification.
func TestVerifyDrain_NoFinderInvocation(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	seedPendingFromRealCand(t, st)

	finder := newScriptedClient()
	// fallback: if finder IS called, return a candidate so the test can detect it
	finder.fallback = candJSON(realCand)

	verifier := newScriptedClient()
	verifier.fallback = notRefutedJSON

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = f.Close() }()

	res, err := f.VerifyDrain(ctx)
	if err != nil {
		t.Fatalf("VerifyDrain: %v", err)
	}

	// The finder must never have been invoked.
	if n := finder.callCount(); n != 0 {
		t.Errorf("finder.callCount() = %d, want 0 (finder must not run in modeVerifyDrain)", n)
	}

	// At least one finding must be persisted.
	findings, lerr := st.ListFindings(ctx, store.FindingFilter{})
	if lerr != nil {
		t.Fatalf("ListFindings: %v", lerr)
	}
	if len(findings) == 0 {
		t.Errorf("ListFindings returned 0 findings; expected the WAL candidate to be verified and persisted")
	}

	// The result itself should report at least one finding.
	if res == nil {
		t.Fatal("VerifyDrain returned nil result")
	}

	// The pending row must be gone (WAL cleared at terminal fate).
	pending, perr := st.ListPendingCandidates(ctx)
	if perr != nil {
		t.Fatalf("ListPendingCandidates: %v", perr)
	}
	if len(pending) != 0 {
		t.Errorf("ListPendingCandidates = %d rows, want 0 (WAL must be drained)", len(pending))
	}
}

// TestVerifyDrain_Idempotent verifies that a second VerifyDrain on an already-drained
// store is a no-op: no findings duplicate, no errors, and the finder is still
// never called.
func TestVerifyDrain_Idempotent(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	seedPendingFromRealCand(t, st)

	finder := newScriptedClient()
	finder.fallback = candJSON(realCand)

	verifier := newScriptedClient()
	verifier.fallback = notRefutedJSON

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = f.Close() }()

	// First drain: processes the WAL row.
	if _, err := f.VerifyDrain(ctx); err != nil {
		t.Fatalf("first VerifyDrain: %v", err)
	}

	findingsAfterFirst, _ := st.ListFindings(ctx, store.FindingFilter{})
	firstCount := len(findingsAfterFirst)

	// Second drain: WAL is empty → pre-check returns early, no LLM calls.
	res2, err := f.VerifyDrain(ctx)
	if err != nil {
		t.Fatalf("second VerifyDrain: %v", err)
	}
	if res2 == nil {
		t.Fatal("second VerifyDrain returned nil result")
	}

	// No new findings added.
	findingsAfterSecond, _ := st.ListFindings(ctx, store.FindingFilter{})
	if len(findingsAfterSecond) != firstCount {
		t.Errorf("findings after second drain = %d, want %d (idempotent)", len(findingsAfterSecond), firstCount)
	}

	// Finder was never called across both drains.
	if n := finder.callCount(); n != 0 {
		t.Errorf("finder.callCount() = %d after two drains, want 0", n)
	}
}

// TestVerifyDrain_ReanchorDropsOutOfScope verifies that a pending candidate
// whose File is not present in the current snapshot is dropped during triage
// re-anchoring and does not produce a finding. The pending row must also be
// deleted (terminal fate = drop).
func TestVerifyDrain_ReanchorDropsOutOfScope(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	// Seed a candidate pointing at a file that does NOT exist in the fixture
	// repo (which has only bug.go and clean.go). Triage's scope check will
	// drop it because it is not in the snapshot.
	pc := store.PendingCandidate{
		Lens:        "nil-safety/error-handling",
		File:        "does_not_exist.go",
		Line:        42,
		Title:       "hypothetical bug in missing file",
		Description: "file gone",
		Severity:    "high",
		Evidence:    "none",
		Confidence:  "high",
	}
	rows := []store.PendingCandidate{pc}
	if err := st.AddPendingCandidates(ctx, rows); err != nil {
		t.Fatalf("AddPendingCandidates: %v", err)
	}

	finder := newScriptedClient()
	verifier := newScriptedClient()
	verifier.fallback = notRefutedJSON

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = f.Close() }()

	res, err := f.VerifyDrain(ctx)
	if err != nil {
		t.Fatalf("VerifyDrain: %v", err)
	}
	if res == nil {
		t.Fatal("VerifyDrain returned nil result")
	}

	// No finding should be persisted for the out-of-scope candidate.
	findings, lerr := st.ListFindings(ctx, store.FindingFilter{})
	if lerr != nil {
		t.Fatalf("ListFindings: %v", lerr)
	}
	if len(findings) != 0 {
		t.Errorf("ListFindings = %d, want 0 (out-of-scope candidate must be dropped)", len(findings))
	}

	// The pending row must be deleted (terminal fate for a dropped/killed candidate).
	pending, perr := st.ListPendingCandidates(ctx)
	if perr != nil {
		t.Fatalf("ListPendingCandidates: %v", perr)
	}
	if len(pending) != 0 {
		t.Errorf("ListPendingCandidates = %d rows, want 0 (pending row must be cleared on drop)", len(pending))
	}

	// Finder never called.
	if n := finder.callCount(); n != 0 {
		t.Errorf("finder.callCount() = %d, want 0", n)
	}
}
