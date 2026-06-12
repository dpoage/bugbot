package store

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"
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

// epochSentinelTime returns the parsed epoch sentinel for assertions.
func epochSentinelTimeParsed() time.Time {
	t, err := time.Parse(time.RFC3339, epochSentinel)
	if err != nil {
		panic("test: failed to parse epoch sentinel: " + err.Error())
	}
	return t
}

// TestRefreshContentHashes_PreservesLastScannedAt verifies that
// RefreshContentHashes does NOT overwrite an existing last_scanned_at set by
// TouchScanCoverage.
func TestRefreshContentHashes_PreservesLastScannedAt(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	// First: TouchScanCoverage sets a truthful scan time.
	if err := st.TouchScanCoverage(ctx, []string{"a.go"}, "c1", nil); err != nil {
		t.Fatalf("TouchScanCoverage: %v", err)
	}
	before, err := st.GetFileState(ctx, "a.go")
	if err != nil {
		t.Fatalf("GetFileState before refresh: %v", err)
	}

	// RefreshContentHashes should NOT overwrite last_scanned_at.
	if err := st.RefreshContentHashes(ctx, []FileState{
		{Path: "a.go", ContentHash: "newhash", LastScannedCommit: "c2"},
	}); err != nil {
		t.Fatalf("RefreshContentHashes: %v", err)
	}

	after, err := st.GetFileState(ctx, "a.go")
	if err != nil {
		t.Fatalf("GetFileState after refresh: %v", err)
	}

	if !after.LastScannedAt.Equal(before.LastScannedAt) {
		t.Errorf("last_scanned_at clobbered: before=%v after=%v", before.LastScannedAt, after.LastScannedAt)
	}
	if after.ContentHash != "newhash" {
		t.Errorf("ContentHash not updated: got %q, want newhash", after.ContentHash)
	}
}

// TestRefreshContentHashes_NewRowGetsEpoch verifies that a new row inserted by
// RefreshContentHashes gets the epoch sentinel for last_scanned_at.
func TestRefreshContentHashes_NewRowGetsEpoch(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	if err := st.RefreshContentHashes(ctx, []FileState{
		{Path: "new.go", ContentHash: "h1", LastScannedCommit: "c1"},
	}); err != nil {
		t.Fatalf("RefreshContentHashes: %v", err)
	}

	fs, err := st.GetFileState(ctx, "new.go")
	if err != nil {
		t.Fatalf("GetFileState: %v", err)
	}

	epoch := epochSentinelTimeParsed()
	if !fs.LastScannedAt.Equal(epoch) {
		t.Errorf("new row LastScannedAt = %v, want epoch sentinel %v", fs.LastScannedAt, epoch)
	}
}

// TestTouchScanCoverage_UpdatesOnlyNamedRows verifies that TouchScanCoverage
// only updates the explicitly named rows and leaves others unchanged.
func TestTouchScanCoverage_UpdatesOnlyNamedRows(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	// Insert two rows via RefreshContentHashes (epoch sentinel).
	if err := st.RefreshContentHashes(ctx, []FileState{
		{Path: "a.go", ContentHash: "ha", LastScannedCommit: "c1"},
		{Path: "b.go", ContentHash: "hb", LastScannedCommit: "c1"},
	}); err != nil {
		t.Fatalf("RefreshContentHashes: %v", err)
	}

	epoch := epochSentinelTimeParsed()

	// Touch only a.go.
	if err := st.TouchScanCoverage(ctx, []string{"a.go"}, "c1", nil); err != nil {
		t.Fatalf("TouchScanCoverage: %v", err)
	}

	a, err := st.GetFileState(ctx, "a.go")
	if err != nil {
		t.Fatalf("GetFileState a.go: %v", err)
	}
	if a.LastScannedAt.Equal(epoch) {
		t.Errorf("a.go last_scanned_at still at epoch; expected it to be updated")
	}

	b, err := st.GetFileState(ctx, "b.go")
	if err != nil {
		t.Fatalf("GetFileState b.go: %v", err)
	}
	if !b.LastScannedAt.Equal(epoch) {
		t.Errorf("b.go last_scanned_at = %v, want epoch (not covered)", b.LastScannedAt)
	}
}

