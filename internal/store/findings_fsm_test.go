package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/domain"
)

// ---------------------------------------------------------------------------
// repro_attempts queue: claim/skip semantics and infra-retry (acceptance 2+3)
// ---------------------------------------------------------------------------

func TestReproQueue_ClaimSkip(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	// Enqueue is idempotent.
	fp := "fingerprint-abc"
	ra1, err := st.EnqueueRepro(ctx, fp)
	if err != nil {
		t.Fatalf("EnqueueRepro: %v", err)
	}
	ra2, err := st.EnqueueRepro(ctx, fp)
	if err != nil {
		t.Fatalf("EnqueueRepro idempotent: %v", err)
	}
	if ra1.ID != ra2.ID {
		t.Errorf("second enqueue should return same row; got %s vs %s", ra1.ID, ra2.ID)
	}
	if ra1.State != ReproStatePending {
		t.Errorf("initial state = %s, want pending", ra1.State)
	}

	// First claim succeeds.
	claimed, err := st.ClaimReproAttempt(ctx, fp)
	if err != nil {
		t.Fatalf("ClaimReproAttempt: %v", err)
	}
	if claimed.State != ReproStateRunning {
		t.Errorf("state = %s, want running", claimed.State)
	}
	if claimed.AttemptCount != 1 {
		t.Errorf("attempt_count = %d, want 1", claimed.AttemptCount)
	}

	// Second claim on the same running row is rejected.
	_, err = st.ClaimReproAttempt(ctx, fp)
	if !errors.Is(err, ErrReproAlreadyClaimed) {
		t.Errorf("second claim: expected ErrReproAlreadyClaimed, got %v", err)
	}

	// Finish marks done.
	if err := st.FinishReproAttempt(ctx, fp); err != nil {
		t.Fatalf("FinishReproAttempt: %v", err)
	}
	done, err := st.GetReproAttempt(ctx, fp)
	if err != nil {
		t.Fatalf("GetReproAttempt: %v", err)
	}
	if done.State != ReproStateDone {
		t.Errorf("state = %s, want done", done.State)
	}

	// Done row is not claimable.
	_, err = st.ClaimReproAttempt(ctx, fp)
	if !errors.Is(err, ErrReproAlreadyClaimed) {
		t.Errorf("claim on done: expected ErrReproAlreadyClaimed, got %v", err)
	}
}

func TestReproQueue_InfraRetryBounded(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	fp := "fingerprint-infra"
	if _, err := st.EnqueueRepro(ctx, fp); err != nil {
		t.Fatalf("EnqueueRepro: %v", err)
	}

	// Simulate DefaultReproMaxAttempts infra errors.
	for i := 0; i < DefaultReproMaxAttempts; i++ {
		if _, err := st.ClaimReproAttempt(ctx, fp); err != nil {
			t.Fatalf("claim attempt %d: %v", i, err)
		}
		if err := st.RequeueReproAttemptOnInfraError(ctx, fp, "sandbox timeout"); err != nil {
			t.Fatalf("requeue attempt %d: %v", i, err)
		}
		ra, err := st.GetReproAttempt(ctx, fp)
		if err != nil {
			t.Fatalf("get attempt %d: %v", i, err)
		}
		if i < DefaultReproMaxAttempts-1 {
			if ra.State != ReproStateInfraRetry {
				t.Errorf("attempt %d: state = %s, want infra_retry", i, ra.State)
			}
		} else {
			// Last infra error exhausts the budget → abandoned.
			if ra.State != ReproStateAbandoned {
				t.Errorf("last attempt: state = %s, want abandoned", ra.State)
			}
		}
	}

	// Exhausted row cannot be claimed.
	_, err := st.ClaimReproAttempt(ctx, fp)
	if !errors.Is(err, ErrReproAlreadyClaimed) {
		t.Errorf("claim on abandoned: expected ErrReproAlreadyClaimed, got %v", err)
	}

	// PendingReproAttempts must not include the abandoned row.
	pending, err := st.PendingReproAttempts(ctx)
	if err != nil {
		t.Fatalf("PendingReproAttempts: %v", err)
	}
	for _, ra := range pending {
		if ra.Fingerprint == fp {
			t.Errorf("abandoned fingerprint %s should not appear in pending list", fp)
		}
	}
}

