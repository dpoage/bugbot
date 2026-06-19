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
	s.Handle(Event{Kind: KindScanFinished, ScanKind: "sweep", Counts: &Counts{Verified: 2}, InputTokens: 11, OutputTokens: 7, CacheReadTokens: 4, Time: time.Unix(2000, 0)})

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
	if st.SpendCacheRead != 4 {
		t.Errorf("spend cache read = %d, want 4", st.SpendCacheRead)
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

// TestSnapshot_LiveCandidateCounter feeds a synthetic finder-AgentFinished
// sequence and asserts the live candidate counter accumulates correctly.
//
// Sequence: 3 finder KindAgentFinished with Candidates 2/0/1 → LiveCandidates=3.
// Then KindStageFinished for hypothesize → LiveCandidates resets to 0.
//
// VACUITY: if the accumulation in apply() is disabled, LiveCandidates stays 0
// and the mid-sequence assertion fails.
func TestSnapshot_LiveCandidateCounter(t *testing.T) {
	clock := time.Unix(10000, 0)
	advance := func(d time.Duration) { clock = clock.Add(d) }
	now := func() time.Time { return clock }
	s, path := newTestSnapshot(t, now)

	// First event also writes (lastWrite zero).
	s.Handle(Event{Kind: KindAgentFinished, Role: RoleFinder, Label: "lens-a", Candidates: 2, Time: clock})
	advance(2 * time.Second) // past rate limit
	s.Handle(Event{Kind: KindAgentFinished, Role: RoleFinder, Label: "lens-b", Candidates: 0, Time: clock})
	advance(2 * time.Second)
	s.Handle(Event{Kind: KindAgentFinished, Role: RoleFinder, Label: "lens-c", Candidates: 1, Time: clock})

	// Force a write by advancing past the rate limit and sending a non-terminal event.
	advance(2 * time.Second)
	s.Handle(Event{Kind: KindStageStarted, Stage: StageHypothesize, Time: clock})

	st, err := ReadStatus(path)
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if st.LiveCandidates != 3 {
		t.Errorf("LiveCandidates = %d after 3 finder finishes (2+0+1), want 3", st.LiveCandidates)
	}

	// After KindStageFinished for hypothesize, LiveCandidates resets.
	advance(2 * time.Second)
	s.Handle(Event{
		Kind: KindStageFinished, Stage: StageHypothesize,
		Counts: &Counts{Hypothesized: 3},
		Time:   clock,
	})
	st2, err := ReadStatus(path)
	if err != nil {
		t.Fatalf("read status after stage-finish: %v", err)
	}
	if st2.LiveCandidates != 0 {
		t.Errorf("LiveCandidates = %d after hypothesize stage-finish, want 0 (final count in Counts)", st2.LiveCandidates)
	}
	if st2.Counts.Hypothesized != 3 {
		t.Errorf("Counts.Hypothesized = %d, want 3", st2.Counts.Hypothesized)
	}
}

// TestSnapshot_LiveVerifyKillCounters feeds a synthetic verify sequence and
// asserts the live verified/killed counters accumulate and reset correctly.
//
// Sequence: 2 KindFindingVerified + 1 KindFindingKilled → LiveVerified=2 LiveKilled=1.
// Then KindStageFinished for verify → both reset to 0.
func TestSnapshot_LiveVerifyKillCounters(t *testing.T) {
	clock := time.Unix(20000, 0)
	advance := func(d time.Duration) { clock = clock.Add(d) }
	now := func() time.Time { return clock }
	s, path := newTestSnapshot(t, now)

	// Force an initial write so lastWrite is set.
	s.Handle(Event{Kind: KindStageStarted, Stage: StageVerify, Time: clock})

	advance(2 * time.Second)
	s.Handle(Event{Kind: KindFindingVerified, Title: "bug-a", File: "a.go", Line: 1, Time: clock})
	advance(2 * time.Second)
	s.Handle(Event{Kind: KindFindingKilled, Title: "bug-b", File: "b.go", Line: 2, Time: clock})
	advance(2 * time.Second)
	s.Handle(Event{Kind: KindFindingVerified, Title: "bug-c", File: "c.go", Line: 3, Time: clock})

	// Force write.
	advance(2 * time.Second)
	s.Handle(Event{Kind: KindStageStarted, Stage: StagePersist, Time: clock})

	st, err := ReadStatus(path)
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if st.LiveVerified != 2 {
		t.Errorf("LiveVerified = %d, want 2", st.LiveVerified)
	}
	if st.LiveKilled != 1 {
		t.Errorf("LiveKilled = %d, want 1", st.LiveKilled)
	}
	// Counts.Verified should also have accumulated (existing behavior).
	if st.Counts.Verified != 2 {
		t.Errorf("Counts.Verified = %d, want 2", st.Counts.Verified)
	}

	// After KindStageFinished for verify, live fields reset.
	advance(2 * time.Second)
	s.Handle(Event{
		Kind: KindStageFinished, Stage: StageVerify,
		Counts: &Counts{Hypothesized: 3, Triaged: 3, Verified: 2, Killed: 1},
		Time:   clock,
	})
	st2, err := ReadStatus(path)
	if err != nil {
		t.Fatalf("read after verify stage-finish: %v", err)
	}
	if st2.LiveVerified != 0 || st2.LiveKilled != 0 {
		t.Errorf("live counters = verified:%d killed:%d after stage-finish, want both 0",
			st2.LiveVerified, st2.LiveKilled)
	}
	if st2.Counts.Verified != 2 || st2.Counts.Killed != 1 {
		t.Errorf("Counts = verified:%d killed:%d, want verified:2 killed:1",
			st2.Counts.Verified, st2.Counts.Killed)
	}
}

// TestSnapshot_FullLiveSequence feeds the three-finder + verify sequence from
// the spec and asserts the combined live counter state.
//
// Input: 3 finder KindAgentFinished with Candidates 2/0/1,
//
//	then 2 KindFindingVerified, 1 KindFindingKilled.
//
// Mid-sequence: LiveCandidates=3, LiveVerified=2, LiveKilled=1.
func TestSnapshot_FullLiveSequence(t *testing.T) {
	clock := time.Unix(30000, 0)
	advance := func(d time.Duration) { clock = clock.Add(d) }
	now := func() time.Time { return clock }
	s, path := newTestSnapshot(t, now)

	// Finder phase.
	s.Handle(Event{Kind: KindAgentFinished, Role: RoleFinder, Label: "lens-1", Candidates: 2, Time: clock})
	advance(2 * time.Second)
	s.Handle(Event{Kind: KindAgentFinished, Role: RoleFinder, Label: "lens-2", Candidates: 0, Time: clock})
	advance(2 * time.Second)
	s.Handle(Event{Kind: KindAgentFinished, Role: RoleFinder, Label: "lens-3", Candidates: 1, Time: clock})

	// Verify phase.
	advance(2 * time.Second)
	s.Handle(Event{Kind: KindFindingVerified, Title: "t1", File: "f.go", Line: 1, Time: clock})
	advance(2 * time.Second)
	s.Handle(Event{Kind: KindFindingVerified, Title: "t2", File: "f.go", Line: 2, Time: clock})
	advance(2 * time.Second)
	s.Handle(Event{Kind: KindFindingKilled, Title: "t3", File: "f.go", Line: 3, Time: clock})

	// Force a write.
	advance(2 * time.Second)
	s.Handle(Event{Kind: KindStageStarted, Stage: StagePersist, Time: clock})

	st, err := ReadStatus(path)
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if st.LiveCandidates != 3 {
		t.Errorf("LiveCandidates = %d, want 3 (2+0+1)", st.LiveCandidates)
	}
	if st.LiveVerified != 2 {
		t.Errorf("LiveVerified = %d, want 2", st.LiveVerified)
	}
	if st.LiveKilled != 1 {
		t.Errorf("LiveKilled = %d, want 1", st.LiveKilled)
	}
}

// TestSnapshot_LiveCountersResetOnNewScan covers the aborted-run lifecycle
// edge: an aborted scan never emits StageFinished, and the daemon reuses one
// SnapshotSink across cycles — so live counters must reset on the NEXT scan's
// start event, or status shows the dead run's "candidates so far" until the
// new run's own stage completes.
func TestSnapshot_LiveCountersResetOnNewScan(t *testing.T) {
	clock := time.Unix(20000, 0)
	advance := func(d time.Duration) { clock = clock.Add(d) }
	now := func() time.Time { return clock }
	s, path := newTestSnapshot(t, now)

	// Run 1 accumulates live counters, then aborts: no StageFinished, no
	// ScanFinished.
	s.Handle(Event{Kind: KindScanStarted, ScanKind: "oneshot", Time: clock})
	advance(2 * time.Second)
	s.Handle(Event{Kind: KindAgentFinished, Role: RoleFinder, Label: "lens-a", Candidates: 4, Time: clock})
	advance(2 * time.Second)
	s.Handle(Event{Kind: KindFindingVerified, Title: "t", Time: clock})
	advance(2 * time.Second)
	s.Handle(Event{Kind: KindFindingKilled, Title: "k", Time: clock})

	// Run 2 starts (daemon's next cycle through the same sink).
	advance(2 * time.Second)
	s.Handle(Event{Kind: KindCycleStarted, ScanKind: "targeted", Time: clock})
	advance(2 * time.Second)
	s.Handle(Event{Kind: KindStageStarted, Stage: StageHypothesize, Time: clock})

	st, err := ReadStatus(path)
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if st.LiveCandidates != 0 || st.LiveVerified != 0 || st.LiveKilled != 0 {
		t.Errorf("live counters not reset on new scan: candidates=%d verified=%d killed=%d, want all 0 (stale values from the aborted run)",
			st.LiveCandidates, st.LiveVerified, st.LiveKilled)
	}
}

// TestSnapshot_LiveTriagedCounter verifies the per-candidate triage tick and
// its reset on StageFinished(triage) — and that an aborted run's value resets
// on the next scan start (same lifecycle rule as the other live counters).
func TestSnapshot_LiveTriagedCounter(t *testing.T) {
	clock := time.Unix(30000, 0)
	advance := func(d time.Duration) { clock = clock.Add(d) }
	now := func() time.Time { return clock }
	s, path := newTestSnapshot(t, now)

	s.Handle(Event{Kind: KindCandidateTriaged, Title: "a", Time: clock})
	advance(2 * time.Second)
	s.Handle(Event{Kind: KindCandidateTriaged, Title: "b", Time: clock})
	advance(2 * time.Second)
	s.Handle(Event{Kind: KindStageStarted, Stage: StageVerify, Time: clock})

	st, err := ReadStatus(path)
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if st.LiveTriaged != 2 {
		t.Errorf("LiveTriaged = %d after 2 triage ticks, want 2", st.LiveTriaged)
	}

	advance(2 * time.Second)
	s.Handle(Event{
		Kind: KindStageFinished, Stage: StageTriage,
		Counts: &Counts{Triaged: 2}, Time: clock,
	})
	advance(2 * time.Second)
	s.Handle(Event{Kind: KindStageStarted, Stage: StageVerify, Time: clock})
	st, err = ReadStatus(path)
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if st.LiveTriaged != 0 {
		t.Errorf("LiveTriaged = %d after StageFinished(triage), want 0", st.LiveTriaged)
	}
	if st.Counts.Triaged != 2 {
		t.Errorf("Counts.Triaged = %d, want 2 (authoritative stage-finish value)", st.Counts.Triaged)
	}

	// Aborted-run lifecycle: tick without a stage finish, then a new scan.
	advance(2 * time.Second)
	s.Handle(Event{Kind: KindCandidateTriaged, Title: "stale", Time: clock})
	advance(2 * time.Second)
	s.Handle(Event{Kind: KindScanStarted, ScanKind: "oneshot", Time: clock})
	advance(2 * time.Second)
	s.Handle(Event{Kind: KindStageStarted, Stage: StageHypothesize, Time: clock})
	st, err = ReadStatus(path)
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if st.LiveTriaged != 0 {
		t.Errorf("LiveTriaged = %d after new scan start, want 0 (stale value from aborted run)", st.LiveTriaged)
	}
}

// TestSnapshot_AgentActivity_UpdatesTrackedAgent verifies that a KindAgentActivity
// event sets Activity and ActivityAt on the matching in-flight agent and that
// the fields survive refreshAgents into the persisted snapshot.
func TestSnapshot_AgentActivity_UpdatesTrackedAgent(t *testing.T) {
	clock := time.Unix(50000, 0)
	now := func() time.Time { return clock }
	s, path := newTestSnapshot(t, now)

	s.Handle(Event{Kind: KindScanStarted, ScanKind: "sweep", Time: clock})
	s.Handle(Event{Kind: KindAgentStarted, Role: RoleFinder, Label: "lensA", Time: clock})

	clock = clock.Add(2 * time.Second) // past rate-limit window
	actAt := clock
	s.Handle(Event{Kind: KindAgentActivity, Role: RoleFinder, Label: "lensA", Activity: "reading main.go", Time: actAt})

	// Force a write with a non-terminal event after the rate-limit window.
	clock = clock.Add(2 * time.Second)
	s.Handle(Event{Kind: KindStageStarted, Stage: StageVerify, Time: clock})

	st, err := ReadStatus(path)
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if len(st.ActiveAgents) != 1 {
		t.Fatalf("ActiveAgents = %d, want 1", len(st.ActiveAgents))
	}
	a := st.ActiveAgents[0]
	if a.Activity != "reading main.go" {
		t.Errorf("Activity = %q, want %q", a.Activity, "reading main.go")
	}
	if !a.ActivityAt.Equal(actAt) {
		t.Errorf("ActivityAt = %v, want %v", a.ActivityAt, actAt)
	}
}

// TestSnapshot_AgentActivity_IgnoresUntracked verifies that a KindAgentActivity
// event for an agent that is not in the tracked-agents map (e.g. a stray or
// post-finish event) does not resurrect a finished agent or create a new one.
func TestSnapshot_AgentActivity_IgnoresUntracked(t *testing.T) {
	clock := time.Unix(51000, 0)
	now := func() time.Time { return clock }
	s, path := newTestSnapshot(t, now)

	// Start and immediately finish an agent.
	s.Handle(Event{Kind: KindAgentStarted, Role: RoleFinder, Label: "lensB", Time: clock})
	s.Handle(Event{Kind: KindAgentFinished, Role: RoleFinder, Label: "lensB", Time: clock})

	// A stray activity event for the now-finished agent.
	clock = clock.Add(2 * time.Second)
	s.Handle(Event{Kind: KindAgentActivity, Role: RoleFinder, Label: "lensB", Activity: "stray", Time: clock})

	// Activity for a never-started agent.
	clock = clock.Add(2 * time.Second)
	s.Handle(Event{Kind: KindAgentActivity, Role: RoleFinder, Label: "ghost", Activity: "ghost activity", Time: clock})

	// Force a write.
	clock = clock.Add(2 * time.Second)
	s.Handle(Event{Kind: KindScanFinished, ScanKind: "sweep", Time: clock})

	st, err := ReadStatus(path)
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if len(st.ActiveAgents) != 0 {
		t.Errorf("stray activity must not resurrect finished agent; got %+v", st.ActiveAgents)
	}
}

// TestSnapshot_AgentActivity_SurvivesRefreshAgents verifies that the Activity
// and ActivityAt fields survive the refreshAgents pass (i.e. they are not
// lost when the sorted slice is rebuilt from the map).
func TestSnapshot_AgentActivity_SurvivesRefreshAgents(t *testing.T) {
	clock := time.Unix(52000, 0)
	now := func() time.Time { return clock }
	s, path := newTestSnapshot(t, now)

	s.Handle(Event{Kind: KindAgentStarted, Role: RoleFinder, Label: "lensC", Time: clock})
	s.Handle(Event{Kind: KindAgentStarted, Role: RoleVerifier, Label: "candX", Time: clock})

	actAt := clock.Add(time.Second)
	clock = actAt
	s.Handle(Event{Kind: KindAgentActivity, Role: RoleFinder, Label: "lensC", Activity: "grepping \"foo\"", Time: actAt})

	clock = clock.Add(2 * time.Second)
	s.Handle(Event{Kind: KindScanFinished, ScanKind: "sweep", Time: clock}) // terminal => always writes

	st, err := ReadStatus(path)
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	// scan_finished does not remove agents from the map (only KindAgentFinished does).
	// The snapshot will still reflect the tracked agents. What matters is the Activity
	// field survived refreshAgents on the terminal write.
	found := false
	for _, a := range st.ActiveAgents {
		if a.Role == RoleFinder && a.Label == "lensC" {
			found = true
			if a.Activity != `grepping "foo"` {
				t.Errorf("Activity not preserved through refreshAgents: got %q", a.Activity)
			}
			if !a.ActivityAt.Equal(actAt) {
				t.Errorf("ActivityAt not preserved through refreshAgents: got %v, want %v", a.ActivityAt, actAt)
			}
		}
	}
	if !found {
		t.Error("lensC not found in ActiveAgents after terminal write")
	}

	// Re-run: start agents again and verify the fields survive a real refresh.
	s2, path2 := newTestSnapshot(t, now)
	s2.Handle(Event{Kind: KindAgentStarted, Role: RoleFinder, Label: "lensD", Time: clock})
	clock = clock.Add(2 * time.Second)
	actAt2 := clock
	s2.Handle(Event{Kind: KindAgentActivity, Role: RoleFinder, Label: "lensD", Activity: "navigating Foo", Time: actAt2})
	clock = clock.Add(2 * time.Second)
	s2.Handle(Event{Kind: KindStageStarted, Stage: StagePersist, Time: clock}) // non-terminal write after interval

	st2, err := ReadStatus(path2)
	if err != nil {
		t.Fatalf("read status2: %v", err)
	}
	if len(st2.ActiveAgents) != 1 {
		t.Fatalf("expected 1 active agent, got %d", len(st2.ActiveAgents))
	}
	if st2.ActiveAgents[0].Activity != "navigating Foo" {
		t.Errorf("Activity = %q, want %q", st2.ActiveAgents[0].Activity, "navigating Foo")
	}
	if !st2.ActiveAgents[0].ActivityAt.Equal(actAt2) {
		t.Errorf("ActivityAt = %v, want %v", st2.ActiveAgents[0].ActivityAt, actAt2)
	}
}
