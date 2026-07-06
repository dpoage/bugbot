package store

import (
	"context"
	"testing"
	"time"
)

// TestLatestScanRun covers the most-recent selection and ErrNotFound.
func TestLatestScanRun(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	if _, err := st.LatestScanRun(ctx); err != ErrNotFound {
		t.Fatalf("empty store: err = %v, want ErrNotFound", err)
	}

	// Pin nowUTC and advance by 1 ms between calls so each ULID has a
	// distinct millisecond timestamp — ORDER BY id DESC is reliable when the
	// timestamp prefix differs (identical prefix + random suffix may sort
	// in arbitrary order within the same millisecond).
	current := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	orig := nowUTC
	nowUTC = func() time.Time { return current }
	defer func() { nowUTC = orig }()

	id1, err := st.BeginScanRun(ctx, ScanSweep, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishScanRun(ctx, id1, "{}"); err != nil {
		t.Fatal(err)
	}
	current = current.Add(time.Millisecond)
	id2, err := st.BeginScanRun(ctx, ScanTargeted, "c2")
	if err != nil {
		t.Fatal(err)
	}

	got, err := st.LatestScanRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != id2 || got.Kind != ScanTargeted {
		t.Errorf("latest = %+v, want id %s (targeted)", got, id2)
	}
}

// TestBeginScanRun_RecordsPIDAndHeartbeat verifies that a new scan run
// records the current process PID and sets an initial heartbeat equal to
// (or very close to) the started_at time.
func TestBeginScanRun_RecordsPIDAndHeartbeat(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	before := time.Now().UTC().Add(-time.Second)
	id, err := st.BeginScanRun(ctx, ScanOneshot, "abc123")
	if err != nil {
		t.Fatal(err)
	}

	run, err := st.GetScanRun(ctx, id)
	if err != nil {
		t.Fatal(err)
	}

	if run.PID == 0 {
		t.Error("BeginScanRun: PID = 0, want current process pid")
	}
	if run.Heartbeat.IsZero() {
		t.Error("BeginScanRun: Heartbeat is zero, want initial heartbeat")
	}
	if run.Heartbeat.Before(before) {
		t.Errorf("BeginScanRun: Heartbeat %v is before test start %v", run.Heartbeat, before)
	}
}

// TestUpdateHeartbeat verifies that UpdateHeartbeat moves the heartbeat
// timestamp forward in time.
func TestUpdateHeartbeat(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	id, err := st.BeginScanRun(ctx, ScanOneshot, "abc")
	if err != nil {
		t.Fatal(err)
	}

	before, err := st.GetScanRun(ctx, id)
	if err != nil {
		t.Fatal(err)
	}

	// Advance nowUTC so the updated heartbeat is observably later.
	orig := nowUTC
	t.Cleanup(func() { nowUTC = orig })
	later := before.Heartbeat.Add(5 * time.Minute)
	nowUTC = func() time.Time { return later }

	if err := st.UpdateHeartbeat(ctx, id); err != nil {
		t.Fatal(err)
	}

	after, err := st.GetScanRun(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Heartbeat.Equal(later) {
		t.Errorf("UpdateHeartbeat: heartbeat = %v, want %v", after.Heartbeat, later)
	}
}

// TestActiveScanRuns_LiveRunIsReturned verifies that an unfinished run with a
// fresh heartbeat and a different PID appears in ActiveScanRuns.
func TestActiveScanRuns_LiveRunIsReturned(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	id, err := st.BeginScanRun(ctx, ScanOneshot, "abc")
	if err != nil {
		t.Fatal(err)
	}

	// Patch pid to something that is not ours.
	_, err = st.DB().ExecContext(ctx, `UPDATE scan_runs SET pid = 99999 WHERE id = ?`, id)
	if err != nil {
		t.Fatal(err)
	}

	runs, err := st.ActiveScanRuns(ctx, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != id {
		t.Errorf("ActiveScanRuns = %v, want [{ID: %s}]", runs, id)
	}
	if runs[0].PID != 99999 {
		t.Errorf("ActiveScanRuns: PID = %d, want 99999", runs[0].PID)
	}
}

// TestActiveScanRuns_FinishedRunExcluded verifies that a finished run does not
// appear in ActiveScanRuns even when the heartbeat is fresh.
func TestActiveScanRuns_FinishedRunExcluded(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	id, err := st.BeginScanRun(ctx, ScanOneshot, "abc")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishScanRun(ctx, id, "{}"); err != nil {
		t.Fatal(err)
	}

	runs, err := st.ActiveScanRuns(ctx, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Errorf("ActiveScanRuns after finish: got %d runs, want 0", len(runs))
	}
}

// TestActiveScanRuns_StaleHeartbeatExcluded verifies that a run whose
// heartbeat is older than staleAfter is NOT returned (the process is
// presumed crashed).
func TestActiveScanRuns_StaleHeartbeatExcluded(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	id, err := st.BeginScanRun(ctx, ScanOneshot, "abc")
	if err != nil {
		t.Fatal(err)
	}

	// Back-date the heartbeat to 20 minutes ago (well past the 10-min window).
	stale := time.Now().UTC().Add(-20 * time.Minute).Format(timeLayout)
	_, err = st.DB().ExecContext(ctx, `UPDATE scan_runs SET heartbeat = ? WHERE id = ?`, stale, id)
	if err != nil {
		t.Fatal(err)
	}

	runs, err := st.ActiveScanRuns(ctx, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Errorf("ActiveScanRuns with stale heartbeat: got %d runs, want 0", len(runs))
	}
}

// TestActiveScanRuns_NullHeartbeatExcluded verifies that a pre-011 row (NULL
// heartbeat) is not returned even if unfinished.
func TestActiveScanRuns_NullHeartbeatExcluded(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	id, err := st.BeginScanRun(ctx, ScanSweep, "abc")
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a pre-011 row by clearing the heartbeat.
	_, err = st.DB().ExecContext(ctx, `UPDATE scan_runs SET heartbeat = NULL WHERE id = ?`, id)
	if err != nil {
		t.Fatal(err)
	}

	runs, err := st.ActiveScanRuns(ctx, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Errorf("ActiveScanRuns with NULL heartbeat: got %d runs, want 0", len(runs))
	}
}

// TestLastFinishedSweepCommit covers the 'last green' baseline query: only
// FINISHED SWEEP runs are candidates, the most recent wins, excludeID drops the
// in-flight run, and non-sweep runs are ignored.
func TestLastFinishedSweepCommit(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	// Pin nowUTC and advance 1 ms between BeginScanRun calls so each ULID has a
	// distinct millisecond timestamp — ORDER BY id DESC is reliable when the
	// timestamp prefix differs (identical prefix + random suffix may sort in
	// arbitrary order within the same millisecond).
	current := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	orig := nowUTC
	nowUTC = func() time.Time { return current }
	defer func() { nowUTC = orig }()

	// No runs at all -> ErrNotFound.
	if _, err := st.LastFinishedSweepCommit(ctx, ""); err != ErrNotFound {
		t.Fatalf("empty store: err = %v, want ErrNotFound", err)
	}

	// An unfinished sweep is not a candidate.
	cur, err := st.BeginScanRun(ctx, ScanSweep, "head-sha")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.LastFinishedSweepCommit(ctx, cur); err != ErrNotFound {
		t.Fatalf("only-unfinished sweep: err = %v, want ErrNotFound", err)
	}

	// A finished sweep at commit A becomes the baseline.
	current = current.Add(time.Millisecond)
	s1, err := st.BeginScanRun(ctx, ScanSweep, "sha-A")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishScanRun(ctx, s1, "{}"); err != nil {
		t.Fatal(err)
	}
	if got, err := st.LastFinishedSweepCommit(ctx, cur); err != nil || got != "sha-A" {
		t.Fatalf("after one finished sweep: got %q, err %v; want sha-A", got, err)
	}

	// A later finished sweep at commit B supersedes A.
	current = current.Add(time.Millisecond)
	s2, err := st.BeginScanRun(ctx, ScanSweep, "sha-B")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishScanRun(ctx, s2, "{}"); err != nil {
		t.Fatal(err)
	}
	if got, err := st.LastFinishedSweepCommit(ctx, cur); err != nil || got != "sha-B" {
		t.Fatalf("after two finished sweeps: got %q, err %v; want sha-B", got, err)
	}

	// excludeID drops the in-flight run: excluding B's run falls back to A.
	if got, err := st.LastFinishedSweepCommit(ctx, s2); err != nil || got != "sha-A" {
		t.Fatalf("excluding B's run: got %q, err %v; want sha-A", got, err)
	}

	// A finished NON-sweep run (targeted) is never a baseline.
	current = current.Add(time.Millisecond)
	tg, err := st.BeginScanRun(ctx, ScanTargeted, "sha-T")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishScanRun(ctx, tg, "{}"); err != nil {
		t.Fatal(err)
	}
	if got, err := st.LastFinishedSweepCommit(ctx, cur); err != nil || got != "sha-B" {
		t.Fatalf("after a targeted run: got %q, err %v; want sha-B (targeted ignored)", got, err)
	}
}
