package store

import (
	"context"
	"testing"
)

// TestSuppressionFlow is the headline behaviour: a dismissed fingerprint must
// never resurface as open, even if a finder re-discovers it on a later scan.
func TestSuppressionFlow_DismissedNeverResurfaces(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	f := sampleFinding()
	if _, err := st.UpsertFinding(ctx, f); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Maintainer dismisses it via UpdateStatus, which records the suppression.
	if err := st.UpdateStatus(ctx, f.Fingerprint, StatusDismissed, "false positive"); err != nil {
		t.Fatalf("dismiss: %v", err)
	}

	sup, err := st.IsSuppressed(ctx, f.Fingerprint)
	if err != nil {
		t.Fatalf("IsSuppressed: %v", err)
	}
	if !sup {
		t.Fatal("fingerprint should be suppressed after dismissal")
	}

	// A later scan re-discovers the same bug and upserts it as OPEN.
	f2 := f
	f2.Status = StatusOpen
	f2.CommitSHA = "newcommit"
	got, err := st.UpsertFinding(ctx, f2)
	if err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	// Suppression memory must force it back to dismissed.
	if got.Status != StatusDismissed {
		t.Fatalf("re-discovered suppressed finding resurfaced as %q; want dismissed", got.Status)
	}

	open, err := st.ListFindings(ctx, FindingFilter{Status: StatusOpen})
	if err != nil {
		t.Fatalf("list open: %v", err)
	}
	if len(open) != 0 {
		t.Fatalf("expected no open findings, got %d", len(open))
	}
}

func TestAddSuppression_FlipsExistingFindingAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	f := sampleFinding()
	if _, err := st.UpsertFinding(ctx, f); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	if err := st.AddSuppression(ctx, f.Fingerprint, "wontfix"); err != nil {
		t.Fatalf("add suppression: %v", err)
	}
	got, err := st.GetFindingByFingerprint(ctx, f.Fingerprint)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != StatusDismissed {
		t.Fatalf("AddSuppression should flip finding to dismissed, got %q", got.Status)
	}

	// Re-suppress with a new reason: idempotent, updates reason, single row.
	if err := st.AddSuppression(ctx, f.Fingerprint, "still wontfix"); err != nil {
		t.Fatalf("re-suppress: %v", err)
	}
	sups, err := st.ListSuppressions(ctx)
	if err != nil {
		t.Fatalf("list suppressions: %v", err)
	}
	if len(sups) != 1 {
		t.Fatalf("expected 1 suppression row, got %d", len(sups))
	}
	if sups[0].Reason != "still wontfix" {
		t.Fatalf("reason not updated: %q", sups[0].Reason)
	}
}

func TestAddSuppression_PreemptiveBeforeFindingExists(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	fp := Fingerprint("x", "a.go", 1, "preempt")
	// Suppress before any finding exists.
	if err := st.AddSuppression(ctx, fp, "known noise"); err != nil {
		t.Fatalf("preemptive suppress: %v", err)
	}

	// A scan later discovers it as open; it must land dismissed.
	got, err := st.UpsertFinding(ctx, Finding{
		Fingerprint: fp, Title: "preempt", Tier: 3, Status: StatusOpen,
		Lens: "x", File: "a.go", Line: 1,
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if got.Status != StatusDismissed {
		t.Fatalf("pre-suppressed finding should be dismissed, got %q", got.Status)
	}
}

func TestIsSuppressed_Unknown(t *testing.T) {
	st := openTemp(t)
	sup, err := st.IsSuppressed(context.Background(), "never-seen")
	if err != nil {
		t.Fatalf("IsSuppressed: %v", err)
	}
	if sup {
		t.Fatal("unknown fingerprint should not be suppressed")
	}
}
