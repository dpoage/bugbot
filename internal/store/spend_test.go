package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestScanRun_BeginFinishGet(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	id, err := st.BeginScanRun(ctx, ScanTargeted, "abc123")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	run, err := st.GetScanRun(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if run.Kind != ScanTargeted || run.CommitSHA != "abc123" {
		t.Fatalf("unexpected run: %+v", run)
	}
	if run.StartedAt.IsZero() {
		t.Fatal("started_at should be set")
	}
	if !run.FinishedAt.IsZero() {
		t.Fatal("finished_at should be zero before finish")
	}

	if err := st.FinishScanRun(ctx, id, `{"candidates":5}`); err != nil {
		t.Fatalf("finish: %v", err)
	}
	run, _ = st.GetScanRun(ctx, id)
	if run.FinishedAt.IsZero() {
		t.Fatal("finished_at should be set after finish")
	}
	if run.StatsJSON != `{"candidates":5}` {
		t.Fatalf("stats not stored: %q", run.StatsJSON)
	}
}

func TestScanRun_NotFound(t *testing.T) {
	st := openTemp(t)
	_, err := st.GetScanRun(context.Background(), "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if err := st.FinishScanRun(context.Background(), "nope", "{}"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("finish unknown: expected ErrNotFound, got %v", err)
	}
}

func TestSpend_RecordAndRollups(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	runA, _ := st.BeginScanRun(ctx, ScanSweep, "c1")
	runB, _ := st.BeginScanRun(ctx, ScanSweep, "c1")

	base := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)

	// Two entries for runA, one for runB.
	mustRecord(t, st, Spend{TS: base, ScanRunID: runA, Role: "finder", InputTokens: 100, OutputTokens: 20})
	mustRecord(t, st, Spend{TS: base.Add(time.Minute), ScanRunID: runA, Role: "verifier", InputTokens: 50, OutputTokens: 10})
	mustRecord(t, st, Spend{TS: base.Add(2 * time.Minute), ScanRunID: runB, Role: "finder", InputTokens: 200, OutputTokens: 30})

	// Per-cycle (per scan-run) rollup.
	totA, err := st.TotalsForScanRun(ctx, runA)
	if err != nil {
		t.Fatalf("totals run A: %v", err)
	}
	if totA.InputTokens != 150 || totA.OutputTokens != 30 {
		t.Fatalf("run A totals wrong: %+v", totA)
	}
	if totA.Total() != 180 {
		t.Fatalf("run A Total() = %d, want 180", totA.Total())
	}

	totB, _ := st.TotalsForScanRun(ctx, runB)
	if totB.Total() != 230 {
		t.Fatalf("run B Total() = %d, want 230", totB.Total())
	}

	// Per-day (time-windowed) rollup: everything since base = all three.
	since, err := st.TotalsSince(ctx, base)
	if err != nil {
		t.Fatalf("totals since: %v", err)
	}
	if since.Total() != 410 {
		t.Fatalf("TotalsSince(base) = %d, want 410", since.Total())
	}

	// Window that excludes the first entry.
	since2, _ := st.TotalsSince(ctx, base.Add(90*time.Second))
	if since2.Total() != 230 {
		t.Fatalf("TotalsSince(+90s) = %d, want 230", since2.Total())
	}

	// Window after everything: zero, not error.
	since3, _ := st.TotalsSince(ctx, base.Add(time.Hour))
	if since3.Total() != 0 {
		t.Fatalf("TotalsSince(future) = %d, want 0", since3.Total())
	}
}

func TestSpend_DefaultsTimestampAndID(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	id, err := st.RecordSpend(ctx, Spend{InputTokens: 1, OutputTokens: 1})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if id == "" {
		t.Fatal("expected generated spend id")
	}
	// ts defaulted to now, so a rollup since an hour ago must include it.
	tot, _ := st.TotalsSince(ctx, nowUTC().Add(-time.Hour))
	if tot.Total() != 2 {
		t.Fatalf("expected default-timestamped spend in window, got %d", tot.Total())
	}
}

func mustRecord(t *testing.T, st *Store, sp Spend) {
	t.Helper()
	if _, err := st.RecordSpend(context.Background(), sp); err != nil {
		t.Fatalf("record spend: %v", err)
	}
}
