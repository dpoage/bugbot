package store

import (
	"context"
	"fmt"
	"testing"

	"github.com/dpoage/bugbot/internal/domain"
)

// sampleFindingGV returns a fresh open Tier-2 finding with a distinct
// fingerprint for genuine_verdicts tests.
func sampleFindingGV(file string, line int, title string, verdicts int) domain.Finding {
	fp := domain.Fingerprint("race", file, fmt.Sprintf("%d|%s", line, title))
	return domain.Finding{
		Fingerprint:     fp,
		Title:           title,
		Description:     "test finding for genuine_verdicts",
		Reasoning:       "test reasoning",
		Severity:        "high",
		Tier:            domain.TierVerified,
		Status:          domain.StatusOpen,
		Lens:            "race",
		File:            file,
		Line:            line,
		CommitSHA:       "sha1",
		FileHash:        "hash-v1",
		GenuineVerdicts: verdicts,
	}
}

// TestGenuineVerdicts_RoundTrip verifies the count survives insert and read.
func TestGenuineVerdicts_RoundTrip(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	got, err := st.UpsertFinding(ctx, sampleFindingGV("pkg/a.go", 1, "round trip", 3))
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if got.GenuineVerdicts != 3 {
		t.Fatalf("returned GenuineVerdicts = %d, want 3", got.GenuineVerdicts)
	}
	read, err := st.GetFinding(ctx, got.ID)
	if err != nil {
		t.Fatalf("GetFinding: %v", err)
	}
	if read.GenuineVerdicts != 3 {
		t.Fatalf("read GenuineVerdicts = %d, want 3", read.GenuineVerdicts)
	}
}

// TestGenuineVerdicts_MonotoneOnUnchangedFileHash verifies the accumulation
// invariant: while file_hash is unchanged, a re-upsert with a LOWER count (a
// later budget-degraded 1-seat panel, or a caller that constructed the struct
// without the field) must not erase a prior full-panel validation; a HIGHER
// count raises it.
func TestGenuineVerdicts_MonotoneOnUnchangedFileHash(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	f := sampleFindingGV("pkg/b.go", 2, "monotone", 3)
	if _, err := st.UpsertFinding(ctx, f); err != nil {
		t.Fatalf("seed upsert: %v", err)
	}

	// Degraded rerun: 1-seat panel, same code version.
	f.GenuineVerdicts = 1
	got, err := st.UpsertFinding(ctx, f)
	if err != nil {
		t.Fatalf("degraded upsert: %v", err)
	}
	if got.GenuineVerdicts != 3 {
		t.Errorf("after degraded re-upsert: returned GenuineVerdicts = %d, want 3 (monotone per code version)", got.GenuineVerdicts)
	}
	read, err := st.GetFindingByFingerprint(ctx, f.Fingerprint)
	if err != nil {
		t.Fatalf("GetFindingByFingerprint: %v", err)
	}
	if read.GenuineVerdicts != 3 {
		t.Errorf("after degraded re-upsert: stored GenuineVerdicts = %d, want 3", read.GenuineVerdicts)
	}

	// Zero-count round-trip (caller constructed the struct without the field).
	f.GenuineVerdicts = 0
	if got, err = st.UpsertFinding(ctx, f); err != nil {
		t.Fatalf("zero upsert: %v", err)
	}
	if got.GenuineVerdicts != 3 {
		t.Errorf("after zero re-upsert: GenuineVerdicts = %d, want 3 (must not zero a recorded validation)", got.GenuineVerdicts)
	}

	// A fuller panel raises the count.
	f.GenuineVerdicts = 5
	if got, err = st.UpsertFinding(ctx, f); err != nil {
		t.Fatalf("raise upsert: %v", err)
	}
	if got.GenuineVerdicts != 5 {
		t.Errorf("after fuller-panel re-upsert: GenuineVerdicts = %d, want 5", got.GenuineVerdicts)
	}
}

// TestGenuineVerdicts_ResetOnFileHashChange verifies that when the anchored
// code changes, the old validation is stale evidence: the incoming panel's
// count is written verbatim, even when lower.
func TestGenuineVerdicts_ResetOnFileHashChange(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	f := sampleFindingGV("pkg/c.go", 3, "reset on change", 3)
	if _, err := st.UpsertFinding(ctx, f); err != nil {
		t.Fatalf("seed upsert: %v", err)
	}

	f.FileHash = "hash-v2"
	f.GenuineVerdicts = 1
	got, err := st.UpsertFinding(ctx, f)
	if err != nil {
		t.Fatalf("changed-hash upsert: %v", err)
	}
	if got.GenuineVerdicts != 1 {
		t.Errorf("after changed-hash re-upsert: GenuineVerdicts = %d, want 1 (old validation is stale evidence)", got.GenuineVerdicts)
	}
}

