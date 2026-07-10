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
