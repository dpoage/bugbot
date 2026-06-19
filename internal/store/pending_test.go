package store

import (
	"context"
	"testing"
)

// TestPendingCandidates_RoundTrip covers the write-ahead-log lifecycle: batch
// insert assigns ids, list returns them creation-ordered with full fidelity,
// and delete removes exactly one row.
func TestPendingCandidates_RoundTrip(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	rows := []PendingCandidate{
		{
			ScanRunID:           "run-1",
			CommitSHA:           "abc123",
			Lens:                "nil-safety/error-handling",
			File:                "a.go",
			Line:                10,
			Title:               "nil deref of cfg",
			Description:         "cfg may be nil",
			Severity:            "high",
			Evidence:            "returns cfg.Name without a nil check",
			Confidence:          "high",
			CorroboratingLenses: []string{"resource-leak"},
		},
		{
			ScanRunID:  "run-1",
			CommitSHA:  "abc123",
			Lens:       "concurrency",
			File:       "b.go",
			Line:       42,
			Title:      "data race on counter",
			Confidence: "medium",
		},
	}

	if err := st.AddPendingCandidates(ctx, rows); err != nil {
		t.Fatalf("AddPendingCandidates: %v", err)
	}
	// IDs must be written back into the caller's slice so it can carry them as
	// the candidate's PendingID for the terminal-fate delete.
	for i := range rows {
		if rows[i].ID == "" {
			t.Fatalf("row %d: ID not assigned", i)
		}
		if rows[i].CreatedAt.IsZero() {
			t.Errorf("row %d: CreatedAt not set", i)
		}
	}

	got, err := st.ListPendingCandidates(ctx)
	if err != nil {
		t.Fatalf("ListPendingCandidates: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("list len = %d, want 2", len(got))
	}
	// Oldest-first by id: insertion order is preserved within a batch.
	if got[0].ID != rows[0].ID || got[1].ID != rows[1].ID {
		t.Errorf("list order = [%s,%s], want [%s,%s]", got[0].ID, got[1].ID, rows[0].ID, rows[1].ID)
	}

	// Full-fidelity round-trip on the first row.
	a := got[0]
	if a.ScanRunID != "run-1" || a.CommitSHA != "abc123" || a.Lens != "nil-safety/error-handling" ||
		a.File != "a.go" || a.Line != 10 || a.Title != "nil deref of cfg" ||
		a.Description != "cfg may be nil" || a.Severity != "high" ||
		a.Evidence != "returns cfg.Name without a nil check" || a.Confidence != "high" {
		t.Errorf("row 0 fidelity mismatch: %+v", a)
	}
	if len(a.CorroboratingLenses) != 1 || a.CorroboratingLenses[0] != "resource-leak" {
		t.Errorf("row 0 corroborating lenses = %v, want [resource-leak]", a.CorroboratingLenses)
	}

	// Delete one row; the other survives.
	if err := st.DeletePendingCandidate(ctx, rows[0].ID); err != nil {
		t.Fatalf("DeletePendingCandidate: %v", err)
	}
	got, err = st.ListPendingCandidates(ctx)
	if err != nil {
		t.Fatalf("ListPendingCandidates after delete: %v", err)
	}
	if len(got) != 1 || got[0].ID != rows[1].ID {
		t.Fatalf("after delete: got %d rows, want only %s", len(got), rows[1].ID)
	}
}

// TestPendingCandidates_DeleteEmptyID asserts the empty-id no-op contract:
// callers delete unconditionally at a terminal fate, including for candidates
// that were never WAL-persisted (empty PendingID).
func TestPendingCandidates_DeleteEmptyID(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)
	if err := st.DeletePendingCandidate(ctx, ""); err != nil {
		t.Fatalf("DeletePendingCandidate(\"\") = %v, want nil", err)
	}
}

// TestPendingCandidates_AddEmptyBatch asserts an empty batch is a no-op and
// never touches the database.
func TestPendingCandidates_AddEmptyBatch(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)
	if err := st.AddPendingCandidates(ctx, nil); err != nil {
		t.Fatalf("AddPendingCandidates(nil) = %v, want nil", err)
	}
	got, err := st.ListPendingCandidates(ctx)
	if err != nil {
		t.Fatalf("ListPendingCandidates: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("list len = %d, want 0", len(got))
	}
}