// TestUnderValidatedOpenFindings verifies the drain's WorkRemaining query:
// only OPEN Tier-2 rows below the threshold qualify; Tier-3 rows (owned by
// ReverifySuspected), fully-validated rows, and non-open rows are excluded.
// Ordering is oldest-updated-first.
func TestUnderValidatedOpenFindings(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	eligible1 := sampleFindingGV("pkg/d.go", 4, "one-seat survivor", 1)
	eligible2 := sampleFindingGV("pkg/e.go", 5, "pre-migration unknown", 0)
	full := sampleFindingGV("pkg/f.go", 6, "fully validated", 3)
	suspected := sampleFindingGV("pkg/g.go", 7, "t3 orphan", 0)
	suspected.Tier = domain.TierSuspected
	dismissed := sampleFindingGV("pkg/h.go", 8, "dismissed row", 0)

	seed1, err := st.UpsertFinding(ctx, eligible1)
	if err != nil {
		t.Fatalf("upsert eligible1: %v", err)
	}
	seed2, err := st.UpsertFinding(ctx, eligible2)
	if err != nil {
		t.Fatalf("upsert eligible2: %v", err)
	}
	for _, f := range []domain.Finding{full, suspected} {
		if _, err := st.UpsertFinding(ctx, f); err != nil {
			t.Fatalf("upsert %q: %v", f.Title, err)
		}
	}
	seedDismissed, err := st.UpsertFinding(ctx, dismissed)
	if err != nil {
		t.Fatalf("upsert dismissed: %v", err)
	}
	if err := st.UpdateStatus(ctx, seedDismissed.Fingerprint, domain.StatusDismissed, "test"); err != nil {
		t.Fatalf("dismiss: %v", err)
	}

	under, err := st.UnderValidatedOpenFindings(ctx, 3)
	if err != nil {
		t.Fatalf("UnderValidatedOpenFindings: %v", err)
	}
	if len(under) != 2 {
		t.Fatalf("under-validated = %d rows, want 2: %+v", len(under), under)
	}
	got := map[string]bool{under[0].ID: true, under[1].ID: true}
	if !got[seed1.ID] || !got[seed2.ID] {
		t.Errorf("under-validated IDs = %v, want {%s, %s}", got, seed1.ID, seed2.ID)
	}
}

// TestClearBelowQuorumNeedsHuman verifies the targeted release valve: only a
// below_quorum flag is cleared; prover_exhausted (a different cause with a
// different resolution) is untouched, and a missing fingerprint is a no-op.
func TestClearBelowQuorumNeedsHuman(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	below := sampleFindingGV("pkg/i.go", 9, "below quorum survivor", 1)
	below.NeedsHuman = true
	below.NeedsHumanReason = domain.NeedsHumanReasonBelowQuorum
	if _, err := st.UpsertFinding(ctx, below); err != nil {
		t.Fatalf("upsert below: %v", err)
	}

	prover := sampleFindingGV("pkg/j.go", 10, "prover exhausted", 3)
	prover.Tier = domain.TierReproduced
	prover.ReproPath = "/tmp/repro"
	prover.NeedsHuman = true
	prover.NeedsHumanReason = domain.NeedsHumanReasonProverExhausted
	if _, err := st.UpsertFinding(ctx, prover); err != nil {
		t.Fatalf("upsert prover: %v", err)
	}

	if err := st.ClearBelowQuorumNeedsHuman(ctx, below.Fingerprint); err != nil {
		t.Fatalf("ClearBelowQuorumNeedsHuman(below): %v", err)
	}
	got, err := st.GetFindingByFingerprint(ctx, below.Fingerprint)
	if err != nil {
		t.Fatalf("GetFindingByFingerprint(below): %v", err)
	}
	if got.NeedsHuman || got.NeedsHumanReason != domain.NeedsHumanReasonNone {
		t.Errorf("below-quorum flag not cleared: NeedsHuman=%v reason=%q", got.NeedsHuman, got.NeedsHumanReason)
	}

	if err := st.ClearBelowQuorumNeedsHuman(ctx, prover.Fingerprint); err != nil {
		t.Fatalf("ClearBelowQuorumNeedsHuman(prover): %v", err)
	}
	got, err = st.GetFindingByFingerprint(ctx, prover.Fingerprint)
	if err != nil {
		t.Fatalf("GetFindingByFingerprint(prover): %v", err)
	}
	if !got.NeedsHuman || got.NeedsHumanReason != domain.NeedsHumanReasonProverExhausted {
		t.Errorf("prover_exhausted flag must be untouched: NeedsHuman=%v reason=%q", got.NeedsHuman, got.NeedsHumanReason)
	}

	if err := st.ClearBelowQuorumNeedsHuman(ctx, "no-such-fingerprint"); err != nil {
		t.Errorf("ClearBelowQuorumNeedsHuman(missing) = %v, want nil (no-op)", err)
	}
}
