package store

import (
	"context"
	"testing"

	"github.com/dpoage/bugbot/internal/domain"
)

// TestSupersedeAsDuplicate_ClosesWithTypedAndProseReason proves the merge-close
// contract bugbot-ezmx.4's backlog reconcile relies on: the duplicate row
// transitions to StatusSuperseded, superseded_by carries the MACHINE-READABLE
// canonical fingerprint (the field any future code must key merge logic on),
// and superseded_reason carries the prose note verbatim -- both round-trip
// through a fresh read.
func TestSupersedeAsDuplicate_ClosesWithTypedAndProseReason(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	canonical := sampleFindingAt("x.go", 10, "nil-safety", "canonical nil deref", domain.StatusOpen)
	dup := sampleFindingAt("x.go", 12, "resource-leaks", "duplicate nil deref", domain.StatusOpen)
	if _, err := st.UpsertFinding(ctx, canonical); err != nil {
		t.Fatalf("seed canonical: %v", err)
	}
	if _, err := st.UpsertFinding(ctx, dup); err != nil {
		t.Fatalf("seed dup: %v", err)
	}

	reason := "backlog reconcile: merged into " + canonical.Fingerprint + " (dedup arbiter yes)"
	if err := st.SupersedeAsDuplicate(ctx, dup.Fingerprint, canonical.Fingerprint, reason); err != nil {
		t.Fatalf("SupersedeAsDuplicate: %v", err)
	}

	got, err := st.queryOne(ctx, "WHERE f.fingerprint = ?", dup.Fingerprint)
	if err != nil {
		t.Fatalf("re-read dup: %v", err)
	}
	if got.Status != domain.StatusSuperseded {
		t.Fatalf("Status = %q, want %q", got.Status, domain.StatusSuperseded)
	}
	if got.SupersededBy != canonical.Fingerprint {
		t.Fatalf("SupersededBy = %q, want %q (machine-readable pointer)", got.SupersededBy, canonical.Fingerprint)
	}
	if got.SupersededReason != reason {
		t.Fatalf("SupersededReason = %q, want %q", got.SupersededReason, reason)
	}

	// The canonical row is untouched by the supersede call itself (callers are
	// responsible for folding sites/lenses beforehand via
	// AppendFindingSites/AddCorroboratingLenses).
	stillOpen, err := st.queryOne(ctx, "WHERE f.fingerprint = ?", canonical.Fingerprint)
	if err != nil {
		t.Fatalf("re-read canonical: %v", err)
	}
	if stillOpen.Status != domain.StatusOpen {
		t.Fatalf("canonical Status = %q, want unchanged %q", stillOpen.Status, domain.StatusOpen)
	}
}

// TestSupersedeAsDuplicate_NotFound proves the ErrNotFound contract for a
// fingerprint with no backing row.
func TestSupersedeAsDuplicate_NotFound(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	err := st.SupersedeAsDuplicate(ctx, "no-such-fingerprint", "some-canonical", "reason")
	if err != ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestSupersedeAsDuplicate_RequiresCanonicalFingerprint proves a superseded
// row can never be written with an empty (dangling) merge pointer.
func TestSupersedeAsDuplicate_RequiresCanonicalFingerprint(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	dup := sampleFindingAt("x.go", 12, "resource-leaks", "duplicate nil deref", domain.StatusOpen)
	if _, err := st.UpsertFinding(ctx, dup); err != nil {
		t.Fatalf("seed dup: %v", err)
	}

	if err := st.SupersedeAsDuplicate(ctx, dup.Fingerprint, "", "reason"); err == nil {
		t.Fatal("expected an error for empty canonical fingerprint, got nil")
	}
}

// TestSupersedeAsDuplicate_RegressionRemint_ClearsProvenanceOnReupsert proves
// the invariant domain.Finding.SupersededBy documents: a live re-discovery of
// the EXACT SAME fingerprint by a future scan (the regression-of-the-merged-
// defect scenario) must clear both superseded_by and superseded_reason back
// to empty, not leave a stale merge pointer dangling on a row that is once
// again StatusOpen.
func TestSupersedeAsDuplicate_RegressionRemint_ClearsProvenanceOnReupsert(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	canonical := sampleFindingAt("x.go", 10, "nil-safety", "canonical nil deref", domain.StatusOpen)
	dup := sampleFindingAt("x.go", 12, "resource-leaks", "duplicate nil deref", domain.StatusOpen)
	if _, err := st.UpsertFinding(ctx, canonical); err != nil {
		t.Fatalf("seed canonical: %v", err)
	}
	if _, err := st.UpsertFinding(ctx, dup); err != nil {
		t.Fatalf("seed dup: %v", err)
	}
	reason := "backlog reconcile: merged into " + canonical.Fingerprint + " (dedup arbiter yes)"
	if err := st.SupersedeAsDuplicate(ctx, dup.Fingerprint, canonical.Fingerprint, reason); err != nil {
		t.Fatalf("SupersedeAsDuplicate: %v", err)
	}

	// A fresh scan re-discovers the SAME defect (same lens/file/locus -> same
	// v3 fingerprint) and upserts it as open, exactly like any other
	// re-discovery. UpsertFinding's incoming struct never carries
	// SupersededBy/Reason (no caller sets them), so this exercises the UPDATE
	// path's clearing behavior.
	remint := sampleFindingAt("x.go", 12, "resource-leaks", "duplicate nil deref", domain.StatusOpen)
	if _, err := st.UpsertFinding(ctx, remint); err != nil {
		t.Fatalf("re-upsert (regression remint): %v", err)
	}

	got, err := st.queryOne(ctx, "WHERE f.fingerprint = ?", dup.Fingerprint)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if got.Status != domain.StatusOpen {
		t.Fatalf("Status after re-upsert = %q, want %q", got.Status, domain.StatusOpen)
	}
	if got.SupersededBy != "" {
		t.Fatalf("SupersededBy after re-upsert = %q, want empty (stale merge pointer must not survive)", got.SupersededBy)
	}
	if got.SupersededReason != "" {
		t.Fatalf("SupersededReason after re-upsert = %q, want empty", got.SupersededReason)
	}
}