// TestTouchScanCoverage_RecordsAndPreservesHashes verifies the content-hash
// semantics: a supplied hash is written (insert and update), and a missing /
// empty hash never clobbers an existing stored hash — so a failed fingerprint
// computation degrades to "treated as changed next sweep" rather than
// corrupting good state.
func TestTouchScanCoverage_RecordsAndPreservesHashes(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	// Insert with a hash: written.
	if err := st.TouchScanCoverage(ctx, []string{"a.go"}, "c1", map[string]string{"a.go": "h1"}); err != nil {
		t.Fatalf("TouchScanCoverage insert: %v", err)
	}
	a, err := st.GetFileState(ctx, "a.go")
	if err != nil {
		t.Fatalf("GetFileState: %v", err)
	}
	if a.ContentHash != "h1" {
		t.Errorf("ContentHash after insert = %q, want h1", a.ContentHash)
	}

	// Update with nil hashes: timestamp advances, hash preserved.
	if err := st.TouchScanCoverage(ctx, []string{"a.go"}, "c2", nil); err != nil {
		t.Fatalf("TouchScanCoverage nil-hash update: %v", err)
	}
	a2, err := st.GetFileState(ctx, "a.go")
	if err != nil {
		t.Fatalf("GetFileState: %v", err)
	}
	if a2.ContentHash != "h1" {
		t.Errorf("ContentHash after nil-hash touch = %q, want h1 (must not clobber)", a2.ContentHash)
	}
	if !a2.LastScannedAt.After(a.LastScannedAt) && !a2.LastScannedAt.Equal(a.LastScannedAt) {
		t.Errorf("last_scanned_at went backwards: %v -> %v", a.LastScannedAt, a2.LastScannedAt)
	}
	if a2.LastScannedCommit != "c2" {
		t.Errorf("LastScannedCommit = %q, want c2", a2.LastScannedCommit)
	}

	// Update with a new hash: overwritten.
	if err := st.TouchScanCoverage(ctx, []string{"a.go"}, "c3", map[string]string{"a.go": "h2"}); err != nil {
		t.Fatalf("TouchScanCoverage new-hash update: %v", err)
	}
	a3, err := st.GetFileState(ctx, "a.go")
	if err != nil {
		t.Fatalf("GetFileState: %v", err)
	}
	if a3.ContentHash != "h2" {
		t.Errorf("ContentHash after new-hash touch = %q, want h2", a3.ContentHash)
	}
}

// TestTouchScanCoverage_InsertsAbsentRow verifies that TouchScanCoverage
// inserts a new row when the path does not exist yet.
func TestTouchScanCoverage_InsertsAbsentRow(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	if err := st.TouchScanCoverage(ctx, []string{"brand-new.go"}, "c1", nil); err != nil {
		t.Fatalf("TouchScanCoverage on absent row: %v", err)
	}

	fs, err := st.GetFileState(ctx, "brand-new.go")
	if err != nil {
		t.Fatalf("GetFileState: %v", err)
	}

	epoch := epochSentinelTimeParsed()
	if fs.LastScannedAt.Equal(epoch) {
		t.Errorf("inserted row LastScannedAt still at epoch; expected a real timestamp")
	}
	if fs.LastScannedCommit != "c1" {
		t.Errorf("LastScannedCommit = %q, want c1", fs.LastScannedCommit)
	}
}

// TestScanWatermarks_BatchRead verifies that LastScannedAt returns timestamps
// for known rows and omits absent ones.
func TestScanWatermarks_BatchRead(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	if err := st.RefreshContentHashes(ctx, []FileState{
		{Path: "x.go", ContentHash: "hx", LastScannedCommit: "c1"},
	}); err != nil {
		t.Fatalf("RefreshContentHashes: %v", err)
	}
	if err := st.TouchScanCoverage(ctx, []string{"y.go"}, "c1", nil); err != nil {
		t.Fatalf("TouchScanCoverage: %v", err)
	}

	got, err := st.ScanWatermarks(ctx, []string{"x.go", "y.go", "absent.go"})
	if err != nil {
		t.Fatalf("ScanWatermarks: %v", err)
	}

	if _, ok := got["x.go"]; !ok {
		t.Error("x.go missing from ScanWatermarks result")
	}
	if _, ok := got["y.go"]; !ok {
		t.Error("y.go missing from ScanWatermarks result")
	}
	if _, ok := got["absent.go"]; ok {
		t.Error("absent.go should not be in ScanWatermarks result")
	}
}

// TestScanWatermarks_EmptyInput verifies that ScanWatermarks with empty input
// returns nil (no error).
func TestScanWatermarks_EmptyInput(t *testing.T) {
	st := openTemp(t)
	got, err := st.ScanWatermarks(context.Background(), nil)
	if err != nil {
		t.Fatalf("ScanWatermarks nil: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil for empty input, got %v", got)
	}
}

// TestChunkStrings_LargeInput verifies that chunkStrings stays under
// sqliteMaxVars and produces no overlapping or missing elements.
func TestChunkStrings_LargeInput(t *testing.T) {
	n := sqliteMaxVars*3 + 7
	s := make([]string, n)
	for i := range s {
		s[i] = "file"
	}
	chunks := chunkStrings(s, sqliteMaxVars)
	total := 0
	for _, c := range chunks {
		if len(c) > sqliteMaxVars {
			t.Errorf("chunk size %d exceeds sqliteMaxVars %d", len(c), sqliteMaxVars)
		}
		total += len(c)
	}
	if total != n {
		t.Errorf("chunkStrings total = %d, want %d", total, n)
	}
}
