package store

import (
	"context"
	"testing"

	"github.com/dpoage/bugbot/internal/domain"
)

// TestReproContradiction_SignalFromExitZeroAttempts verifies acceptance criterion 1:
// a finding with >= domain.ReproContradictionThreshold exit-zero repro attempts carries
// a visible repro-contradicted signal in its domain.Finding struct (via report list/show
// data path: ListFindings and GetFindingByFingerprint both read ReproContradicted).
func TestReproContradiction_SignalFromExitZeroAttempts(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	f := sampleFinding()
	stored, err := st.UpsertFinding(ctx, f)
	if err != nil {
		t.Fatalf("UpsertFinding: %v", err)
	}

	// Enqueue and perform one exit-zero attempt: not yet contradicted.
	if _, err := st.EnqueueRepro(ctx, stored.Fingerprint); err != nil {
		t.Fatalf("EnqueueRepro: %v", err)
	}
	if err := st.RecordExitZeroAttempt(ctx, stored.Fingerprint); err != nil {
		t.Fatalf("RecordExitZeroAttempt #1: %v", err)
	}

	got, err := st.GetFindingByFingerprint(ctx, stored.Fingerprint)
	if err != nil {
		t.Fatalf("GetFindingByFingerprint after 1 exit-zero: %v", err)
	}
	if got.ReproContradicted {
		t.Errorf("ReproContradicted = true after 1 exit-zero attempt, want false (threshold = %d)", domain.ReproContradictionThreshold)
	}

	// Second exit-zero attempt: now contradicted.
	if err := st.RecordExitZeroAttempt(ctx, stored.Fingerprint); err != nil {
		t.Fatalf("RecordExitZeroAttempt #2: %v", err)
	}

	got2, err := st.GetFindingByFingerprint(ctx, stored.Fingerprint)
	if err != nil {
		t.Fatalf("GetFindingByFingerprint after 2 exit-zeros: %v", err)
	}
	if !got2.ReproContradicted {
		t.Errorf("ReproContradicted = false after %d exit-zero attempts, want true", domain.ReproContradictionThreshold)
	}

	// Also visible via ListFindings.
	all, err := st.ListFindings(ctx, domain.FindingFilter{Status: domain.StatusOpen})
	if err != nil {
		t.Fatalf("ListFindings: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(all))
	}
	if !all[0].ReproContradicted {
		t.Errorf("ListFindings: ReproContradicted = false, want true after %d exit-zeros", domain.ReproContradictionThreshold)
	}
}

// TestReproContradiction_IsReproContradicted_Accessor verifies the scalar
// accessor returns correct values before and after the threshold is crossed.
func TestReproContradiction_IsReproContradicted_Accessor(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	f := sampleFinding()
	stored, _ := st.UpsertFinding(ctx, f)
	_, _ = st.EnqueueRepro(ctx, stored.Fingerprint)

	// No attempts yet: not contradicted.
	contradicted, err := st.IsReproContradicted(ctx, stored.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if contradicted {
		t.Error("IsReproContradicted = true with zero exit-zero attempts, want false")
	}

	// One attempt: still not contradicted.
	_ = st.RecordExitZeroAttempt(ctx, stored.Fingerprint)
	contradicted, _ = st.IsReproContradicted(ctx, stored.Fingerprint)
	if contradicted {
		t.Error("IsReproContradicted = true with 1 exit-zero attempt, want false")
	}

	// Two attempts: contradicted.
	_ = st.RecordExitZeroAttempt(ctx, stored.Fingerprint)
	contradicted, _ = st.IsReproContradicted(ctx, stored.Fingerprint)
	if !contradicted {
		t.Error("IsReproContradicted = false with 2 exit-zero attempts, want true")
	}
}

// TestReproContradiction_InfraErrorDoesNotCount verifies acceptance criterion 3:
// an infrastructure error (RequeueReproAttemptOnInfraError) does NOT increment
// exit_zero_count and does NOT trigger the repro-contradicted signal.
func TestReproContradiction_InfraErrorDoesNotCount(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	f := sampleFinding()
	stored, _ := st.UpsertFinding(ctx, f)

	if _, err := st.EnqueueRepro(ctx, stored.Fingerprint); err != nil {
		t.Fatal(err)
	}
	// Simulate an infrastructure error (not an exit-zero run):
	if _, err := st.ClaimReproAttempt(ctx, stored.Fingerprint); err != nil {
		t.Fatal(err)
	}
	// RequeueReproAttemptOnInfraError: only increments attempt_count, not exit_zero_count.
	if err := st.RequeueReproAttemptOnInfraError(ctx, stored.Fingerprint, "sandbox crashed"); err != nil {
		t.Fatal(err)
	}
	// Do NOT call RecordExitZeroAttempt — infra errors must not count.

	got, _ := st.GetFindingByFingerprint(ctx, stored.Fingerprint)
	if got.ReproContradicted {
		t.Error("InfraError path: ReproContradicted = true, want false (infra errors must not count)")
	}
}

// TestReproContradiction_SuccessfulReproDoesNotCount verifies acceptance
// criterion 3: a successful reproduction (FinishReproAttempt after att.Promoted)
// does NOT increment exit_zero_count and does NOT set ReproContradicted.
func TestReproContradiction_SuccessfulReproDoesNotCount(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	f := sampleFinding()
	stored, _ := st.UpsertFinding(ctx, f)

	if _, err := st.EnqueueRepro(ctx, stored.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimReproAttempt(ctx, stored.Fingerprint); err != nil {
		t.Fatal(err)
	}
	// Successful repro: FinishReproAttempt (sets state='done'), no RecordExitZeroAttempt.
	if err := st.FinishReproAttempt(ctx, stored.Fingerprint); err != nil {
		t.Fatal(err)
	}

	got, _ := st.GetFindingByFingerprint(ctx, stored.Fingerprint)
	if got.ReproContradicted {
		t.Error("Successful repro path: ReproContradicted = true, want false")
	}
}

// TestReproContradiction_NoQueueRow verifies that a finding without a
// repro_attempts row (never enqueued) has ReproContradicted = false.
func TestReproContradiction_NoQueueRow(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	f := sampleFinding()
	stored, _ := st.UpsertFinding(ctx, f)

	got, err := st.GetFindingByFingerprint(ctx, stored.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if got.ReproContradicted {
		t.Error("domain.Finding with no repro_attempts row: ReproContradicted = true, want false")
	}
}

// TestReproContradiction_ContradictedFingerprintsAccessor verifies that
// ReproContradictedFingerprints returns all contradicted findings and only those.
func TestReproContradiction_ContradictedFingerprintsAccessor(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	// f1 gets 2 exit-zero attempts (contradicted).
	f1 := sampleFinding()
	s1, _ := st.UpsertFinding(ctx, f1)
	_, _ = st.EnqueueRepro(ctx, s1.Fingerprint)
	_ = st.RecordExitZeroAttempt(ctx, s1.Fingerprint)
	_ = st.RecordExitZeroAttempt(ctx, s1.Fingerprint)

	// f2 gets 1 exit-zero attempt (not contradicted).
	f2 := sampleFinding()
	f2.Fingerprint = domain.Fingerprint("nil-deref", "pkg/foo.go", "42|nil deref")
	f2.Lens = "nil-deref"
	f2.File = "pkg/foo.go"
	s2, _ := st.UpsertFinding(ctx, f2)
	_, _ = st.EnqueueRepro(ctx, s2.Fingerprint)
	_ = st.RecordExitZeroAttempt(ctx, s2.Fingerprint)

	fps, err := st.ReproContradictedFingerprints(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(fps) != 1 {
		t.Fatalf("got %d contradicted fingerprints, want 1: %v", len(fps), fps)
	}
	if fps[0] != s1.Fingerprint {
		t.Errorf("contradicted fingerprint = %s, want %s", fps[0], s1.Fingerprint)
	}
}

// TestReproContradiction_SuccessfulReproClearsContradiction pins that a
// successful repro (ZeroExitZeroCount) clears a prior repro-contradicted signal.
// This guards the incoherent state: simultaneously Tier<=1 reproduced AND
// repro-contradicted.
func TestReproContradiction_SuccessfulReproClearsContradiction(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	f := sampleFinding()
	stored, _ := st.UpsertFinding(ctx, f)
	_, _ = st.EnqueueRepro(ctx, stored.Fingerprint)

	// Two exit-zero attempts: now contradicted.
	_ = st.RecordExitZeroAttempt(ctx, stored.Fingerprint)
	_ = st.RecordExitZeroAttempt(ctx, stored.Fingerprint)

	contradicted, _ := st.IsReproContradicted(ctx, stored.Fingerprint)
	if !contradicted {
		t.Fatal("precondition: want contradicted after 2 exit-zero attempts")
	}

	// Successful repro: ZeroExitZeroCount clears the signal.
	if err := st.ZeroExitZeroCount(ctx, stored.Fingerprint); err != nil {
		t.Fatalf("ZeroExitZeroCount: %v", err)
	}

	contradicted, _ = st.IsReproContradicted(ctx, stored.Fingerprint)
	if contradicted {
		t.Error("ReproContradicted still true after ZeroExitZeroCount — must be false once repro succeeds")
	}

	// And via FindingByFingerprint (the report path).
	got, _ := st.GetFindingByFingerprint(ctx, stored.Fingerprint)
	if got.ReproContradicted {
		t.Error("GetFindingByFingerprint: ReproContradicted = true after successful repro, want false")
	}
}
