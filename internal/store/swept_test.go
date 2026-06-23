package store

import (
	"context"
	"fmt"
	"testing"
)

// sampleFindingSwept returns a fresh Finding with a distinct fingerprint for
// sweep tests so it does not collide with sampleFinding().
func sampleFindingSwept(lens, file string, line int, title string) Finding {
	fp := Fingerprint(lens, file, fmt.Sprintf("%d|%s", line, title))
	return Finding{
		Fingerprint: fp,
		Title:       title,
		Description: "test finding for swept_at",
		Reasoning:   "test reasoning",
		Severity:    "high",
		Tier:        2,
		Status:      StatusOpen,
		Lens:        lens,
		File:        file,
		Line:        line,
		CommitSHA:   "sha1",
		FileHash:    "hash-v1",
	}
}

// TestSweptAt_NewFindingIsUnswept checks that a freshly upserted finding has
// a zero SweptAt and appears in UnsweptOpenFindings.
func TestSweptAt_NewFindingIsUnswept(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	f := sampleFindingSwept("race", "pkg/a.go", 1, "unswept race")
	got, err := st.UpsertFinding(ctx, f)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if !got.SweptAt.IsZero() {
		t.Fatalf("new finding should have zero SweptAt, got %v", got.SweptAt)
	}

	unswept, err := st.UnsweptOpenFindings(ctx)
	if err != nil {
		t.Fatalf("UnsweptOpenFindings: %v", err)
	}
	found := false
	for _, u := range unswept {
		if u.ID == got.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("new finding should appear in UnsweptOpenFindings")
	}
}

// TestSweptAt_UpdateFindingSeveritySetsSweptAt checks that calling
// UpdateFindingSeverity sets swept_at to a non-zero time and excludes the
// finding from UnsweptOpenFindings.
func TestSweptAt_UpdateFindingSeveritySetsSweptAt(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	f := sampleFindingSwept("race", "pkg/b.go", 2, "to be swept")
	got, err := st.UpsertFinding(ctx, f)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	if err := st.UpdateFindingSeverity(ctx, got.ID, "critical", "definitely reachable"); err != nil {
		t.Fatalf("UpdateFindingSeverity: %v", err)
	}

	// Re-read via GetFinding and verify SweptAt is non-zero.
	reread, err := st.GetFinding(ctx, got.ID)
	if err != nil {
		t.Fatalf("GetFinding: %v", err)
	}
	if reread.SweptAt.IsZero() {
		t.Fatal("SweptAt should be non-zero after UpdateFindingSeverity")
	}

	// Must be excluded from UnsweptOpenFindings.
	unswept, err := st.UnsweptOpenFindings(ctx)
	if err != nil {
		t.Fatalf("UnsweptOpenFindings: %v", err)
	}
	for _, u := range unswept {
		if u.ID == got.ID {
			t.Fatalf("swept finding should NOT appear in UnsweptOpenFindings")
		}
	}
}

// TestSweptAt_UpsertSameFileHashPreservesSweptAt verifies that re-upserting a
// finding with the same file_hash preserves the swept_at marker set by
// UpdateFindingSeverity (idempotent re-discovery must not reset the sweep).
func TestSweptAt_UpsertSameFileHashPreservesSweptAt(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	f := sampleFindingSwept("race", "pkg/c.go", 3, "preserve sweep")
	got, err := st.UpsertFinding(ctx, f)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	if err := st.UpdateFindingSeverity(ctx, got.ID, "medium", "low impact"); err != nil {
		t.Fatalf("UpdateFindingSeverity: %v", err)
	}

	// Re-upsert with the SAME file_hash.
	f2 := f // same Fingerprint, same FileHash ("hash-v1")
	f2.Reasoning = "re-discovered"
	got2, err := st.UpsertFinding(ctx, f2)
	if err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if got2.SweptAt.IsZero() {
		t.Fatal("swept_at should be preserved across same-file_hash re-upsert")
	}

	// Still excluded from UnsweptOpenFindings.
	unswept, err := st.UnsweptOpenFindings(ctx)
	if err != nil {
		t.Fatalf("UnsweptOpenFindings: %v", err)
	}
	for _, u := range unswept {
		if u.ID == got.ID {
			t.Fatalf("finding with preserved swept_at should NOT appear in UnsweptOpenFindings")
		}
	}
}

