package store

import (
	"context"
	"testing"
	"time"
)

// makeDeadHypothesis returns a minimal valid DeadHypothesis for use in tests.
func makeDeadHypothesis(scanRunID, fingerprint, title string, arbiterRan bool) DeadHypothesis {
	return DeadHypothesis{
		ScanRunID:    scanRunID,
		Fingerprint:  fingerprint,
		Lens:         "nil-safety/error-handling",
		File:         "pkg/foo.go",
		Line:         42,
		Title:        title,
		Severity:     "medium",
		SeatNames:    []string{"reachability", "semantics", "guards"},
		RefutedCount: 2,
		TotalSeats:   3,
		ArbiterRan:   arbiterRan,
	}
}

// TestDeadHypotheses_InsertListRoundTrip verifies that AddDeadHypothesis and
// ListDeadHypotheses faithfully round-trip all fields including the structured
// verdict breakdown (seat names, refuted count, arbiter verdict) and the
// free-text reasoning trace.
func TestDeadHypotheses_InsertListRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	scanRunID, err := st.BeginScanRun(ctx, ScanOneshot, "abc123")
	if err != nil {
		t.Fatalf("BeginScanRun: %v", err)
	}

	h := DeadHypothesis{
		ScanRunID:      scanRunID,
		Fingerprint:    "fp:abc",
		Lens:           "concurrency",
		File:           "internal/store/store.go",
		Line:           17,
		Title:          "race in scheduler loop",
		Severity:       "high",
		SeatNames:      []string{"reachability", "semantics"},
		RefutedCount:   2,
		TotalSeats:     2,
		ArbiterRan:     true,
		ArbiterRefuted: true,
		ArbiterVerdict: "refuted",
		ReasoningTrace: "Survived adversarial verification (split panel decided by arbitration, 0/2 refuter verdicts could not disprove it):\n  refuter 1 [reachability, refuted, confidence=high]: I read the code\n",
	}
	if err := st.AddDeadHypothesis(ctx, h); err != nil {
		t.Fatalf("AddDeadHypothesis: %v", err)
	}

	rows, err := st.ListDeadHypotheses(ctx, scanRunID)
	if err != nil {
		t.Fatalf("ListDeadHypotheses: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}

	got := rows[0]

	if got.ScanRunID != scanRunID {
		t.Errorf("ScanRunID = %q, want %q", got.ScanRunID, scanRunID)
	}
	if got.Fingerprint != "fp:abc" {
		t.Errorf("Fingerprint = %q, want fp:abc", got.Fingerprint)
	}
	if got.Lens != "concurrency" {
		t.Errorf("Lens = %q, want concurrency", got.Lens)
	}
	if got.File != "internal/store/store.go" {
		t.Errorf("File = %q, want internal/store/store.go", got.File)
	}
	if got.Line != 17 {
		t.Errorf("Line = %d, want 17", got.Line)
	}
	if got.Title != "race in scheduler loop" {
		t.Errorf("Title = %q, want race in scheduler loop", got.Title)
	}
	if got.Severity != "high" {
		t.Errorf("Severity = %q, want high", got.Severity)
	}
	if len(got.SeatNames) != 2 || got.SeatNames[0] != "reachability" || got.SeatNames[1] != "semantics" {
		t.Errorf("SeatNames = %v, want [reachability semantics]", got.SeatNames)
	}
	if got.RefutedCount != 2 {
		t.Errorf("RefutedCount = %d, want 2", got.RefutedCount)
	}
	if got.TotalSeats != 2 {
		t.Errorf("TotalSeats = %d, want 2", got.TotalSeats)
	}
	if !got.ArbiterRan {
		t.Error("ArbiterRan = false, want true")
	}
	if !got.ArbiterRefuted {
		t.Error("ArbiterRefuted = false, want true")
	}
	if got.ArbiterVerdict != "refuted" {
		t.Errorf("ArbiterVerdict = %q, want refuted", got.ArbiterVerdict)
	}
	if got.ReasoningTrace != h.ReasoningTrace {
		t.Errorf("ReasoningTrace mismatch:\n got: %q\nwant: %q", got.ReasoningTrace, h.ReasoningTrace)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}
}

// TestDeadHypotheses_NoArbiterPath verifies that the no-arbiter round-trip
// (majorityRefuted unanimous kill) leaves arbiter_ran=0 / arbiter_verdict=”
// and the structured fields are still queryable. This is the common case for
// a small panel.
func TestDeadHypotheses_NoArbiterPath(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	scanRunID, err := st.BeginScanRun(ctx, ScanOneshot, "abc123")
	if err != nil {
		t.Fatalf("BeginScanRun: %v", err)
	}

	h := makeDeadHypothesis(scanRunID, "fp:xyz", "nil-deref on shutdown", false)
	// ArbiterRan is false: no override needed.
	h.ArbiterVerdict = "" // explicit: no arbiter
	h.RefutedCount = 3    // unanimous

	if err := st.AddDeadHypothesis(ctx, h); err != nil {
		t.Fatalf("AddDeadHypothesis: %v", err)
	}

	rows, err := st.ListDeadHypotheses(ctx, scanRunID)
	if err != nil {
		t.Fatalf("ListDeadHypotheses: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	got := rows[0]
	if got.ArbiterRan {
		t.Error("ArbiterRan = true, want false")
	}
	if got.ArbiterRefuted {
		t.Error("ArbiterRefuted = true, want false")
	}
	if got.ArbiterVerdict != "" {
		t.Errorf("ArbiterVerdict = %q, want empty", got.ArbiterVerdict)
	}
	if got.RefutedCount != 3 {
		t.Errorf("RefutedCount = %d, want 3", got.RefutedCount)
	}
}

// TestDeadHypotheses_Prune verifies the prune semantics with EXACTLY 4 scan
// runs and keepRuns=2: the 2 oldest runs' rows are deleted and the 2 newest
// kept. Both directions are asserted (deletion AND retention).
func TestDeadHypotheses_Prune(t *testing.T) {
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

	// Create 4 scan runs and add one dead_hypotheses row per run.
	runIDs := make([]string, 4)
	for i := range runIDs {
		id, err := st.BeginScanRun(ctx, ScanOneshot, "sha"+string(rune('0'+i)))
		if err != nil {
			t.Fatalf("BeginScanRun %d: %v", i, err)
		}
		runIDs[i] = id
		h := makeDeadHypothesis(id, "fp:run"+string(rune('0'+i)), "kill in run "+string(rune('0'+i)), false)
		if err := st.AddDeadHypothesis(ctx, h); err != nil {
			t.Fatalf("AddDeadHypothesis run %d: %v", i, err)
		}
	}

	deleted, err := st.PruneDeadHypotheses(ctx, 2)
	if err != nil {
		t.Fatalf("PruneDeadHypotheses: %v", err)
	}
	if deleted != 2 {
		t.Errorf("PruneDeadHypotheses deleted %d rows, want 2 (the 2 oldest runs)", deleted)
	}

	// The 2 oldest runs (runIDs[0], runIDs[1]) must have no rows.
	for i := 0; i < 2; i++ {
		rows, err := st.ListDeadHypotheses(ctx, runIDs[i])
		if err != nil {
			t.Fatalf("ListDeadHypotheses old run %d: %v", i, err)
		}
		if len(rows) != 0 {
			t.Errorf("old run %d (%s): expected 0 rows after prune, got %d", i, runIDs[i], len(rows))
		}
	}

	// The 2 newest runs (runIDs[2], runIDs[3]) must still have their rows.
	for i := 2; i < 4; i++ {
		rows, err := st.ListDeadHypotheses(ctx, runIDs[i])
		if err != nil {
			t.Fatalf("ListDeadHypotheses new run %d: %v", i, err)
		}
		if len(rows) != 1 {
			t.Errorf("new run %d (%s): expected 1 row after prune, got %d", i, runIDs[i], len(rows))
		}
	}
}
