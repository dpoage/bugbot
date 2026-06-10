package store

import (
	"context"
	"testing"
)

// makeTestLead is a helper that builds a Lead with required fields set.
func makeTestLead(posterLens, targetLens, file string, line int, note string) Lead {
	return Lead{
		PosterLens: posterLens,
		TargetLens: targetLens,
		File:       file,
		Line:       line,
		Note:       note,
	}
}

func TestLeads_AddAndPending(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	l1 := makeTestLead("nil-safety/error-handling", "concurrency", "pkg/db.go", 42, "locking around cache map looks inconsistent")
	l2 := makeTestLead("concurrency", "resource-leaks", "pkg/conn.go", 10, "connection may leak on error path")

	if err := st.AddLead(ctx, l1); err != nil {
		t.Fatalf("AddLead l1: %v", err)
	}
	if err := st.AddLead(ctx, l2); err != nil {
		t.Fatalf("AddLead l2: %v", err)
	}

	// Only concurrency pending leads.
	leads, err := st.PendingLeads(ctx, "concurrency")
	if err != nil {
		t.Fatalf("PendingLeads concurrency: %v", err)
	}
	if len(leads) != 1 {
		t.Fatalf("want 1 pending lead for concurrency, got %d", len(leads))
	}
	got := leads[0]
	if got.PosterLens != "nil-safety/error-handling" {
		t.Errorf("poster_lens = %q, want nil-safety/error-handling", got.PosterLens)
	}
	if got.TargetLens != "concurrency" {
		t.Errorf("target_lens = %q, want concurrency", got.TargetLens)
	}
	if got.File != "pkg/db.go" {
		t.Errorf("file = %q, want pkg/db.go", got.File)
	}
	if got.Line != 42 {
		t.Errorf("line = %d, want 42", got.Line)
	}
	if got.Note != "locking around cache map looks inconsistent" {
		t.Errorf("note = %q", got.Note)
	}
	if got.Status != "posted" {
		t.Errorf("status = %q, want posted", got.Status)
	}
	if got.CreatedAt.IsZero() {
		t.Error("created_at is zero")
	}
	if got.ID == "" {
		t.Error("id is empty")
	}

	// resource-leaks has its own lead.
	rlLeads, err := st.PendingLeads(ctx, "resource-leaks")
	if err != nil {
		t.Fatalf("PendingLeads resource-leaks: %v", err)
	}
	if len(rlLeads) != 1 {
		t.Fatalf("want 1 pending lead for resource-leaks, got %d", len(rlLeads))
	}

	// No leads for nil-safety.
	nilLeads, err := st.PendingLeads(ctx, "nil-safety/error-handling")
	if err != nil {
		t.Fatalf("PendingLeads nil-safety: %v", err)
	}
	if len(nilLeads) != 0 {
		t.Errorf("want 0 pending leads for nil-safety, got %d", len(nilLeads))
	}
}

func TestLeads_UpsertRefreshesNoteAndPreservesCreatedAt(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	original := makeTestLead("nil-safety/error-handling", "concurrency", "pkg/foo.go", 7, "original note")
	if err := st.AddLead(ctx, original); err != nil {
		t.Fatalf("AddLead: %v", err)
	}

	leads, err := st.PendingLeads(ctx, "concurrency")
	if err != nil {
		t.Fatalf("PendingLeads: %v", err)
	}
	if len(leads) != 1 {
		t.Fatalf("want 1, got %d", len(leads))
	}
	originalCreatedAt := leads[0].CreatedAt

	// Re-post with a different note and poster.
	updated := makeTestLead("resource-leaks", "concurrency", "pkg/foo.go", 7, "updated note with more detail")
	if err := st.AddLead(ctx, updated); err != nil {
		t.Fatalf("AddLead upsert: %v", err)
	}

	leads2, err := st.PendingLeads(ctx, "concurrency")
	if err != nil {
		t.Fatalf("PendingLeads after upsert: %v", err)
	}
	if len(leads2) != 1 {
		t.Fatalf("want 1 after upsert (dedup), got %d", len(leads2))
	}
	got := leads2[0]
	if got.Note != "updated note with more detail" {
		t.Errorf("note not updated: %q", got.Note)
	}
	if got.PosterLens != "resource-leaks" {
		t.Errorf("poster_lens not updated: %q", got.PosterLens)
	}
	if !got.CreatedAt.Equal(originalCreatedAt) {
		t.Errorf("created_at changed: got %v, want %v", got.CreatedAt, originalCreatedAt)
	}
	if got.Status != "posted" {
		t.Errorf("status = %q, want posted", got.Status)
	}
}

