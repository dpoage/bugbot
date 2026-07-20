package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
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

// TestSetPublishedManagedLabels_RoundTrip pins migration 027
// (published_managed_labels): a fresh openTemp store runs 001-027 in order,
// so managed_labels must exist, default to ” (read back as nil for rows
// that predate any Set), and round-trip sorted through both Get and List
// regardless of the caller's input order. A second row with its own labels
// pins the UPDATE's fingerprint scoping: Set on one fingerprint must not
// touch any other row.
func TestSetPublishedManagedLabels_RoundTrip(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	fp := "fp-labels"
	if err := st.UpsertPublishedIssue(ctx, fp, 11, IssueStateOpen, ""); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Bystander row with its own labels: must be untouched by Sets on fp.
	other := "fp-other"
	otherWant := []string{"bugbot:auto-filed", "severity:low"}
	if err := st.UpsertPublishedIssue(ctx, other, 12, IssueStateOpen, ""); err != nil {
		t.Fatalf("seed other: %v", err)
	}
	if err := st.SetPublishedManagedLabels(ctx, other, otherWant); err != nil {
		t.Fatalf("set other: %v", err)
	}

	// Pre-Set: the column's DEFAULT '' decodes to nil (legacy-row marker).
	pre, err := st.GetPublishedIssue(ctx, fp)
	if err != nil {
		t.Fatalf("pre-set get: %v", err)
	}
	if pre.ManagedLabels != nil {
		t.Errorf("pre-set ManagedLabels = %v, want nil", pre.ManagedLabels)
	}

	// Set in deliberately unsorted order; reads must come back sorted.
	input := []string{"severity:high", "bugbot:auto-filed"}
	if err := st.SetPublishedManagedLabels(ctx, fp, input); err != nil {
		t.Fatalf("set: %v", err)
	}
	want := []string{"bugbot:auto-filed", "severity:high"}

	// The caller's slice must not be mutated by the internal sort.
	if !slices.Equal(input, []string{"severity:high", "bugbot:auto-filed"}) {
		t.Errorf("caller slice mutated: %v", input)
	}

	got, err := st.GetPublishedIssue(ctx, fp)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !slices.Equal(got.ManagedLabels, want) {
		t.Errorf("Get ManagedLabels = %v, want %v", got.ManagedLabels, want)
	}

	list, err := st.ListPublishedIssues(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 rows, got %d: %+v", len(list), list)
	}
	// ORDER BY fingerprint ASC -> fp-labels, fp-other.
	if !slices.Equal(list[0].ManagedLabels, want) {
		t.Errorf("List ManagedLabels = %v, want %v", list[0].ManagedLabels, want)
	}

	// Fingerprint scoping: the bystander row's labels survived the Set on fp.
	if !slices.Equal(list[1].ManagedLabels, otherWant) {
		t.Errorf("bystander List ManagedLabels = %v, want %v", list[1].ManagedLabels, otherWant)
	}
	otherGot, err := st.GetPublishedIssue(ctx, other)
	if err != nil {
		t.Fatalf("get other: %v", err)
	}
	if !slices.Equal(otherGot.ManagedLabels, otherWant) {
		t.Errorf("bystander Get ManagedLabels = %v, want %v", otherGot.ManagedLabels, otherWant)
	}
}