// TestSweptAt_UpsertDifferentFileHashResetsSweptAt verifies that re-upserting a
// finding with a DIFFERENT file_hash resets swept_at to zero (code changed →
// reachability must be re-evaluated).
func TestSweptAt_UpsertDifferentFileHashResetsSweptAt(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	f := sampleFindingSwept("race", "pkg/d.go", 4, "reset on new code")
	got, err := st.UpsertFinding(ctx, f)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	if err := st.UpdateFindingSeverity(ctx, got.ID, "high", "reachable"); err != nil {
		t.Fatalf("UpdateFindingSeverity: %v", err)
	}

	// Re-upsert with a DIFFERENT file_hash.
	f2 := f
	f2.FileHash = "hash-v2" // code changed
	f2.CommitSHA = "sha2"
	got2, err := st.UpsertFinding(ctx, f2)
	if err != nil {
		t.Fatalf("re-upsert different file_hash: %v", err)
	}
	if !got2.SweptAt.IsZero() {
		t.Fatalf("swept_at should be reset to zero when file_hash changes, got %v", got2.SweptAt)
	}

	// Must re-appear in UnsweptOpenFindings.
	unswept, err := st.UnsweptOpenFindings(ctx)
	if err != nil {
		t.Fatalf("UnsweptOpenFindings: %v", err)
	}
	found := false
	for _, u := range unswept {
		if u.ID == got.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("finding with reset swept_at should re-appear in UnsweptOpenFindings")
	}
}

// TestSweptAt_ClosedFindingExcluded checks that a closed (non-open) finding
// with swept_at NULL is NOT returned by UnsweptOpenFindings.
func TestSweptAt_ClosedFindingExcluded(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	f := sampleFindingSwept("race", "pkg/e.go", 5, "closed finding")
	f.Status = StatusFixed
	got, err := st.UpsertFinding(ctx, f)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	unswept, err := st.UnsweptOpenFindings(ctx)
	if err != nil {
		t.Fatalf("UnsweptOpenFindings: %v", err)
	}
	for _, u := range unswept {
		if u.ID == got.ID {
			t.Fatalf("closed finding should NOT appear in UnsweptOpenFindings")
		}
	}
}

// TestSweptAt_OrderingOldestFirst verifies that UnsweptOpenFindings returns
// results ordered oldest-updated-at first.
func TestSweptAt_OrderingOldestFirst(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	f1 := sampleFindingSwept("race", "pkg/f.go", 6, "finding one")
	got1, err := st.UpsertFinding(ctx, f1)
	if err != nil {
		t.Fatalf("upsert f1: %v", err)
	}

	// Force f2 to have a later updated_at by re-upserting it (bumps updated_at).
	f2 := sampleFindingSwept("race", "pkg/g.go", 7, "finding two")
	got2, err := st.UpsertFinding(ctx, f2)
	if err != nil {
		t.Fatalf("upsert f2: %v", err)
	}

	// Re-upsert f2 to bump its updated_at after f1.
	_, err = st.UpsertFinding(ctx, f2)
	if err != nil {
		t.Fatalf("re-upsert f2: %v", err)
	}

	unswept, err := st.UnsweptOpenFindings(ctx)
	if err != nil {
		t.Fatalf("UnsweptOpenFindings: %v", err)
	}

	// Find positions.
	pos1, pos2 := -1, -1
	for i, u := range unswept {
		if u.ID == got1.ID {
			pos1 = i
		}
		if u.ID == got2.ID {
			pos2 = i
		}
	}
	if pos1 < 0 || pos2 < 0 {
		t.Fatalf("both findings should appear in UnsweptOpenFindings (pos1=%d pos2=%d)", pos1, pos2)
	}
	if pos1 >= pos2 {
		t.Fatalf("f1 (older updated_at) should come before f2: pos1=%d pos2=%d", pos1, pos2)
	}
}
