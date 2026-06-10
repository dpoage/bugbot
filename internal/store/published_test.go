package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestUpsertAndGetPublishedIssue(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	fp := "fp-abc123"
	if err := st.UpsertPublishedIssue(ctx, fp, 42, "open"); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	pi, err := st.GetPublishedIssue(ctx, fp)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if pi.Fingerprint != fp {
		t.Errorf("fingerprint = %q, want %q", pi.Fingerprint, fp)
	}
	if pi.IssueNumber != 42 {
		t.Errorf("issue_number = %d, want 42", pi.IssueNumber)
	}
	if pi.State != "open" {
		t.Errorf("state = %q, want open", pi.State)
	}
	if pi.CreatedAt.IsZero() {
		t.Error("created_at is zero")
	}
	if pi.UpdatedAt.IsZero() {
		t.Error("updated_at is zero")
	}
}

func TestGetPublishedIssue_NotFound(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	_, err := st.GetPublishedIssue(ctx, "does-not-exist")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestUpsertPublishedIssue_PreservesCreatedAt(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	fp := "fp-preserve"
	if err := st.UpsertPublishedIssue(ctx, fp, 10, "open"); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	first, err := st.GetPublishedIssue(ctx, fp)
	if err != nil {
		t.Fatalf("get first: %v", err)
	}

	// Upsert again with a new issue number and closed state.
	if err := st.UpsertPublishedIssue(ctx, fp, 99, "closed"); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	second, err := st.GetPublishedIssue(ctx, fp)
	if err != nil {
		t.Fatalf("get second: %v", err)
	}

	// created_at must not change.
	if !second.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("created_at changed: %v -> %v", first.CreatedAt, second.CreatedAt)
	}
	// issue_number and state must update.
	if second.IssueNumber != 99 {
		t.Errorf("issue_number = %d, want 99", second.IssueNumber)
	}
	if second.State != "closed" {
		t.Errorf("state = %q, want closed", second.State)
	}
}

func TestListPublishedIssues_DeterministicOrder(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	fps := []string{"zzz", "aaa", "mmm"}
	for i, fp := range fps {
		if err := st.UpsertPublishedIssue(ctx, fp, i+1, "open"); err != nil {
			t.Fatalf("upsert %q: %v", fp, err)
		}
	}

	list, err := st.ListPublishedIssues(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(list))
	}
	// ORDER BY fingerprint ASC -> aaa, mmm, zzz
	want := []string{"aaa", "mmm", "zzz"}
	for i, pi := range list {
		if pi.Fingerprint != want[i] {
			t.Errorf("row %d fingerprint = %q, want %q", i, pi.Fingerprint, want[i])
		}
	}
}

func TestListPublishedIssues_Empty(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	list, err := st.ListPublishedIssues(ctx)
	if err != nil {
		t.Fatalf("list empty: %v", err)
	}
	if list != nil {
		t.Errorf("expected nil slice for empty table, got %v", list)
	}
}

// TestCountPublishedIssues covers the state tally for the status pane.
func TestCountPublishedIssues(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	empty, err := st.CountPublishedIssues(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("empty store: %v", empty)
	}

	for i, state := range []string{"open", "open", "closed", "pending"} {
		if err := st.UpsertPublishedIssue(ctx, fmt.Sprintf("fp%d", i), i+1, state); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.CountPublishedIssues(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got["open"] != 2 || got["closed"] != 1 || got["pending"] != 1 {
		t.Errorf("counts = %v", got)
	}
}
