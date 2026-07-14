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
	if err := st.UpsertPublishedIssue(ctx, fp, 42, "open", ""); err != nil {
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
	_, err := st.GetPublishedIssue(ctx, "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestUpsertPublishedIssue_PreservesCreatedAt(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	fp := "fp-stable"
	if err := st.UpsertPublishedIssue(ctx, fp, 1, "open", ""); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	first, err := st.GetPublishedIssue(ctx, fp)
	if err != nil {
		t.Fatalf("first get: %v", err)
	}

	if err := st.UpsertPublishedIssue(ctx, fp, 2, "closed", ""); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	second, err := st.GetPublishedIssue(ctx, fp)
	if err != nil {
		t.Fatalf("second get: %v", err)
	}

	if !second.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("created_at drifted: %s -> %s", first.CreatedAt, second.CreatedAt)
	}
	if second.IssueNumber != 2 {
		t.Errorf("issue_number = %d, want 2", second.IssueNumber)
	}
	if second.State != "closed" {
		t.Errorf("state = %q, want closed", second.State)
	}
	if !second.UpdatedAt.After(first.UpdatedAt) {
		t.Errorf("updated_at did not advance: %s -> %s", first.UpdatedAt, second.UpdatedAt)
	}
}

func TestListPublishedIssues_DeterministicOrder(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	fps := []string{"zzz", "aaa", "mmm"}
	for i, fp := range fps {
		if err := st.UpsertPublishedIssue(ctx, fp, i+1, "open", ""); err != nil {
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

	for i, state := range []IssueState{IssueStateOpen, IssueStateOpen, IssueStateClosed, IssueStatePending} {
		if err := st.UpsertPublishedIssue(ctx, fmt.Sprintf("fp%d", i), i+1, state, ""); err != nil {
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

// TestListPublishedIssues_StableOrderAcrossCalls is a determinism guard for
// the 89r.5 secondary sort. fingerprint is the primary key so two rows with
// the same fingerprint cannot exist; the test instead pins the externally
// observable contract that two consecutive calls return the rows in the
// same order. Any future schema that removes the PK constraint (so duplicate
// fingerprints become possible) gets the rowid tiebreak for free, and this
// test will still pass.
func TestListPublishedIssues_StableOrderAcrossCalls(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	// Insert a handful of rows in a non-sorted insertion order; with the
	// fingerprint ASC primary, the list must always return them in
	// fingerprint order, regardless of insertion order.
	fps := []string{"zzz", "aaa", "mmm", "bbb", "yyy"}
	for i, fp := range fps {
		if err := st.UpsertPublishedIssue(ctx, fp, i+1, "open", ""); err != nil {
			t.Fatalf("upsert %q: %v", fp, err)
		}
	}

	first, err := st.ListPublishedIssues(ctx)
	if err != nil {
		t.Fatalf("list 1: %v", err)
	}
	second, err := st.ListPublishedIssues(ctx)
	if err != nil {
		t.Fatalf("list 2: %v", err)
	}
	if len(first) != len(fps) || len(second) != len(fps) {
		t.Fatalf("lengths: first=%d second=%d want=%d", len(first), len(second), len(fps))
	}

	want := []string{"aaa", "bbb", "mmm", "yyy", "zzz"}
	for i, w := range want {
		if first[i].Fingerprint != w {
			t.Errorf("first[%d] = %q, want %q", i, first[i].Fingerprint, w)
		}
		if second[i].Fingerprint != w {
			t.Errorf("second[%d] = %q, want %q", i, second[i].Fingerprint, w)
		}
	}
}

// TestDeletePublishedIssue covers the stale-row cleanup path used by the
// publish reconciler when a GitHub issue returns 410/404: insert, delete,
// confirm the row is gone (ErrNotFound) and confirm a second delete is
// idempotent (no error).
func TestDeletePublishedIssue(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	fp := "fp-stale"
	if err := st.UpsertPublishedIssue(ctx, fp, 99, "open", ""); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Pre-condition: row exists.
	if _, err := st.GetPublishedIssue(ctx, fp); err != nil {
		t.Fatalf("pre-delete get: %v", err)
	}

	// Delete the row.
	if err := st.DeletePublishedIssue(ctx, fp); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Row is gone.
	if _, err := st.GetPublishedIssue(ctx, fp); !errors.Is(err, ErrNotFound) {
		t.Errorf("post-delete get err = %v, want ErrNotFound", err)
	}

	// Idempotent: deleting again is a no-op, not an error.
	if err := st.DeletePublishedIssue(ctx, fp); err != nil {
		t.Errorf("second delete err = %v, want nil (idempotent)", err)
	}

	// A different fingerprint is unaffected.
	if err := st.UpsertPublishedIssue(ctx, "fp-fresh", 7, "open", ""); err != nil {
		t.Fatalf("seed fresh: %v", err)
	}
	if err := st.DeletePublishedIssue(ctx, fp); err != nil {
		t.Errorf("delete on non-existent fp err = %v, want nil", err)
	}
	if _, err := st.GetPublishedIssue(ctx, "fp-fresh"); err != nil {
		t.Errorf("unrelated row was touched by delete: %v", err)
	}
}

// TestUpsertPublishedIssue_BodyHashRoundTrip pins migration 025
// (published_body_hash): a fresh openTemp store runs 001-025 in order, so
// body_hash must exist and round-trip through Get/List, and a conflict
// upsert must refresh it (not just issue_number/state/updated_at) so the
// publish apply loop's no-op-PATCH check (bugbot-klaj) always compares
// against the hash of the body actually pushed last.
func TestUpsertPublishedIssue_BodyHashRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	fp := "fp-hash"
	if err := st.UpsertPublishedIssue(ctx, fp, 5, IssueStateOpen, "abc123hash"); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := st.GetPublishedIssue(ctx, fp)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.BodyHash != "abc123hash" {
		t.Errorf("BodyHash = %q, want %q", got.BodyHash, "abc123hash")
	}

	list, err := st.ListPublishedIssues(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].BodyHash != "abc123hash" {
		t.Errorf("list BodyHash = %+v, want [abc123hash]", list)
	}

	// Conflict-update path must also refresh body_hash.
	if err := st.UpsertPublishedIssue(ctx, fp, 5, IssueStateOpen, "def456hash"); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	got2, err := st.GetPublishedIssue(ctx, fp)
	if err != nil {
		t.Fatalf("get2: %v", err)
	}
	if got2.BodyHash != "def456hash" {
		t.Errorf("BodyHash after conflict update = %q, want %q", got2.BodyHash, "def456hash")
	}

	// A row upserted with "" (e.g. pending/closing/closed states, which
	// never push a body) leaves the column at the migration's DEFAULT ''.
	if err := st.UpsertPublishedIssue(ctx, "fp-nobody", 6, IssueStatePending, ""); err != nil {
		t.Fatalf("upsert nobody: %v", err)
	}
	nobody, err := st.GetPublishedIssue(ctx, "fp-nobody")
	if err != nil {
		t.Fatalf("get nobody: %v", err)
	}
	if nobody.BodyHash != "" {
		t.Errorf("BodyHash = %q, want empty for a pending row", nobody.BodyHash)
	}
}
