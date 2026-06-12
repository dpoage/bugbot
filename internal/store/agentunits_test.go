package store

import (
	"context"
	"testing"
	"time"
)

// makeUnit returns a minimal valid AgentUnit for use in tests.
func makeUnit(scanRunID, role, lens, strategy, status string, launchOrder int) AgentUnit {
	return AgentUnit{
		ScanRunID:   scanRunID,
		Role:        role,
		Lens:        lens,
		Strategy:    strategy,
		LaunchOrder: launchOrder,
		Status:      status,
		Files:       []string{"pkg/foo.go", "pkg/bar.go"},
	}
}

// TestAgentUnits_InsertListRoundTrip verifies that AddAgentUnit and
// ListAgentUnits faithfully round-trip all fields including files_json with
// multiple files, token counts, and the detail string.
func TestAgentUnits_InsertListRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	// Seed a scan run so the scan_run_id exists.
	scanRunID, err := st.BeginScanRun(ctx, ScanOneshot, "abc123")
	if err != nil {
		t.Fatalf("BeginScanRun: %v", err)
	}

	now := nowUTC().Truncate(time.Second) // truncate for stable comparison

	u := AgentUnit{
		ScanRunID:       scanRunID,
		Role:            "finder",
		Lens:            "nil-safety/error-handling",
		Strategy:        "sweep-wide",
		LaunchOrder:     0,
		Files:           []string{"cmd/main.go", "internal/store/store.go", "pkg/foo.go"},
		StartedAt:       now,
		FinishedAt:      now.Add(5 * time.Second),
		Status:          "ok",
		InputTokens:     1000,
		OutputTokens:    200,
		CacheReadTokens: 800,
		Candidates:      3,
		LeadsPosted:     1,
		Detail:          "some detail note",
	}

	if err := st.AddAgentUnit(ctx, u); err != nil {
		t.Fatalf("AddAgentUnit: %v", err)
	}

	units, err := st.ListAgentUnits(ctx, scanRunID)
	if err != nil {
		t.Fatalf("ListAgentUnits: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("got %d units, want 1", len(units))
	}

	got := units[0]

	if got.ScanRunID != scanRunID {
		t.Errorf("ScanRunID = %q, want %q", got.ScanRunID, scanRunID)
	}
	if got.Role != "finder" {
		t.Errorf("Role = %q, want finder", got.Role)
	}
	if got.Lens != "nil-safety/error-handling" {
		t.Errorf("Lens = %q, want nil-safety/error-handling", got.Lens)
	}
	if got.Strategy != "sweep-wide" {
		t.Errorf("Strategy = %q, want sweep-wide", got.Strategy)
	}
	if got.LaunchOrder != 0 {
		t.Errorf("LaunchOrder = %d, want 0", got.LaunchOrder)
	}
	if len(got.Files) != 3 || got.Files[0] != "cmd/main.go" || got.Files[1] != "internal/store/store.go" || got.Files[2] != "pkg/foo.go" {
		t.Errorf("Files = %v, want [cmd/main.go internal/store/store.go pkg/foo.go]", got.Files)
	}
	if !got.StartedAt.Equal(now) {
		t.Errorf("StartedAt = %v, want %v", got.StartedAt, now)
	}
	if !got.FinishedAt.Equal(now.Add(5 * time.Second)) {
		t.Errorf("FinishedAt = %v, want %v", got.FinishedAt, now.Add(5*time.Second))
	}
	if got.Status != "ok" {
		t.Errorf("Status = %q, want ok", got.Status)
	}
	if got.InputTokens != 1000 {
		t.Errorf("InputTokens = %d, want 1000", got.InputTokens)
	}
	if got.OutputTokens != 200 {
		t.Errorf("OutputTokens = %d, want 200", got.OutputTokens)
	}
	if got.CacheReadTokens != 800 {
		t.Errorf("CacheReadTokens = %d, want 800", got.CacheReadTokens)
	}
	if got.Candidates != 3 {
		t.Errorf("Candidates = %d, want 3", got.Candidates)
	}
	if got.LeadsPosted != 1 {
		t.Errorf("LeadsPosted = %d, want 1", got.LeadsPosted)
	}
	if got.Detail != "some detail note" {
		t.Errorf("Detail = %q, want 'some detail note'", got.Detail)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}
}