// TestSetPublishedManagedLabels_PreservedAcrossUpsert verifies (not assumes)
// that UpsertPublishedIssue's ON CONFLICT clause does not touch
// managed_labels: a conflict upsert refreshes issue_number/state/body_hash/
// updated_at but the last-applied label bookkeeping survives, so the
// reconciler's diff basis is not wiped by an unrelated body push.
func TestSetPublishedManagedLabels_PreservedAcrossUpsert(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	fp := "fp-upsert-keep"
	if err := st.UpsertPublishedIssue(ctx, fp, 1, IssueStateOpen, "hash-a"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	want := []string{"bugbot:auto-filed", "severity:low"}
	if err := st.SetPublishedManagedLabels(ctx, fp, want); err != nil {
		t.Fatalf("set: %v", err)
	}

	// Conflict upsert on the same fingerprint.
	if err := st.UpsertPublishedIssue(ctx, fp, 2, IssueStateClosed, "hash-b"); err != nil {
		t.Fatalf("conflict upsert: %v", err)
	}

	got, err := st.GetPublishedIssue(ctx, fp)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !slices.Equal(got.ManagedLabels, want) {
		t.Errorf("ManagedLabels after upsert = %v, want %v", got.ManagedLabels, want)
	}
	// Sanity: the upsert itself still applied its own columns.
	if got.IssueNumber != 2 || got.State != IssueStateClosed || got.BodyHash != "hash-b" {
		t.Errorf("upsert columns not refreshed: %+v", got)
	}
}

// TestSetPublishedManagedLabels_ClearAndNoUpdatedAtBump covers the two
// bookkeeping subtleties: setting empty/nil clears the column back to ”
// (reads back nil), and Set never bumps updated_at — the planner compares
// finding.updated_at > published.updated_at and label bookkeeping must not
// masquerade as a body push.
func TestSetPublishedManagedLabels_ClearAndNoUpdatedAtBump(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	fp := "fp-clear"
	if err := st.UpsertPublishedIssue(ctx, fp, 3, IssueStateOpen, ""); err != nil {
		t.Fatalf("seed: %v", err)
	}
	before, err := st.GetPublishedIssue(ctx, fp)
	if err != nil {
		t.Fatalf("get before: %v", err)
	}

	// Clear with nil, then with an empty non-nil slice: both decode to nil.
	for _, clear := range [][]string{nil, {}} {
		if err := st.SetPublishedManagedLabels(ctx, fp, []string{"severity:medium"}); err != nil {
			t.Fatalf("set: %v", err)
		}
		if err := st.SetPublishedManagedLabels(ctx, fp, clear); err != nil {
			t.Fatalf("clear with %v: %v", clear, err)
		}
		got, err := st.GetPublishedIssue(ctx, fp)
		if err != nil {
			t.Fatalf("get after clear: %v", err)
		}
		if got.ManagedLabels != nil {
			t.Errorf("ManagedLabels after clear with %v = %v, want nil", clear, got.ManagedLabels)
		}
	}

	after, err := st.GetPublishedIssue(ctx, fp)
	if err != nil {
		t.Fatalf("get after: %v", err)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("SetPublishedManagedLabels bumped updated_at: %s -> %s", before.UpdatedAt, after.UpdatedAt)
	}
}

// TestSetPublishedManagedLabels_MissingFingerprint mirrors
// DeletePublishedIssue's idempotency: setting labels for a fingerprint with
// no published row is a nil no-op and must not create a row.
func TestSetPublishedManagedLabels_MissingFingerprint(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	if err := st.SetPublishedManagedLabels(ctx, "fp-ghost", []string{"severity:high"}); err != nil {
		t.Fatalf("set on missing fingerprint: %v", err)
	}
	if _, err := st.GetPublishedIssue(ctx, "fp-ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("get err = %v, want ErrNotFound (no row must be created)", err)
	}
	list, err := st.ListPublishedIssues(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if list != nil {
		t.Errorf("table not empty after no-op set: %+v", list)
	}
}

// TestPublishedIssue_IssueKeyTrackerDefaults pins migration 028
// (published_issue_key): a fresh openTemp store runs 001-028 in order, so
// issue_key and tracker must exist and read back at their column DEFAULTs
// for rows written through the existing API — UpsertPublishedIssue does not
// set them yet (the epic bugbot-6gfy cutover does). Two rows are written so
// the assertion covers both the Get and List scan paths on more than one row.
func TestPublishedIssue_IssueKeyTrackerDefaults(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	fps := []string{"fp-a", "fp-b"}
	for i, fp := range fps {
		if err := st.UpsertPublishedIssue(ctx, fp, i+1, IssueStateOpen, ""); err != nil {
			t.Fatalf("upsert %q: %v", fp, err)
		}
	}

	for _, fp := range fps {
		got, err := st.GetPublishedIssue(ctx, fp)
		if err != nil {
			t.Fatalf("get %q: %v", fp, err)
		}
		if got.IssueKey != "" {
			t.Errorf("%q IssueKey = %q, want empty (API writer does not set it yet)", fp, got.IssueKey)
		}
		if got.Tracker != "github" {
			t.Errorf("%q Tracker = %q, want %q (column DEFAULT)", fp, got.Tracker, "github")
		}
	}

	list, err := st.ListPublishedIssues(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(list))
	}
	for _, pi := range list {
		if pi.IssueKey != "" || pi.Tracker != "github" {
			t.Errorf("list row %q: IssueKey = %q, Tracker = %q; want \"\", \"github\"", pi.Fingerprint, pi.IssueKey, pi.Tracker)
		}
	}
}

// TestUpsertPublishedIssue_PreservesIssueKeyTracker verifies (not assumes)
// that UpsertPublishedIssue's ON CONFLICT clause does not touch issue_key or
// tracker — same discipline as managed_labels: a conflict upsert refreshes
// issue_number/state/body_hash/updated_at, but tracker identity set by a
// future keyed writer must survive an unrelated body push. The columns are
// set by raw UPDATE because no API writes them yet (that is the epic
// bugbot-6gfy cutover, not this slice). A bystander row with its own
// distinct issue_key/tracker pins the upsert's fingerprint scoping.
func TestUpsertPublishedIssue_PreservesIssueKeyTracker(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	fp := "fp-keyed"
	other := "fp-bystander"
	if err := st.UpsertPublishedIssue(ctx, fp, 1, IssueStateOpen, "hash-a"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := st.UpsertPublishedIssue(ctx, other, 12, IssueStateOpen, "hash-o"); err != nil {
		t.Fatalf("seed other: %v", err)
	}
	for _, row := range []struct{ fp, key, tracker string }{
		{fp, "PROJ-42", "jira"},
		{other, "999", "gitlab"},
	} {
		res, err := st.DB().ExecContext(ctx,
			`UPDATE published_issues SET issue_key = ?, tracker = ? WHERE fingerprint = ?`,
			row.key, row.tracker, row.fp)
		if err != nil {
			t.Fatalf("raw update %q: %v", row.fp, err)
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			t.Fatalf("raw update %q affected %d rows (err=%v), want 1", row.fp, n, err)
		}
	}
	otherBefore, err := st.GetPublishedIssue(ctx, other)
	if err != nil {
		t.Fatalf("get bystander before: %v", err)
	}

	// Conflict upsert on fp only.
	if err := st.UpsertPublishedIssue(ctx, fp, 2, IssueStateClosed, "hash-b"); err != nil {
		t.Fatalf("conflict upsert: %v", err)
	}

	got, err := st.GetPublishedIssue(ctx, fp)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.IssueKey != "PROJ-42" {
		t.Errorf("IssueKey after upsert = %q, want %q", got.IssueKey, "PROJ-42")
	}
	if got.Tracker != "jira" {
		t.Errorf("Tracker after upsert = %q, want %q", got.Tracker, "jira")
	}
	// Sanity: the upsert itself still applied its own columns.
	if got.IssueNumber != 2 || got.State != IssueStateClosed || got.BodyHash != "hash-b" {
		t.Errorf("upsert columns not refreshed: %+v", got)
	}

	// Bystander row untouched in full.
	otherGot, err := st.GetPublishedIssue(ctx, other)
	if err != nil {
		t.Fatalf("get bystander after: %v", err)
	}
	if otherGot.IssueKey != "999" || otherGot.Tracker != "gitlab" {
		t.Errorf("bystander IssueKey/Tracker = %q/%q, want 999/gitlab", otherGot.IssueKey, otherGot.Tracker)
	}
	if otherGot.IssueNumber != otherBefore.IssueNumber || otherGot.State != otherBefore.State ||
		otherGot.BodyHash != otherBefore.BodyHash || !otherGot.UpdatedAt.Equal(otherBefore.UpdatedAt) {
		t.Errorf("bystander row changed by upsert on %q:\n before: %+v\n after:  %+v", fp, otherBefore, otherGot)
	}
}

// TestMigration028_BackfillsIssueKeyFromIssueNumber is a real upgrade test
// for the 028 backfill: it builds a database at schema version 27 by applying
// the embedded migrations one at a time (applyMigration keeps the
// schema_migrations bookkeeping consistent, exactly as migrate() would),
// seeds published_issues rows that predate 028, then runs the real Open() —
// whose migrate() applies 028 — and asserts through the store API that
// issue_key == CAST(issue_number AS TEXT) and tracker == 'github' for every
// pre-existing row. Two rows with distinct numbers pin that the backfill
// UPDATE is table-wide, not accidentally scoped.
//
// NOTE for the 029 cutover (epic bugbot-6gfy): when issue_number is dropped,
// the seed INSERT below (valid at schema 27) still works, but the
// IssueNumber assertions must go.
func TestMigration028_BackfillsIssueKeyFromIssueNumber(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}

	// migrate() creates this table before applying anything; mirror it so
	// applyMigration's bookkeeping insert works and the later Open() resumes
	// from version 27 instead of re-running 001.
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			name       TEXT NOT NULL,
			applied_at TEXT NOT NULL
		)`); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}

	migs, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	saw028 := false
	for _, m := range migs {
		if m.version >= 28 {
			if m.version == 28 {
				saw028 = true
			}
			continue
		}
		if err := applyMigration(ctx, db, m); err != nil {
			t.Fatalf("apply %s: %v", m.name, err)
		}
	}
	if !saw028 {
		t.Fatal("embedded migrations lack version 028")
	}

	// Seed rows as they existed before 028: issue_number is the only issue
	// identity. body_hash (025) and managed_labels (027) fall back to their
	// DEFAULTs.
	now := nowUTC().Format(timeLayout)
	seeds := []struct {
		fp  string
		num int
	}{
		{"fp-old", 123},
		{"fp-bystander", 456},
	}
	for _, s := range seeds {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO published_issues (fingerprint, issue_number, state, created_at, updated_at)
			VALUES (?, ?, 'open', ?, ?)`,
			s.fp, s.num, now, now); err != nil {
			t.Fatalf("seed %q: %v", s.fp, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close pre-upgrade db: %v", err)
	}

	// The real upgrade path: Open() runs migrate(), which applies 028+.
	st, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open (upgrade): %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	for _, s := range seeds {
		got, err := st.GetPublishedIssue(ctx, s.fp)
		if err != nil {
			t.Fatalf("get %q: %v", s.fp, err)
		}
		if want := fmt.Sprintf("%d", s.num); got.IssueKey != want {
			t.Errorf("%q IssueKey = %q, want %q (backfill from issue_number)", s.fp, got.IssueKey, want)
		}
		if got.Tracker != "github" {
			t.Errorf("%q Tracker = %q, want %q", s.fp, got.Tracker, "github")
		}
		if got.IssueNumber != s.num {
			t.Errorf("%q IssueNumber = %d, want %d (028 is additive; number must survive)", s.fp, got.IssueNumber, s.num)
		}
	}
}
