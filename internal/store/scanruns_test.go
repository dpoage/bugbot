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

	id1, err := st.BeginScanRun(ctx, ScanSweep, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishScanRun(ctx, id1, "{}"); err != nil {
		t.Fatal(err)
	}
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
