package store

import (
	"context"
	"testing"
	"time"
)

// makeFinderTraversal returns a minimal valid FinderTraversal for use in tests.
func makeFinderTraversal(scanRunID, lens, strategy string, candidateCount int) FinderTraversal {
	return FinderTraversal{
		ScanRunID:      scanRunID,
		Lens:           lens,
		Strategy:       strategy,
		Files:          []string{"pkg/foo.go", "pkg/bar.go"},
		Enumerated:     []string{"(*Foo).Bar", "(*Baz).Qux"},
		Visited:        []string{"(*Foo).Bar"},
		CandidateCount: candidateCount,
	}
}

// TestFinderTraversals_InsertListRoundTrip verifies that AddFinderTraversal and
// ListFinderTraversals faithfully round-trip all fields including the JSON-stored
// slices (Files, Enumerated, Visited) and the candidate count.
func TestFinderTraversals_InsertListRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	scanRunID, err := st.BeginScanRun(ctx, ScanOneshot, "abc123")
	if err != nil {
		t.Fatalf("BeginScanRun: %v", err)
	}

	ft := FinderTraversal{
		ScanRunID:      scanRunID,
		Lens:           "contract-trace-deep",
		Strategy:       "deep",
		Files:          []string{"internal/store/store.go", "internal/store/ops.go"},
		Enumerated:     []string{"(*Store).exec", "(*Store).retry", "queryRows"},
		Visited:        []string{"(*Store).exec", "(*Store).retry"},
		CandidateCount: 0,
	}
	if err := st.AddFinderTraversal(ctx, ft); err != nil {
		t.Fatalf("AddFinderTraversal: %v", err)
	}

	rows, err := st.ListFinderTraversals(ctx, scanRunID)
	if err != nil {
		t.Fatalf("ListFinderTraversals: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}

	got := rows[0]

	if got.ScanRunID != scanRunID {
		t.Errorf("ScanRunID = %q, want %q", got.ScanRunID, scanRunID)
	}
	if got.Lens != "contract-trace-deep" {
		t.Errorf("Lens = %q, want contract-trace-deep", got.Lens)
	}
	if got.Strategy != "deep" {
		t.Errorf("Strategy = %q, want deep", got.Strategy)
	}
	if len(got.Files) != 2 || got.Files[0] != "internal/store/store.go" || got.Files[1] != "internal/store/ops.go" {
		t.Errorf("Files = %v, want [internal/store/store.go internal/store/ops.go]", got.Files)
	}
	if len(got.Enumerated) != 3 || got.Enumerated[0] != "(*Store).exec" {
		t.Errorf("Enumerated = %v, want 3 items starting with (*Store).exec", got.Enumerated)
	}
	if len(got.Visited) != 2 || got.Visited[0] != "(*Store).exec" || got.Visited[1] != "(*Store).retry" {
		t.Errorf("Visited = %v, want [(*Store).exec (*Store).retry]", got.Visited)
	}
	if got.CandidateCount != 0 {
		t.Errorf("CandidateCount = %d, want 0", got.CandidateCount)
	}
	if got.ID == "" {
		t.Error("ID is empty (should be generated)")
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}
}

// TestFinderTraversals_EmptySlicesRoundTrip verifies that nil/empty Enumerated
// and Visited slices round-trip cleanly (stored as "[]", returned as nil).
func TestFinderTraversals_EmptySlicesRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	scanRunID, err := st.BeginScanRun(ctx, ScanOneshot, "def456")
	if err != nil {
		t.Fatalf("BeginScanRun: %v", err)
	}

	ft := FinderTraversal{
		ScanRunID:      scanRunID,
		Lens:           "nil-safety/error-handling",
		Strategy:       "",
		Files:          []string{"a.go"},
		Enumerated:     nil,
		Visited:        nil,
		CandidateCount: 2,
	}
	if err := st.AddFinderTraversal(ctx, ft); err != nil {
		t.Fatalf("AddFinderTraversal: %v", err)
	}

	rows, err := st.ListFinderTraversals(ctx, scanRunID)
	if err != nil {
		t.Fatalf("ListFinderTraversals: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	got := rows[0]
	// nil slices come back as nil (unmarshalStringSlice short-circuits on "[]").
	if got.Enumerated != nil {
		t.Errorf("Enumerated = %v, want nil for empty input", got.Enumerated)
	}
	if got.Visited != nil {
		t.Errorf("Visited = %v, want nil for empty input", got.Visited)
	}
	if got.CandidateCount != 2 {
		t.Errorf("CandidateCount = %d, want 2", got.CandidateCount)
	}
}

// TestFinderTraversals_Prune verifies the prune semantics with EXACTLY 4 scan
// runs and keepRuns=2: the 2 oldest runs' rows are deleted and the 2 newest
// kept. Both directions are asserted (deletion AND retention).
func TestFinderTraversals_Prune(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	// Pin nowUTC so runs get strictly ordered ids.
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	step := 0
	orig := nowUTC
	nowUTC = func() time.Time {
		t := base.Add(time.Duration(step) * time.Second)
		step++
		return t
	}
	defer func() { nowUTC = orig }()

	// Create 4 scan runs and add one finder_traversals row per run.
	runIDs := make([]string, 4)
	for i := range runIDs {
		id, err := st.BeginScanRun(ctx, ScanOneshot, "sha"+string(rune('0'+i)))
		if err != nil {
			t.Fatalf("BeginScanRun %d: %v", i, err)
		}
		runIDs[i] = id
		ft := makeFinderTraversal(id, "nil-safety/error-handling", "default", i)
		if err := st.AddFinderTraversal(ctx, ft); err != nil {
			t.Fatalf("AddFinderTraversal run %d: %v", i, err)
		}
	}

	deleted, err := st.PruneFinderTraversals(ctx, 2)
	if err != nil {
		t.Fatalf("PruneFinderTraversals: %v", err)
	}
	if deleted != 2 {
		t.Errorf("PruneFinderTraversals deleted %d rows, want 2 (the 2 oldest runs)", deleted)
	}

	// The 2 oldest runs (runIDs[0], runIDs[1]) must have no rows.
	for i := 0; i < 2; i++ {
		rows, err := st.ListFinderTraversals(ctx, runIDs[i])
		if err != nil {
			t.Fatalf("ListFinderTraversals old run %d: %v", i, err)
		}
		if len(rows) != 0 {
			t.Errorf("old run %d (%s): expected 0 rows after prune, got %d", i, runIDs[i], len(rows))
		}
	}

	// The 2 newest runs (runIDs[2], runIDs[3]) must still have their rows.
	for i := 2; i < 4; i++ {
		rows, err := st.ListFinderTraversals(ctx, runIDs[i])
		if err != nil {
			t.Fatalf("ListFinderTraversals new run %d: %v", i, err)
		}
		if len(rows) != 1 {
			t.Errorf("new run %d (%s): expected 1 row after prune, got %d", i, runIDs[i], len(rows))
		}
	}
}

// TestFinderTraversals_Prune_TimestampDivergence verifies that
// PruneFinderTraversals uses id order (ULID), NOT started_at order, to
// determine recency. If an implementation were to switch to started_at, the
// wrong rows would be evicted when started_at and id order diverge.
//
// Setup: create 3 runs in id order (R0 < R1 < R2), then patch started_at so
// that started_at DESC picks R0 and R1 (R0 gets '...T00:00:10Z', R1 gets
// '...T00:00:09Z', R2 gets '...T00:00:01.5Z'). keepRuns=2 must retain R1 and
// R2 (newest BY ID); a started_at impl would wrongly retain R0 and R1.
func TestFinderTraversals_Prune_TimestampDivergence(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	// Pin nowUTC so runs get strictly ordered ids (different milliseconds).
	base := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	step := 0
	orig := nowUTC
	nowUTC = func() time.Time {
		ts := base.Add(time.Duration(step) * time.Millisecond)
		step++
		return ts
	}
	defer func() { nowUTC = orig }()

	// Create 3 runs: R0 (oldest id), R1, R2 (newest id).
	runIDs := make([]string, 3)
	for i := range runIDs {
		id, err := st.BeginScanRun(ctx, ScanOneshot, "sha"+string(rune('0'+i)))
		if err != nil {
			t.Fatalf("BeginScanRun %d: %v", i, err)
		}
		runIDs[i] = id
		ft := makeFinderTraversal(id, "nil-safety/error-handling", "default", i)
		if err := st.AddFinderTraversal(ctx, ft); err != nil {
			t.Fatalf("AddFinderTraversal run %d: %v", i, err)
		}
	}

	// Patch started_at so id-order and started_at-order DIVERGE:
	//   R0 (oldest id) → '...T00:00:10Z'  — largest timestamp; first in started_at DESC
	//   R1 (middle id) → '...T00:00:09Z'  — second in started_at DESC
	//   R2 (newest id) → '...T00:00:01.5Z' — fractional; last in started_at DESC
	// started_at DESC keepRuns=2 → keep R0, R1 (WRONG).
	// id       DESC keepRuns=2 → keep R1, R2 (CORRECT).
	patches := map[int]string{
		0: "2025-06-01T00:00:10Z",
		1: "2025-06-01T00:00:09Z",
		2: "2025-06-01T00:00:01.5Z",
	}
	for i, ts := range patches {
		if _, err := st.DB().ExecContext(ctx,
			`UPDATE scan_runs SET started_at = ? WHERE id = ?`, ts, runIDs[i]); err != nil {
			t.Fatalf("patch started_at R%d: %v", i, err)
		}
	}

	deleted, err := st.PruneFinderTraversals(ctx, 2)
	if err != nil {
		t.Fatalf("PruneFinderTraversals: %v", err)
	}
	if deleted != 1 {
		t.Errorf("PruneFinderTraversals deleted %d rows, want 1 (only R0 evicted)", deleted)
	}

	// R0 (oldest id) must have no rows.
	rows0, err := st.ListFinderTraversals(ctx, runIDs[0])
	if err != nil {
		t.Fatalf("ListFinderTraversals R0: %v", err)
	}
	if len(rows0) != 0 {
		t.Errorf("R0 (oldest id): expected 0 rows after prune, got %d", len(rows0))
	}

	// R1 and R2 (two newest by id) must still have their rows.
	for i, id := range runIDs[1:] {
		rows, err := st.ListFinderTraversals(ctx, id)
		if err != nil {
			t.Fatalf("ListFinderTraversals R%d: %v", i+1, err)
		}
		if len(rows) != 1 {
			t.Errorf("R%d (id=%s): expected 1 row after prune, got %d", i+1, id, len(rows))
		}
	}
}