// TestAgentUnits_SkippedUnitZeroTokens verifies that a skipped unit (empty
// started_at, zero tokens) round-trips correctly — the primary use case for
// "what did the budget never reach".
func TestAgentUnits_SkippedUnitZeroTokens(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	scanRunID, err := st.BeginScanRun(ctx, ScanOneshot, "abc123")
	if err != nil {
		t.Fatalf("BeginScanRun: %v", err)
	}

	skipped := AgentUnit{
		ScanRunID:   scanRunID,
		Role:        "finder",
		Lens:        "concurrency",
		Strategy:    "sweep-wide",
		LaunchOrder: 2,
		Files:       []string{"pkg/worker.go"},
		// StartedAt and FinishedAt deliberately zero (skipped before launch).
		Status: "skipped_hard_budget",
		// All token fields deliberately zero.
	}
	if err := st.AddAgentUnit(ctx, skipped); err != nil {
		t.Fatalf("AddAgentUnit skipped: %v", err)
	}

	units, err := st.ListAgentUnits(ctx, scanRunID)
	if err != nil {
		t.Fatalf("ListAgentUnits: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("got %d units, want 1", len(units))
	}

	got := units[0]
	if got.StartedAt != (time.Time{}) {
		t.Errorf("StartedAt should be zero for skipped unit, got %v", got.StartedAt)
	}
	if got.FinishedAt != (time.Time{}) {
		t.Errorf("FinishedAt should be zero for skipped unit, got %v", got.FinishedAt)
	}
	if got.InputTokens != 0 || got.OutputTokens != 0 || got.CacheReadTokens != 0 {
		t.Errorf("token counts should be zero for skipped unit: in=%d out=%d cached=%d",
			got.InputTokens, got.OutputTokens, got.CacheReadTokens)
	}
	if got.Status != "skipped_hard_budget" {
		t.Errorf("Status = %q, want skipped_hard_budget", got.Status)
	}
}

// TestAgentUnits_ListOrderedByLaunchOrder verifies that ListAgentUnits returns
// rows in ascending launch_order, not insertion order.
func TestAgentUnits_ListOrderedByLaunchOrder(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	scanRunID, err := st.BeginScanRun(ctx, ScanOneshot, "abc123")
	if err != nil {
		t.Fatalf("BeginScanRun: %v", err)
	}

	// Insert in reverse order to prove ORDER BY launch_order works.
	for order := 4; order >= 0; order-- {
		u := makeUnit(scanRunID, "finder", "nil-safety/error-handling", "sweep-wide", "ok", order)
		if err := st.AddAgentUnit(ctx, u); err != nil {
			t.Fatalf("AddAgentUnit order=%d: %v", order, err)
		}
	}

	units, err := st.ListAgentUnits(ctx, scanRunID)
	if err != nil {
		t.Fatalf("ListAgentUnits: %v", err)
	}
	if len(units) != 5 {
		t.Fatalf("got %d units, want 5", len(units))
	}
	for i, u := range units {
		if u.LaunchOrder != i {
			t.Errorf("units[%d].LaunchOrder = %d, want %d", i, u.LaunchOrder, i)
		}
	}
}

// TestAgentUnits_Prune verifies the prune semantics with EXACTLY 4 scan runs
// and keepRuns=2: the 2 oldest runs' rows are deleted and the 2 newest kept.
// Both directions are asserted (deletion AND retention).
func TestAgentUnits_Prune(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	// Pin nowUTC so runs get strictly ordered started_at values.
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	step := 0
	orig := nowUTC
	nowUTC = func() time.Time {
		t := base.Add(time.Duration(step) * time.Second)
		step++
		return t
	}
	defer func() { nowUTC = orig }()

	// Create 4 scan runs and add one agent_unit per run.
	runIDs := make([]string, 4)
	for i := range runIDs {
		id, err := st.BeginScanRun(ctx, ScanOneshot, "sha"+string(rune('0'+i)))
		if err != nil {
			t.Fatalf("BeginScanRun %d: %v", i, err)
		}
		runIDs[i] = id
		u := makeUnit(id, "finder", "nil-safety/error-handling", "sweep-wide", "ok", 0)
		if err := st.AddAgentUnit(ctx, u); err != nil {
			t.Fatalf("AddAgentUnit run %d: %v", i, err)
		}
	}

	// Prune keeping only the 2 most recent runs.
	deleted, err := st.PruneAgentUnits(ctx, 2)
	if err != nil {
		t.Fatalf("PruneAgentUnits: %v", err)
	}
	if deleted != 2 {
		t.Errorf("PruneAgentUnits deleted %d rows, want 2 (the 2 oldest runs)", deleted)
	}

	// The 2 oldest runs (runIDs[0], runIDs[1]) must have no rows.
	for i := 0; i < 2; i++ {
		units, err := st.ListAgentUnits(ctx, runIDs[i])
		if err != nil {
			t.Fatalf("ListAgentUnits old run %d: %v", i, err)
		}
		if len(units) != 0 {
			t.Errorf("old run %d (%s): expected 0 rows after prune, got %d", i, runIDs[i], len(units))
		}
	}

	// The 2 newest runs (runIDs[2], runIDs[3]) must still have their rows.
	for i := 2; i < 4; i++ {
		units, err := st.ListAgentUnits(ctx, runIDs[i])
		if err != nil {
			t.Fatalf("ListAgentUnits new run %d: %v", i, err)
		}
		if len(units) != 1 {
			t.Errorf("new run %d (%s): expected 1 row after prune, got %d", i, runIDs[i], len(units))
		}
	}
}

// TestAgentUnits_Prune_SubSecondBoundary is the regression test for the
// RFC3339Nano lexicographic-ordering trap: a run started at a fractional
// second ("...17.5Z") sorts lexicographically BEFORE a strictly older run at
// the whole second ("...17Z", because 'Z' > '.'), so a prune that computes
// recency via ORDER BY started_at silently keeps the wrong run. Recency must
// come from the ULID-style id instead.
func TestAgentUnits_Prune_SubSecondBoundary(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	// Pin nowUTC to a settable clock so each run's id AND started_at derive
	// from the same instant.
	current := time.Date(2025, 1, 1, 0, 0, 17, 0, time.UTC)
	orig := nowUTC
	nowUTC = func() time.Time { return current }
	defer func() { nowUTC = orig }()

	begin := func(sha string) string {
		t.Helper()
		id, err := st.BeginScanRun(ctx, ScanOneshot, sha)
		if err != nil {
			t.Fatalf("BeginScanRun %s: %v", sha, err)
		}
		if err := st.AddAgentUnit(ctx, makeUnit(id, "finder", "nil-safety/error-handling", "sweep-wide", "ok", 0)); err != nil {
			t.Fatalf("AddAgentUnit %s: %v", sha, err)
		}
		return id
	}

	// Creation order: A at :17.0 ("...17Z"), B at :17.5 ("...17.5Z"),
	// C at :18.0. Lexicographically "...17Z" > "...17.5Z", so a
	// started_at-ordered prune with keep=2 would keep {C, A} and wrongly
	// delete B — the truly 2nd-most-recent run.
	runA := begin("shaA")
	current = current.Add(500 * time.Millisecond)
	runB := begin("shaB")
	current = current.Add(500 * time.Millisecond)
	runC := begin("shaC")

	if _, err := st.PruneAgentUnits(ctx, 2); err != nil {
		t.Fatalf("PruneAgentUnits: %v", err)
	}

	for id, wantRows := range map[string]int{runA: 0, runB: 1, runC: 1} {
		units, err := st.ListAgentUnits(ctx, id)
		if err != nil {
			t.Fatalf("ListAgentUnits %s: %v", id, err)
		}
		if len(units) != wantRows {
			t.Errorf("run %s: %d rows after prune, want %d (sub-second boundary mis-sort)", id, len(units), wantRows)
		}
	}
}
