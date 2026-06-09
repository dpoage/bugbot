package store

import (
	"context"
	"errors"
	"sort"
	"testing"
)

func TestFileState_UpsertGetAndChangedSince(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	// Initially nothing scanned: every current file is "changed".
	current := map[string]string{
		"a.go": "ha1",
		"b.go": "hb1",
		"c.go": "hc1",
	}
	changed, err := st.ChangedSince(ctx, current)
	if err != nil {
		t.Fatalf("ChangedSince empty store: %v", err)
	}
	if len(changed) != 3 {
		t.Fatalf("expected all 3 files changed against empty store, got %d", len(changed))
	}

	// Record watermarks for a and b only.
	if err := st.UpsertFileStates(ctx, []FileState{
		{Path: "a.go", ContentHash: "ha1", LastScannedCommit: "c1"},
		{Path: "b.go", ContentHash: "hb1", LastScannedCommit: "c1"},
	}); err != nil {
		t.Fatalf("upsert file states: %v", err)
	}

	got, err := st.GetFileState(ctx, "a.go")
	if err != nil {
		t.Fatalf("get file state: %v", err)
	}
	if got.ContentHash != "ha1" || got.LastScannedCommit != "c1" {
		t.Fatalf("unexpected file state: %+v", got)
	}
	if got.LastScannedAt.IsZero() {
		t.Fatal("LastScannedAt should be auto-filled")
	}

	// Now: a unchanged, b changed, c never scanned. Expect b and c.
	changed, err = st.ChangedSince(ctx, map[string]string{
		"a.go": "ha1",
		"b.go": "hb2",
		"c.go": "hc1",
	})
	if err != nil {
		t.Fatalf("ChangedSince: %v", err)
	}
	sort.Strings(changed)
	want := []string{"b.go", "c.go"}
	if len(changed) != 2 || changed[0] != want[0] || changed[1] != want[1] {
		t.Fatalf("expected %v, got %v", want, changed)
	}

	// Upsert overwrites existing rows by path.
	if err := st.UpsertFileStates(ctx, []FileState{
		{Path: "a.go", ContentHash: "ha2", LastScannedCommit: "c2"},
	}); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	got, _ = st.GetFileState(ctx, "a.go")
	if got.ContentHash != "ha2" || got.LastScannedCommit != "c2" {
		t.Fatalf("upsert did not overwrite: %+v", got)
	}
}

func TestGetFileState_NotFound(t *testing.T) {
	st := openTemp(t)
	_, err := st.GetFileState(context.Background(), "nope.go")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestChangedSince_EmptyInput(t *testing.T) {
	st := openTemp(t)
	changed, err := st.ChangedSince(context.Background(), nil)
	if err != nil {
		t.Fatalf("ChangedSince nil: %v", err)
	}
	if changed != nil {
		t.Fatalf("expected nil for empty input, got %v", changed)
	}
}
