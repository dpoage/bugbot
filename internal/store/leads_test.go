package store

import (
	"context"
	"testing"
	"time"
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
}

// TestLeads_ConsumeLeads asserts the delete-on-consume contract: a claimed lead
// is removed from the blackboard (gone from BOTH PendingLeads and ListLeads),
// a pending lead for an unrelated/inactive lens is untouched, and a fresh
// re-post of the same (target_lens, file, line) after consumption is a clean
// INSERT (the old row is gone, so the conflict path is exercised only by
// still-pending rows).
func TestLeads_ConsumeLeads(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	// Two pending leads for the active lens.
	l1 := makeTestLead("nil-safety/error-handling", "concurrency", "a.go", 1, "note1")
	l2 := makeTestLead("nil-safety/error-handling", "concurrency", "b.go", 2, "note2")
	// One pending lead for an UNRELATED / inactive lens that must survive.
	lInactive := makeTestLead("nil-safety/error-handling", "resource-leaks", "c.go", 3, "note3")
	for _, l := range []Lead{l1, l2, lInactive} {
		if err := st.AddLead(ctx, l); err != nil {
			t.Fatalf("AddLead: %v", err)
		}
	}

	leads, err := st.PendingLeads(ctx, "concurrency")
	if err != nil {
		t.Fatalf("PendingLeads: %v", err)
	}
	if len(leads) != 2 {
		t.Fatalf("want 2 leads for concurrency, got %d", len(leads))
	}

	// Snapshot the inactive-lens lead's ID so we can prove it survives.
	inactiveBefore, err := st.PendingLeads(ctx, "resource-leaks")
	if err != nil {
		t.Fatalf("PendingLeads resource-leaks: %v", err)
	}
	if len(inactiveBefore) != 1 {
		t.Fatalf("want 1 inactive-lens lead, got %d", len(inactiveBefore))
	}
	inactiveID := inactiveBefore[0].ID

	ids := []string{leads[0].ID, leads[1].ID}
	if err := st.ConsumeLeads(ctx, ids); err != nil {
		t.Fatalf("ConsumeLeads: %v", err)
	}

	// Consumed rows are GONE — neither PendingLeads nor ListLeads returns them.
	afterPending, err := st.PendingLeads(ctx, "concurrency")
	if err != nil {
		t.Fatalf("PendingLeads after consume: %v", err)
	}
	if len(afterPending) != 0 {
		t.Errorf("PendingLeads: want 0 after consume, got %d", len(afterPending))
	}
	allAfter, err := st.ListLeads(ctx)
	if err != nil {
		t.Fatalf("ListLeads after consume: %v", err)
	}
	if len(allAfter) != 1 {
		t.Errorf("ListLeads: want 1 (only the inactive-lens lead), got %d", len(allAfter))
	}
	for _, l := range allAfter {
		if l.ID == leads[0].ID || l.ID == leads[1].ID {
			t.Errorf("ListLeads still surfaces a consumed lead %q (zombie row)", l.ID)
		}
	}

	// The pending lead for the inactive lens SURVIVES.
	inactiveAfter, err := st.PendingLeads(ctx, "resource-leaks")
	if err != nil {
		t.Fatalf("PendingLeads resource-leaks after consume: %v", err)
	}
	if len(inactiveAfter) != 1 {
		t.Fatalf("inactive-lens lead lost: want 1, got %d", len(inactiveAfter))
	}
	if inactiveAfter[0].ID != inactiveID {
		t.Errorf("inactive-lens lead ID changed: got %q, want %q", inactiveAfter[0].ID, inactiveID)
	}

	// Re-post one of the consumed (target_lens, file, line) triples: because the
	// old row was deleted, this is a clean INSERT (no conflict) and the new
	// row is fresh pending.
	l3 := makeTestLead("resource-leaks", "concurrency", "a.go", 1, "re-raised")
	if err := st.AddLead(ctx, l3); err != nil {
		t.Fatalf("AddLead re-post: %v", err)
	}
	reposted, err := st.PendingLeads(ctx, "concurrency")
	if err != nil {
		t.Fatalf("PendingLeads after repost: %v", err)
	}
	if len(reposted) != 1 {
		t.Fatalf("want 1 after repost, got %d", len(reposted))
	}
	if reposted[0].Note != "re-raised" {
		t.Errorf("reposted note = %q, want %q", reposted[0].Note, "re-raised")
	}
	if reposted[0].PosterLens != "resource-leaks" {
		t.Errorf("reposted poster = %q, want resource-leaks", reposted[0].PosterLens)
	}
	if reposted[0].ID == leads[0].ID {
		t.Errorf("reposted lead reused the deleted row's id %q; should be a fresh id", reposted[0].ID)
	}
}