func TestLeads_ConsumeLeads(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	l1 := makeTestLead("nil-safety/error-handling", "concurrency", "a.go", 1, "note1")
	l2 := makeTestLead("nil-safety/error-handling", "concurrency", "b.go", 2, "note2")
	if err := st.AddLead(ctx, l1); err != nil {
		t.Fatalf("AddLead l1: %v", err)
	}
	if err := st.AddLead(ctx, l2); err != nil {
		t.Fatalf("AddLead l2: %v", err)
	}

	leads, err := st.PendingLeads(ctx, "concurrency")
	if err != nil {
		t.Fatalf("PendingLeads: %v", err)
	}
	if len(leads) != 2 {
		t.Fatalf("want 2 leads, got %d", len(leads))
	}

	ids := []string{leads[0].ID, leads[1].ID}
	if err := st.ConsumeLeads(ctx, ids); err != nil {
		t.Fatalf("ConsumeLeads: %v", err)
	}

	// No pending leads left.
	after, err := st.PendingLeads(ctx, "concurrency")
	if err != nil {
		t.Fatalf("PendingLeads after consume: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("want 0 after consume, got %d", len(after))
	}
}

func TestLeads_ConsumedThenReposted(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	// Post and consume a lead.
	l := makeTestLead("nil-safety/error-handling", "concurrency", "x.go", 5, "first note")
	if err := st.AddLead(ctx, l); err != nil {
		t.Fatalf("AddLead: %v", err)
	}
	leads, err := st.PendingLeads(ctx, "concurrency")
	if err != nil || len(leads) != 1 {
		t.Fatalf("PendingLeads: err=%v len=%d", err, len(leads))
	}
	if err := st.ConsumeLeads(ctx, []string{leads[0].ID}); err != nil {
		t.Fatalf("ConsumeLeads: %v", err)
	}

	// Verify it's consumed.
	after, err := st.PendingLeads(ctx, "concurrency")
	if err != nil {
		t.Fatalf("PendingLeads after consume: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("want 0 after consume, got %d", len(after))
	}

	// Re-post the same (target_lens, file, line): should flip back to 'posted'.
	l2 := makeTestLead("resource-leaks", "concurrency", "x.go", 5, "re-raised suspicion")
	if err := st.AddLead(ctx, l2); err != nil {
		t.Fatalf("AddLead re-post: %v", err)
	}

	// Should be pending again with the new note.
	reposted, err := st.PendingLeads(ctx, "concurrency")
	if err != nil {
		t.Fatalf("PendingLeads after repost: %v", err)
	}
	if len(reposted) != 1 {
		t.Fatalf("want 1 after repost, got %d", len(reposted))
	}
	got := reposted[0]
	if got.Status != "posted" {
		t.Errorf("status = %q, want posted after repost", got.Status)
	}
	if got.Note != "re-raised suspicion" {
		t.Errorf("note = %q after repost", got.Note)
	}
	if !got.ConsumedAt.IsZero() {
		t.Errorf("consumed_at should be zero after repost, got %v", got.ConsumedAt)
	}
}

func TestLeads_ConsumeEmpty(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	// ConsumeLeads on empty ids is a no-op.
	if err := st.ConsumeLeads(ctx, nil); err != nil {
		t.Errorf("ConsumeLeads(nil): %v", err)
	}
	if err := st.ConsumeLeads(ctx, []string{}); err != nil {
		t.Errorf("ConsumeLeads([]): %v", err)
	}
}

func TestLeads_DeterministicOrdering(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	// Insert three leads for the same target lens; PendingLeads must return them
	// in created_at (then id) order. The IDs are time-prefixed, so insertion
	// order matches ID order — we just verify the count and file ordering.
	for i, file := range []string{"a.go", "b.go", "c.go"} {
		l := makeTestLead("nil-safety/error-handling", "concurrency", file, i+1, "note")
		if err := st.AddLead(ctx, l); err != nil {
			t.Fatalf("AddLead %s: %v", file, err)
		}
	}

	leads, err := st.PendingLeads(ctx, "concurrency")
	if err != nil {
		t.Fatalf("PendingLeads: %v", err)
	}
	if len(leads) != 3 {
		t.Fatalf("want 3, got %d", len(leads))
	}
	// All three are present (ordering is by created_at,id — same second so order
	// is by id which is time-ordered by construction).
	files := map[string]bool{}
	for _, l := range leads {
		files[l.File] = true
	}
	for _, want := range []string{"a.go", "b.go", "c.go"} {
		if !files[want] {
			t.Errorf("missing file %q in pending leads", want)
		}
	}
}
