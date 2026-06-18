package store

import (
	"context"
	"testing"
)

func TestRunMetrics(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	// Run 1: cartographer OFF. 2 verified of 5 hypothesized, 1000 total tokens.
	id1, err := st.BeginScanRun(ctx, ScanOneshot, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishScanRun(ctx, id1, `{"hypothesized":5,"verified":2,"killed":1,"input_tokens":900,"output_tokens":100,"cartographer_enabled":false}`); err != nil {
		t.Fatal(err)
	}
	// Run 2 (started later): cartographer ON. 3 verified, 2000 total tokens.
	id2, err := st.BeginScanRun(ctx, ScanSweep, "c2")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishScanRun(ctx, id2, `{"hypothesized":4,"verified":3,"finder_runs":8,"input_tokens":1900,"output_tokens":100,"cartographer_enabled":true}`); err != nil {
		t.Fatal(err)
	}
	// An unfinished run must be excluded (no finished_at).
	if _, err := st.BeginScanRun(ctx, ScanOneshot, "c3"); err != nil {
		t.Fatal(err)
	}

	got, err := st.RunMetrics(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("RunMetrics returned %d rows, want 2 (unfinished run excluded)", len(got))
	}
	// Newest-first ordering: run 2 started after run 1.
	if got[0].ScanRunID != id2 || got[1].ScanRunID != id1 {
		t.Fatalf("ordering = [%s,%s], want newest-first [%s,%s]", got[0].ScanRunID, got[1].ScanRunID, id2, id1)
	}
	if !got[0].StartedAt.After(got[1].StartedAt) && !got[0].StartedAt.Equal(got[1].StartedAt) {
		t.Errorf("rows not ordered by started_at desc: %v then %v", got[0].StartedAt, got[1].StartedAt)
	}

	on := got[0]
	if !on.CartographerEnabled || on.Verified != 3 || on.FinderRuns != 8 || on.TotalTokens() != 2000 {
		t.Errorf("ON run = %+v, want carto=true verified=3 finder_runs=8 total=2000", on)
	}
	if got := on.VerifiedPer1K(); got != 1.5 {
		t.Errorf("ON VerifiedPer1K = %v, want 1.5", got)
	}
	off := got[1]
	if off.CartographerEnabled || off.Verified != 2 || off.Hypothesized != 5 || off.Killed != 1 || off.TotalTokens() != 1000 {
		t.Errorf("OFF run = %+v, want carto=false verified=2 hyp=5 killed=1 total=1000", off)
	}
	if got := off.VerifiedPer1K(); got != 2.0 {
		t.Errorf("OFF VerifiedPer1K = %v, want 2.0", got)
	}
}

// TestRunMetrics_UnparseableStatsKeepsRun pins the "never drop a run" contract:
// a finished run with garbage stats_json is still returned, with zero counters.
func TestRunMetrics_UnparseableStatsKeepsRun(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)
	id, err := st.BeginScanRun(ctx, ScanOneshot, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishScanRun(ctx, id, `not-json`); err != nil {
		t.Fatal(err)
	}
	got, err := st.RunMetrics(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ScanRunID != id {
		t.Fatalf("RunMetrics = %+v, want one row for %s", got, id)
	}
	if got[0].Verified != 0 || got[0].TotalTokens() != 0 {
		t.Errorf("unparseable stats should yield zero counters, got %+v", got[0])
	}
}