// TestUpsertFinding_PreMigrationNeedsHumanReason is a regression test for the
// migration-018 backfill blocker: a pre-migration row with needs_human=1 and
// needs_human_reason=" (the default before the backfill) must NOT cause
// UpsertFinding to reject the row on its next UPDATE. The migration backfill
// assigns a non-empty reason to every such row; this test verifies that a row
// with an empty reason (simulating a missed backfill or direct-SQL write) is
// recovered gracefully — by the recovery branch synthesizing a non-None reason
// when the stored reason is empty and NeedsHuman is true.
func TestUpsertFinding_PreMigrationNeedsHumanRow(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	// Seed a "below_quorum" finding through the normal path so the row exists.
	base := sampleFinding()
	base.NeedsHuman = true
	base.NeedsHumanReason = domain.NeedsHumanReasonBelowQuorum
	stored, err := st.UpsertFinding(ctx, base)
	if err != nil {
		t.Fatalf("initial upsert: %v", err)
	}

	// Simulate a pre-migration row: overwrite needs_human_reason to '' in the DB
	// directly, as if the migration backfill had not run or a direct SQL write
	// cleared the column.
	if _, err := st.DB().ExecContext(ctx,
		"UPDATE findings SET needs_human_reason='' WHERE id=?", stored.ID,
	); err != nil {
		t.Fatalf("raw SQL wipe: %v", err)
	}

	// Re-upsert: incoming re-scan carries NeedsHuman=true, domain.NeedsHumanReason=None
	// (as a verifier re-run would). The recovery branch must synthesise a
	// non-None reason so ValidateFindingState does not reject the UPDATE.
	rescan := base
	rescan.NeedsHuman = true
	rescan.NeedsHumanReason = domain.NeedsHumanReasonNone // incoming has no reason
	rescan.Reasoning = "updated reasoning"
	got, err := st.UpsertFinding(ctx, rescan)
	if err != nil {
		t.Fatalf("re-upsert of pre-migration row: %v", err)
	}
	if !got.NeedsHuman {
		t.Errorf("NeedsHuman cleared; want still true")
	}
	if got.NeedsHumanReason == domain.NeedsHumanReasonNone {
		t.Errorf("domain.NeedsHumanReason empty after re-upsert; want a non-None reason to be synthesised")
	}
}

// TestReproQueue_StaleLease verifies that a crash-stuck 'running' row older
// than ReproStaleLeaseDuration is reclaimed on the next ClaimReproAttempt call
// (acceptance criterion for the should-fix: crash-stuck rows must not be
// permanently unclaimable).
func TestReproQueue_StaleLease(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	fp := "fingerprint-stale"
	if _, err := st.EnqueueRepro(ctx, fp); err != nil {
		t.Fatalf("EnqueueRepro: %v", err)
	}
	// Claim once to move to 'running'.
	if _, err := st.ClaimReproAttempt(ctx, fp); err != nil {
		t.Fatalf("first claim: %v", err)
	}

	// Backdate updated_at to simulate a crash-stuck row.
	staleTime := nowUTC().Add(-(ReproStaleLeaseDuration + time.Minute))
	if _, err := st.DB().ExecContext(ctx,
		"UPDATE repro_attempts SET updated_at=? WHERE fingerprint=?",
		staleTime.Format(timeLayout), fp,
	); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	// A second ClaimReproAttempt must reclaim the stale row.
	claimed, err := st.ClaimReproAttempt(ctx, fp)
	if err != nil {
		t.Fatalf("reclaim stale: %v", err)
	}
	if claimed.State != ReproStateRunning {
		t.Errorf("reclaimed state = %s, want running", claimed.State)
	}
	if claimed.AttemptCount != 2 {
		t.Errorf("attempt_count = %d, want 2 (reclaim counts as an attempt)", claimed.AttemptCount)
	}
}

// TestReproQueue_ResetOnCodeChange verifies that a 'done' queue row is reset
// to 'pending' when the finding's anchored code (file_hash) changes, so the
// finding is re-eligible for reproduction after a code change.
func TestReproQueue_ResetOnCodeChange(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	// Seed a finding and enqueue + finish its repro attempt.
	f := sampleFinding()
	f.Tier = 1
	f.ReproPath = "/artifacts/repro"
	stored, err := st.UpsertFinding(ctx, f)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	fp := stored.Fingerprint
	if _, err := st.EnqueueRepro(ctx, fp); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := st.ClaimReproAttempt(ctx, fp); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := st.FinishReproAttempt(ctx, fp); err != nil {
		t.Fatalf("finish: %v", err)
	}
	ra, _ := st.GetReproAttempt(ctx, fp)
	if ra.State != ReproStateDone {
		t.Fatalf("state = %s, want done", ra.State)
	}

	// Re-upsert with a changed file_hash — mirrors a real code change.
	updated := stored
	updated.FileHash = "new-file-hash"
	if _, err := st.UpsertFinding(ctx, updated); err != nil {
		t.Fatalf("re-upsert with new hash: %v", err)
	}

	// Queue row must now be pending.
	ra2, err := st.GetReproAttempt(ctx, fp)
	if err != nil {
		t.Fatalf("GetReproAttempt after reset: %v", err)
	}
	if ra2.State != ReproStatePending {
		t.Errorf("state = %s, want pending after code change", ra2.State)
	}
	if ra2.AttemptCount != 0 {
		t.Errorf("attempt_count = %d, want 0 after reset", ra2.AttemptCount)
	}
}