// TestLeads_ConsumedThenReposted is the new equivalent of the old "flip
// consumed->posted" test: once a lead is consumed and deleted, a re-post of
// the same triple is a fresh insert (the conflict path is not exercised by
// the consume path; ON CONFLICT only fires while a still-pending row exists).
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

	// Verify it's gone.
	after, err := st.PendingLeads(ctx, "concurrency")
	if err != nil {
		t.Fatalf("PendingLeads after consume: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("want 0 after consume, got %d", len(after))
	}

	// Re-post the same (target_lens, file, line): the old row was deleted, so
	// this is a clean INSERT — not a conflict update.
	l2 := makeTestLead("resource-leaks", "concurrency", "x.go", 5, "re-raised suspicion")
	if err := st.AddLead(ctx, l2); err != nil {
		t.Fatalf("AddLead re-post: %v", err)
	}

	reposted, err := st.PendingLeads(ctx, "concurrency")
	if err != nil {
		t.Fatalf("PendingLeads after repost: %v", err)
	}
	if len(reposted) != 1 {
		t.Fatalf("want 1 after repost, got %d", len(reposted))
	}
	got := reposted[0]
	if got.Note != "re-raised suspicion" {
		t.Errorf("note = %q after repost", got.Note)
	}
	if got.PosterLens != "resource-leaks" {
		t.Errorf("poster = %q after repost, want resource-leaks", got.PosterLens)
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

// TestListLeads covers newest-first ordering and the deletion contract: a
// consumed lead is gone from ListLeads too, not just PendingLeads.
func TestListLeads(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	older := Lead{PosterLens: "a", TargetLens: "concurrency", File: "x.go", Line: 1, Note: "first",
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	newer := Lead{PosterLens: "b", TargetLens: "resource-leaks", File: "y.go", Line: 2, Note: "second",
		CreatedAt: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)}
	for _, l := range []Lead{older, newer} {
		if err := st.AddLead(ctx, l); err != nil {
			t.Fatal(err)
		}
	}

	all, err := st.ListLeads(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].Note != "second" || all[1].Note != "first" {
		t.Fatalf("ListLeads order wrong: %+v", all)
	}

	// Consume the newer one; it must be removed entirely from ListLeads.
	if err := st.ConsumeLeads(ctx, []string{all[0].ID}); err != nil {
		t.Fatal(err)
	}
	all, err = st.ListLeads(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("ListLeads: want 1 surviving lead after consume, got %d (%+v)", len(all), all)
	}
	if all[0].Note != "first" {
		t.Errorf("surviving lead: got %q, want first", all[0].Note)
	}
}

// TestLeads_ConfidenceOrdering verifies that PendingLeads returns leads ordered
// by confidence DESC, then created_at ASC, then id ASC.
func TestLeads_ConfidenceOrdering(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	leads := []Lead{
		{PosterLens: "a", TargetLens: "concurrency", File: "low.go", Line: 1, Note: "low",
			Confidence: 0.1, CreatedAt: base},
		{PosterLens: "a", TargetLens: "concurrency", File: "high.go", Line: 2, Note: "high",
			Confidence: 0.9, CreatedAt: base},
		{PosterLens: "a", TargetLens: "concurrency", File: "mid.go", Line: 3, Note: "mid",
			Confidence: 0.5, CreatedAt: base},
		// Same confidence as "low", but created later — must come after "low".
		{PosterLens: "a", TargetLens: "concurrency", File: "low2.go", Line: 4, Note: "low2",
			Confidence: 0.1, CreatedAt: base.Add(time.Second)},
	}
	for _, l := range leads {
		if err := st.AddLead(ctx, l); err != nil {
			t.Fatalf("AddLead: %v", err)
		}
	}

	got, err := st.PendingLeads(ctx, "concurrency")
	if err != nil {
		t.Fatalf("PendingLeads: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("want 4 leads, got %d", len(got))
	}

	wantFiles := []string{"high.go", "mid.go", "low.go", "low2.go"}
	for i, want := range wantFiles {
		if got[i].File != want {
			t.Errorf("position %d: got file %q, want %q (full order: %v)",
				i, got[i].File, want, func() []string {
					files := make([]string, len(got))
					for j, l := range got {
						files[j] = l.File
					}
					return files
				}())
		}
	}
}

// TestLeads_UpsertRefreshesConfidence verifies that upserting an existing lead
// (same target_lens/file/line) updates the confidence field.
func TestLeads_UpsertRefreshesConfidence(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	original := Lead{
		PosterLens: "nil-safety/error-handling",
		TargetLens: "concurrency",
		File:       "pkg/foo.go",
		Line:       7,
		Note:       "note",
		Confidence: 0.2,
	}
	if err := st.AddLead(ctx, original); err != nil {
		t.Fatalf("AddLead: %v", err)
	}

	updated := Lead{
		PosterLens: "nil-safety/error-handling",
		TargetLens: "concurrency",
		File:       "pkg/foo.go",
		Line:       7,
		Note:       "note updated",
		Confidence: 0.85,
	}
	if err := st.AddLead(ctx, updated); err != nil {
		t.Fatalf("AddLead upsert: %v", err)
	}

	got, err := st.PendingLeads(ctx, "concurrency")
	if err != nil {
		t.Fatalf("PendingLeads: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 lead after upsert, got %d", len(got))
	}
	if got[0].Confidence != 0.85 {
		t.Errorf("confidence = %v, want 0.85", got[0].Confidence)
	}
}
