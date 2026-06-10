package progress

import (
	"path/filepath"
	"testing"
	"time"
)

// newTestSnapshot builds a snapshot sink writing into a temp dir with an
// injectable clock, returning the sink and its status path.
func newTestSnapshot(t *testing.T, now func() time.Time) (*SnapshotSink, string) {
	t.Helper()
	dir := t.TempDir()
	s := NewSnapshotSink(dir)
	s.now = now
	return s, filepath.Join(dir, StatusFileName)
}

func TestSnapshot_WritesAndReadsBack(t *testing.T) {
	s, path := newTestSnapshot(t, func() time.Time { return time.Unix(2000, 0) })

	s.Handle(Event{Kind: KindScanStarted, ScanKind: "sweep", Commit: "deadbeef", Time: time.Unix(2000, 0)})
	s.Handle(Event{Kind: KindAgentStarted, Role: RoleFinder, Label: "lensA", Time: time.Unix(2000, 0)})
	// Terminal event forces a write.
	s.Handle(Event{Kind: KindScanFinished, ScanKind: "sweep", Counts: &Counts{Verified: 2}, InputTokens: 11, OutputTokens: 7, Time: time.Unix(2000, 0)})

	st, err := ReadStatus(path)
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if st.ScanKind != "sweep" || st.Commit != "deadbeef" {
		t.Errorf("scan identity not persisted: %+v", st)
	}
	if st.Counts.Verified != 2 {
		t.Errorf("counts.verified = %d, want 2", st.Counts.Verified)
	}
	if st.SpendInput != 11 || st.SpendOutput != 7 {
		t.Errorf("spend = in:%d out:%d, want 11/7", st.SpendInput, st.SpendOutput)
	}
	if st.PID <= 0 {
		t.Errorf("pid not stamped: %d", st.PID)
	}
	if st.LastUpdated.IsZero() {
		t.Errorf("last_updated not set")
	}
}

func TestSnapshot_RateLimitsWrites(t *testing.T) {
	// Clock advances by less than snapshotInterval between events; only the first
	// (lastWrite zero) and any terminal write should hit disk.
	clock := time.Unix(5000, 0)
	now := func() time.Time { return clock }
	s, path := newTestSnapshot(t, now)

	// First non-terminal event writes (lastWrite zero).
	s.Handle(Event{Kind: KindStageStarted, Stage: StageHypothesize, Time: clock})
	first, err := ReadStatus(path)
	if err != nil {
		t.Fatalf("read after first: %v", err)
	}

	// A spend tick 100ms later (< 1s interval) must NOT change the on-disk doc:
	// rate-limited away.
	clock = clock.Add(100 * time.Millisecond)
	s.Handle(Event{Kind: KindSpendTick, InputTokens: 999, OutputTokens: 999, Time: clock})
	second, err := ReadStatus(path)
	if err != nil {
		t.Fatalf("read after tick: %v", err)
	}
	if second.SpendInput == 999 {
		t.Errorf("rate-limited spend tick was written: %+v", second)
	}
	if !second.LastUpdated.Equal(first.LastUpdated) {
		t.Errorf("rate-limited event changed last_updated: %v vs %v", first.LastUpdated, second.LastUpdated)
	}

	// After the interval elapses, the next event writes and reflects accumulated
	// in-memory state (the earlier tick's spend).
	clock = clock.Add(2 * time.Second)
	s.Handle(Event{Kind: KindStageStarted, Stage: StageVerify, Time: clock})
	third, err := ReadStatus(path)
	if err != nil {
		t.Fatalf("read after interval: %v", err)
	}
	if third.SpendInput != 999 {
		t.Errorf("post-interval write lost accumulated spend: %+v", third)
	}
	if third.Stage != StageVerify {
		t.Errorf("stage = %q, want verify", third.Stage)
	}
}

func TestSnapshot_TerminalEventAlwaysWrites(t *testing.T) {
	clock := time.Unix(9000, 0)
	s, path := newTestSnapshot(t, func() time.Time { return clock })

	s.Handle(Event{Kind: KindStageStarted, Stage: StageHypothesize, Time: clock})
	// Immediately (within the rate-limit window) a terminal event must still write.
	s.Handle(Event{Kind: KindScanFinished, ScanKind: "sweep", InputTokens: 42, Time: clock})

	st, err := ReadStatus(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if st.SpendInput != 42 {
		t.Errorf("terminal write not applied: %+v", st)
	}
	if st.Stage != "" {
		t.Errorf("finished status should clear stage, got %q", st.Stage)
	}
}

func TestSnapshot_DaySpendGetter(t *testing.T) {
	clock := time.Unix(9000, 0)
	s, path := newTestSnapshot(t, func() time.Time { return clock })
	s.WithDaySpend(func() (int64, int64) { return 500, 250 })

	s.Handle(Event{Kind: KindScanFinished, ScanKind: "sweep", Time: clock})

	st, err := ReadStatus(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if st.SpendTodayInput != 500 || st.SpendTodayOutput != 250 {
		t.Errorf("day spend = in:%d out:%d, want 500/250", st.SpendTodayInput, st.SpendTodayOutput)
	}
}

func TestReadStatus_MissingFile(t *testing.T) {
	_, err := ReadStatus(filepath.Join(t.TempDir(), "nope.json"))
	if err == nil {
		t.Fatal("expected error reading missing status file")
	}
}
