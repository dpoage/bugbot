package repro

import (
	"context"
	"testing"

	"github.com/dpoage/bugbot/internal/sandbox"
	"github.com/dpoage/bugbot/internal/store"
)

// TestPromoteOne_ExitZeroIncrementsContradiction verifies that an exit-zero
// outcome (test ran, bug did not manifest) increments exit_zero_count and
// the finding becomes repro-contradicted after >= threshold such attempts.
func TestPromoteOne_ExitZeroIncrementsContradiction(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	finding := seedFinding(t, st)
	repoDir := newRepoDir(t)

	// exit 0: sandbox ran, test passed (bug did not manifest).
	client := newScriptedClient(
		planBody(t, goodPlan()),
		planBody(t, goodPlan()),
	)
	sb := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 0, Stdout: "ok"}})

	r, err := New(client, sb, repoDir, Options{ArtifactDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	// First attempt: exit zero — not yet contradicted.
	outcome, err := promoteOne(ctx, r, st, finding)
	if err != nil {
		t.Fatalf("promoteOne #1: %v", err)
	}
	if !outcome.ExitZero {
		t.Error("outcome.ExitZero = false for exit-0 sandbox result, want true")
	}
	if outcome.Promoted {
		t.Error("outcome.Promoted = true for exit-0 sandbox result, want false")
	}

	f1, _ := st.GetFindingByFingerprint(ctx, finding.Fingerprint)
	if f1.ReproContradicted {
		t.Errorf("ReproContradicted = true after 1 exit-zero attempt, want false (threshold = %d)", store.ReproContradictionThreshold)
	}

	// Second attempt: exit zero — now contradicted.
	// Re-seed so ClaimReproAttempt can claim again (queue is 'done' from first attempt;
	// re-enqueue to reset for a second dispatch cycle).
	if _, err := st.EnqueueRepro(ctx, finding.Fingerprint); err != nil {
		// Idempotent: if already in a terminal state, re-enqueue doesn't error but is a no-op.
		// Instead, directly call RecordExitZeroAttempt to simulate a second exit-zero run.
		_ = err
	}
	if err := st.RecordExitZeroAttempt(ctx, finding.Fingerprint); err != nil {
		t.Fatalf("RecordExitZeroAttempt (direct, second pass): %v", err)
	}

	f2, _ := st.GetFindingByFingerprint(ctx, finding.Fingerprint)
	if !f2.ReproContradicted {
		t.Errorf("ReproContradicted = false after %d exit-zero attempts, want true", store.ReproContradictionThreshold)
	}
}

// TestPromoteOne_InfraErrorDoesNotIncrementContradiction verifies that an
// infrastructure error (agent/sandbox failure) does NOT increment exit_zero_count.
func TestPromoteOne_InfraErrorDoesNotIncrementContradiction(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	finding := seedFinding(t, st)
	repoDir := newRepoDir(t)

	// goodPlan but sandbox exits with 125 (container/shell launch failure).
	// interpret() classifies this as environment_error (not exit_zero).
	client := newScriptedClient(planBody(t, goodPlan()))
	sb := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 125, Stdout: "container not found"}})

	r, err := New(client, sb, repoDir, Options{ArtifactDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	outcome, err := promoteOne(ctx, r, st, finding)
	if err != nil {
		t.Fatalf("promoteOne: %v", err)
	}
	if outcome.ExitZero {
		t.Error("outcome.ExitZero = true for environment_error (exit 125), want false")
	}
	if outcome.Promoted {
		t.Error("outcome.Promoted = true for environment_error, want false")
	}

	f, _ := st.GetFindingByFingerprint(ctx, finding.Fingerprint)
	if f.ReproContradicted {
		t.Error("ReproContradicted = true after environment_error, want false (only exit_zero counts)")
	}
}

// TestPromoteOne_SuccessfulReproClearsContradiction verifies that a successful
// repro (Promoted=true) zeroes exit_zero_count so a previously contradicted
// finding is no longer contradicted.
func TestPromoteOne_SuccessfulReproClearsContradiction(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	finding := seedFinding(t, st)
	repoDir := newRepoDir(t)

	// Pre-load 2 exit-zero counts directly (simulates prior dispatch cycles).
	if _, err := st.EnqueueRepro(ctx, finding.Fingerprint); err != nil {
		t.Fatal(err)
	}
	_ = st.RecordExitZeroAttempt(ctx, finding.Fingerprint)
	_ = st.RecordExitZeroAttempt(ctx, finding.Fingerprint)

	fPre, _ := st.GetFindingByFingerprint(ctx, finding.Fingerprint)
	if !fPre.ReproContradicted {
		t.Fatal("precondition: finding should be contradicted before promotion")
	}

	// Now promote successfully: exit 1 with test-failure output.
	client := newScriptedClient(planBody(t, goodPlan()))
	sb := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{
		ExitCode: 1,
		Stdout:   "--- FAIL: TestBug\n    bug_test.go:1: boom\nFAIL",
	}})
	r, err := New(client, sb, repoDir, Options{ArtifactDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	outcome, err := promoteOne(ctx, r, st, finding)
	if err != nil {
		t.Fatalf("promoteOne: %v", err)
	}
	if !outcome.Promoted {
		t.Fatalf("expected Promoted=true, outcome.Reason=%q", outcome.Reason)
	}
	if outcome.ExitZero {
		t.Error("ExitZero must be false for a successful promotion")
	}

	// ReproContradicted must be cleared.
	fPost, _ := st.GetFindingByFingerprint(ctx, finding.Fingerprint)
	if fPost.ReproContradicted {
		t.Error("ReproContradicted = true after successful promotion — ZeroExitZeroCount must have been called")
	}
}

// TestPromoteOne_BuildErrorDoesNotCountTowardContradiction verifies that a
// build_error outcome (compile failure in the repro test) does NOT set ExitZero
// and does NOT increment exit_zero_count.
func TestPromoteOne_BuildErrorDoesNotCountTowardContradiction(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	finding := seedFinding(t, st)
	repoDir := newRepoDir(t)

	// exit 2 with a syntax error: interpret() classifies as build_error.
	client := newScriptedClient(planBody(t, goodPlan()))
	sb := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{
		ExitCode: 2,
		Stderr:   "./bug_test.go:3:1: syntax error: unexpected token",
	}})

	r, err := New(client, sb, repoDir, Options{ArtifactDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	outcome, err := promoteOne(ctx, r, st, finding)
	if err != nil {
		t.Fatalf("promoteOne: %v", err)
	}
	if outcome.ExitZero {
		t.Error("outcome.ExitZero = true for build_error outcome, want false")
	}

	f, _ := st.GetFindingByFingerprint(ctx, finding.Fingerprint)
	if f.ReproContradicted {
		t.Error("ReproContradicted = true after build_error, want false (build_error must not count)")
	}
}
