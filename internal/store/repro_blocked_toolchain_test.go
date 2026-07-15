package store

import (
	"context"
	"errors"
	"testing"
)

// ---------------------------------------------------------------------------
// repro_attempts: blocked_toolchain state + unsandboxed flag (bugbot-14g0)
// ---------------------------------------------------------------------------

// TestReproQueue_BlockOnToolchain_NoAttempt verifies that
// BlockReproAttemptOnToolchain transitions a pending row to blocked_toolchain
// without incrementing attempt_count (no sandbox run happened) and records the
// missing ecosystem — the "blocked_toolchain, no attempt" half of bugbot-14g0
// acceptance 6(a).
func TestReproQueue_BlockOnToolchain_NoAttempt(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)
	fp := "fingerprint-ts-blocked"

	if _, err := st.EnqueueRepro(ctx, fp); err != nil {
		t.Fatalf("EnqueueRepro: %v", err)
	}

	blocked, err := st.BlockReproAttemptOnToolchain(ctx, fp, "js")
	if err != nil {
		t.Fatalf("BlockReproAttemptOnToolchain: %v", err)
	}
	if blocked.State != ReproStateBlockedToolchain {
		t.Errorf("state = %s, want blocked_toolchain", blocked.State)
	}
	if blocked.BlockedEcosystem != "js" {
		t.Errorf("BlockedEcosystem = %q, want %q", blocked.BlockedEcosystem, "js")
	}
	if blocked.AttemptCount != 0 {
		t.Errorf("AttemptCount = %d, want 0 (blocking is not an attempt)", blocked.AttemptCount)
	}

	// A blocked_toolchain row must not be claimable via the "already claimed
	// or exhausted" outcome — it IS claimable (see next test); this asserts
	// the state persisted correctly via a fresh read.
	got, err := st.GetReproAttempt(ctx, fp)
	if err != nil {
		t.Fatalf("GetReproAttempt: %v", err)
	}
	if got.State != ReproStateBlockedToolchain || got.BlockedEcosystem != "js" {
		t.Errorf("GetReproAttempt = %+v, want state=blocked_toolchain blocked_ecosystem=js", got)
	}
}

// TestReproQueue_BlockedToolchain_ClaimableOnceCapabilityRestored verifies
// that a blocked_toolchain row IS claimable via the normal ClaimReproAttempt
// path — the "same finding after host-toolchain mount -> attempt proceeds"
// half of bugbot-14g0 acceptance 6(b): the caller's own capability re-check
// decides whether to call BlockReproAttemptOnToolchain again or
// ClaimReproAttempt; the store must not itself refuse a retry.
func TestReproQueue_BlockedToolchain_ClaimableOnceCapabilityRestored(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)
	fp := "fingerprint-ts-recovers"

	if _, err := st.EnqueueRepro(ctx, fp); err != nil {
		t.Fatalf("EnqueueRepro: %v", err)
	}
	if _, err := st.BlockReproAttemptOnToolchain(ctx, fp, "js"); err != nil {
		t.Fatalf("BlockReproAttemptOnToolchain: %v", err)
	}

	claimed, err := st.ClaimReproAttempt(ctx, fp)
	if err != nil {
		t.Fatalf("ClaimReproAttempt on a blocked_toolchain row should succeed once the capability gate is satisfied, got %v", err)
	}
	if claimed.State != ReproStateRunning {
		t.Errorf("state = %s, want running", claimed.State)
	}
	if claimed.AttemptCount != 1 {
		t.Errorf("AttemptCount = %d, want 1 (the claim IS the first real attempt)", claimed.AttemptCount)
	}
	if claimed.BlockedEcosystem != "" {
		t.Errorf("BlockedEcosystem = %q, want cleared on claim", claimed.BlockedEcosystem)
	}
}

// TestReproQueue_BlockOnToolchain_DoesNotClobberRunning verifies that
// BlockReproAttemptOnToolchain never overwrites an already-running row: if
// another dispatch path won the claim race first, its outcome stands.
func TestReproQueue_BlockOnToolchain_DoesNotClobberRunning(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)
	fp := "fingerprint-race"

	if _, err := st.EnqueueRepro(ctx, fp); err != nil {
		t.Fatalf("EnqueueRepro: %v", err)
	}
	if _, err := st.ClaimReproAttempt(ctx, fp); err != nil {
		t.Fatalf("ClaimReproAttempt: %v", err)
	}

	// A late block call must not clobber the running row.
	got, err := st.BlockReproAttemptOnToolchain(ctx, fp, "js")
	if err != nil {
		t.Fatalf("BlockReproAttemptOnToolchain: %v", err)
	}
	if got.State != ReproStateRunning {
		t.Errorf("state = %s, want running (block must not clobber an active claim)", got.State)
	}
}

// TestReproQueue_MarkUnsandboxed verifies the unsandboxed provenance flag
// (bugbot-14g0 acceptance 5): set only via MarkReproAttemptUnsandboxed, off by
// default, and readable back through GetReproAttempt.
func TestReproQueue_MarkUnsandboxed(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)
	fp := "fingerprint-unsandboxed"

	ra, err := st.EnqueueRepro(ctx, fp)
	if err != nil {
		t.Fatalf("EnqueueRepro: %v", err)
	}
	if ra.Unsandboxed {
		t.Error("Unsandboxed should default to false")
	}

	if err := st.MarkReproAttemptUnsandboxed(ctx, fp); err != nil {
		t.Fatalf("MarkReproAttemptUnsandboxed: %v", err)
	}
	got, err := st.GetReproAttempt(ctx, fp)
	if err != nil {
		t.Fatalf("GetReproAttempt: %v", err)
	}
	if !got.Unsandboxed {
		t.Error("Unsandboxed should be true after MarkReproAttemptUnsandboxed")
	}
}

// TestReproQueue_MarkUnsandboxed_NoRowIsNoError verifies MarkReproAttemptUnsandboxed
// is a safe no-op (not an error) for a fingerprint with no queue row, matching
// the store's existing best-effort update convention for missing rows.
func TestReproQueue_MarkUnsandboxed_NoRowIsNoError(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)
	if err := st.MarkReproAttemptUnsandboxed(ctx, "no-such-fingerprint"); err != nil {
		t.Errorf("MarkReproAttemptUnsandboxed on missing row should be a no-op, got %v", err)
	}
}

// TestReproQueue_BlockOnToolchain_RequiresExistingRow guards the documented
// caller contract (EnqueueRepro first): blocking a fingerprint with no queue
// row updates nothing, so the trailing GetReproAttempt inside
// BlockReproAttemptOnToolchain surfaces ErrNotFound.
func TestReproQueue_BlockOnToolchain_RequiresExistingRow(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)
	if _, err := st.BlockReproAttemptOnToolchain(ctx, "no-such-fingerprint", "js"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound for a fingerprint with no queue row, got %v", err)
	}
}
