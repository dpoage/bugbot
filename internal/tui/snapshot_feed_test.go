package tui

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/store"
)

// testStore opens a fresh on-disk store in a temp dir, mirroring
// internal/store's own openTemp helper (unexported there, so duplicated).
func testStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, dir
}

// seedScan seeds one scan_run with an agent_units row, a finding, and a lead,
// returning the scan run id.
func seedScan(t *testing.T, st *store.Store) string {
	t.Helper()
	ctx := context.Background()

	runID, err := st.BeginScanRun(ctx, store.ScanSweep, "deadbeef")
	if err != nil {
		t.Fatalf("BeginScanRun: %v", err)
	}
	if err := st.FinishScanRun(ctx, runID, "{}"); err != nil {
		t.Fatalf("FinishScanRun: %v", err)
	}

	unit := store.AgentUnit{
		ScanRunID:    runID,
		Role:         store.AgentRoleFinder,
		Lens:         "nil-safety",
		Strategy:     "error-handling",
		LaunchOrder:  0,
		Files:        []string{"internal/x/y.go"},
		StartedAt:    time.Now().Add(-time.Minute),
		FinishedAt:   time.Now(),
		Status:       store.AgentStatusOK,
		InputTokens:  1000,
		OutputTokens: 200,
		Candidates:   2,
	}
	if err := st.AddAgentUnit(ctx, unit); err != nil {
		t.Fatalf("AddAgentUnit: %v", err)
	}

	f := domain.Finding{
		Fingerprint: domain.Fingerprint("nil-safety", "internal/x/y.go", "10|nil deref"),
		Title:       "nil deref",
		Description: "dereferences without a nil check",
		Severity:    "high",
		Tier:        domain.Tier(2),
		Status:      domain.StatusOpen,
		Lens:        "nil-safety",
		File:        "internal/x/y.go",
		Line:        10,
		CommitSHA:   "deadbeef",
		FileHash:    "abc123",
	}
	if _, err := st.UpsertFinding(ctx, f); err != nil {
		t.Fatalf("UpsertFinding: %v", err)
	}

	if err := st.AddLead(ctx, store.Lead{
		ScanRunID:  runID,
		PosterLens: "nil-safety",
		TargetLens: "concurrency",
		File:       "internal/x/y.go",
		Line:       20,
		Note:       "check this too",
		Confidence: 0.8,
	}); err != nil {
		t.Fatalf("AddLead: %v", err)
	}

	return runID
}

func writeStatusJSON(t *testing.T, dir string, st progress.Status) {
	t.Helper()
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	path := progress.StatusPath(dir)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("write status.json: %v", err)
	}
}

// TestSnapshotFeed_BuildFrame_NoStore verifies a missing state DB degrades to
// an empty world state without creating anything on disk.
func TestSnapshotFeed_BuildFrame_NoStore(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Storage.Path = filepath.Join(dir, "state.db")

	f, err := NewSnapshotFeed(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewSnapshotFeed: %v", err)
	}
	defer f.Close()

	if _, err := os.Stat(cfg.Storage.Path); err == nil {
		t.Fatal("NewSnapshotFeed must not create the state DB when absent")
	}

	fr := f.buildFrame(context.Background())
	if fr.HasSnapshot {
		t.Error("HasSnapshot = true, want false (no status.json)")
	}
	if !fr.Stale {
		t.Error("Stale = false, want true (no snapshot => static)")
	}
	if fr.World.HasTallies {
		t.Error("World.HasTallies = true, want false (no store)")
	}
	if len(fr.Agents) != 0 {
		t.Errorf("Agents = %v, want empty", fr.Agents)
	}
}

// TestSnapshotFeed_BuildFrame_WorldStateAndAgents seeds a store with a
// finished scan run (agent_units row, finding, lead) and a fresh status.json
// with one live agent, then asserts the built Frame merges both.
func TestSnapshotFeed_BuildFrame_WorldStateAndAgents(t *testing.T) {
	// Build the store directly at the expected path so SnapshotFeed opens it.
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	st, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	seedScan(t, st)
	if err := st.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}

	now := time.Now()
	writeStatusJSON(t, dir, progress.Status{
		PID:         os.Getpid(),
		StartedAt:   now.Add(-time.Minute),
		LastUpdated: now,
		ScanKind:    "sweep",
		Stage:       "verify",
		ActiveAgents: []progress.AgentStatus{
			{Role: "verifier", Label: "nil deref candidate", Started: now.Add(-30 * time.Second), Activity: "reading file"},
		},
	})

	cfg := config.Default()
	cfg.Storage.Path = dbPath

	f, err := NewSnapshotFeed(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewSnapshotFeed: %v", err)
	}
	defer f.Close()

	fr := f.buildFrame(context.Background())

	if !fr.HasSnapshot {
		t.Fatal("HasSnapshot = false, want true")
	}
	if fr.Stale {
		t.Error("Stale = true, want false (fresh status.json, live pid)")
	}
	if !fr.World.HasTallies || fr.World.Tallies.OpenByTier[2] != 1 {
		t.Errorf("World.Tallies = %+v, want one T2 open finding", fr.World.Tallies)
	}
	if fr.World.PendingLeadsTotal != 1 {
		t.Errorf("PendingLeadsTotal = %d, want 1", fr.World.PendingLeadsTotal)
	}
	if !fr.World.HasLastRun {
		t.Fatal("HasLastRun = false, want true")
	}

	if len(fr.Agents) != 2 {
		t.Fatalf("Agents = %d, want 2 (1 historical + 1 live), got %+v", len(fr.Agents), fr.Agents)
	}
	var sawHistorical, sawLive bool
	for _, a := range fr.Agents {
		if a.Live && a.Role == "verifier" {
			sawLive = true
		}
		if !a.Live && a.Role == string(store.AgentRoleFinder) && a.Lens == "nil-safety" {
			sawHistorical = true
			if a.Status != string(store.AgentStatusOK) {
				t.Errorf("historical Status = %q, want %q", a.Status, store.AgentStatusOK)
			}
		}
	}
	if !sawHistorical {
		t.Error("missing merged historical agent_units row")
	}
	if !sawLive {
		t.Error("missing merged live agent from status.json")
	}
}

// TestSnapshotFeed_StaleSnapshot_DropsLiveAgents verifies a stale status.json
// (old LastUpdated) is reflected in Stale=true and live agents are dropped
// from the merge, leaving only historical rows.
func TestSnapshotFeed_StaleSnapshot_DropsLiveAgents(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	st, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	seedScan(t, st)
	if err := st.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}

	writeStatusJSON(t, dir, progress.Status{
		PID:         os.Getpid(),
		LastUpdated: time.Now().Add(-time.Hour), // way past staleAfter
		ActiveAgents: []progress.AgentStatus{
			{Role: "verifier", Label: "should be dropped", Started: time.Now()},
		},
	})

	cfg := config.Default()
	cfg.Storage.Path = dbPath

	f, err := NewSnapshotFeed(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewSnapshotFeed: %v", err)
	}
	defer f.Close()

	fr := f.buildFrame(context.Background())
	if !fr.Stale {
		t.Error("Stale = false, want true")
	}
	for _, a := range fr.Agents {
		if a.Live {
			t.Errorf("stale frame kept a live agent: %+v", a)
		}
	}
	if len(fr.Agents) != 1 {
		t.Errorf("Agents = %d, want 1 (historical only)", len(fr.Agents))
	}
}
