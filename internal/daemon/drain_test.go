package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/report"
	"github.com/dpoage/bugbot/internal/store"
)

// ---------------------------------------------------------------------------
// Scheduler composition tests for the verify-drain and impact-sweep maintenance
// drains (bugbot-vpx.5): they fire on their own cadence, are day-budget gated
// exactly like the repro backlog, and their cycles are logged.
// ---------------------------------------------------------------------------

// recordingProgSink records every progress event. Safe for concurrent use: the
// daemon emits from its loop goroutine while the test reads from another.
type recordingProgSink struct {
	mu     sync.Mutex
	events []progress.Event
}

func (s *recordingProgSink) Handle(ev progress.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
}

// finishedCount returns how many KindCycleFinished events carried scan kind.
func (s *recordingProgSink) finishedCount(kind store.ScanKind) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.events {
		if e.Kind == progress.KindCycleFinished && e.ScanKind == string(kind) {
			n++
		}
	}
	return n
}

// scheduledCount returns how many KindCycleScheduled events were emitted; the
// Run loop emits exactly one per iteration, so a rise past the initial 1 proves
// the loop processed a cycle and came back around.
func (s *recordingProgSink) scheduledCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.events {
		if e.Kind == progress.KindCycleScheduled {
			n++
		}
	}
	return n
}

// buildDrainDaemon builds a daemon wired with a recording progress sink and the
// caller's drain intervals. clk drives the loop.
func buildDrainDaemon(t *testing.T, fr *fixtureRepo, st *store.Store, llmc *fakeLLM, cfg DaemonConfig, clk *fakeClock, prog progress.EventSink) *Daemon {
	t.Helper()
	d, err := New(Deps{
		Repo:       fr.open(),
		Store:      st,
		Clients:    funnel.RoleClients{Finder: llmc, Verifier: llmc},
		FunnelOpts: funnel.Options{Limits: funnel.StageLimits{Refuters: 1, MaxParallel: 2}},
		Sinks:      []report.Sink{&captureSink{}},
		Logger:     discardLogger(),
		Progress:   prog,
	}, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.clock = clk
	return d
}

// markPriorSweep records a prior sweep scan-run and the last-seen sentinel so
// the startup sweep is skipped (poll/sweep parked at 1h), leaving only the drain
// timer under test eligible to fire.
func markPriorSweep(t *testing.T, st *store.Store, base string) {
	t.Helper()
	if _, err := st.BeginScanRun(context.Background(), store.ScanSweep, base); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertFileStates(context.Background(), []store.FileState{{
		Path: lastSeenSentinel, ContentHash: base, LastScannedCommit: base,
	}}); err != nil {
		t.Fatal(err)
	}
}

// seedPendingCandidate writes one pending_candidates row (an interrupted run's
// leftover) anchored at fixtureFile:fixtureLine so the verify drain has work.
func seedPendingCandidate(t *testing.T, st *store.Store, base string) {
	t.Helper()
	if err := st.AddPendingCandidates(context.Background(), []store.PendingCandidate{{
		ScanRunID:   "prior-interrupted",
		CommitSHA:   base,
		Lens:        "nil-deref",
		File:        fixtureFile,
		Line:        fixtureLine,
		Title:       "possible nil dereference",
		Description: "x may be nil here",
		Severity:    "high",
		Evidence:    "no guard before use",
		Confidence:  "high",
	}}); err != nil {
		t.Fatal(err)
	}
}

// TestDaemonVerifyDrainFiresOnCadence: with a short verify-drain interval and
// poll/sweep parked at 1h, the verify-drain timer fires, drains the seeded
// pending candidate (opening a verify-drain scan run), and logs the cycle.
func TestDaemonVerifyDrainFiresOnCadence(t *testing.T) {
	fr := newFixtureRepo(t)
	fr.write(fixtureFile, "package p\n\nfunc f(x *int) int { return *x }\n")
	base := fr.commit("init")

	st := openStore(t)
	markPriorSweep(t, st, base)
	seedPendingCandidate(t, st, base)

	llmc := newFakeLLM(emptyJSON, notRefutedJSON)
	clk := newFakeClock(mustTime(t, testStart))
	prog := &recordingProgSink{}
	cfg := DaemonConfig{
		PollInterval:        time.Hour,
		SweepInterval:       time.Hour,
		PerCycleTokens:      1_000_000,
		PerDayTokens:        10_000_000,
		VerifyDrainInterval: 10 * time.Millisecond, // nearest deadline -> fires first
		ImpactSweepInterval: time.Hour,
	}
	d := buildDrainDaemon(t, fr, st, llmc, cfg, clk, prog)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runInBackground(ctx, d)

	if !clk.fire(ctx, t) {
		t.Fatal("clock fire failed")
	}
	// A verify-drain cycle-finished event is the last thing runVerifyDrain emits,
	// so it signals the cycle fully completed (scan run opened + logged).
	waitFor(t, func() bool { return prog.finishedCount(store.ScanVerifyDrain) >= 1 })

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if got := countScanRuns(t, st, store.ScanVerifyDrain); got < 1 {
		t.Fatalf("expected >=1 verify-drain scan run, got %d", got)
	}
	if got := countScanRuns(t, st, store.ScanImpactSweep); got != 0 {
		t.Fatalf("verify-drain firing must not open an impact-sweep run, got %d", got)
	}
}

// TestDaemonImpactSweepFiresOnCadence: with a short impact-sweep interval and
// poll/sweep parked at 1h, the impact-sweep timer fires, re-ranks the seeded
// unswept finding (opening an impact-sweep scan run, setting swept_at), and logs.
func TestDaemonImpactSweepFiresOnCadence(t *testing.T) {
	fr := newFixtureRepo(t)
	fr.write(fixtureFile, "package p\n\nfunc f(x *int) int { return *x }\n")
	base := fr.commit("init")

	st := openStore(t)
	markPriorSweep(t, st, base)
	seeded := seedFinding(t, st, "unswept finding", 2, "", false)

	// Sanity: unswept before the drain fires.
	before, err := st.UnsweptOpenFindings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 {
		t.Fatalf("expected 1 unswept finding before drain, got %d", len(before))
	}

	llmc := newFakeLLM(emptyJSON, notRefutedJSON)
	clk := newFakeClock(mustTime(t, testStart))
	prog := &recordingProgSink{}
	cfg := DaemonConfig{
		PollInterval:        time.Hour,
		SweepInterval:       time.Hour,
		PerCycleTokens:      1_000_000,
		PerDayTokens:        10_000_000,
		VerifyDrainInterval: time.Hour,
		ImpactSweepInterval: 10 * time.Millisecond, // nearest deadline -> fires first
	}
	d := buildDrainDaemon(t, fr, st, llmc, cfg, clk, prog)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runInBackground(ctx, d)

	if !clk.fire(ctx, t) {
		t.Fatal("clock fire failed")
	}
	waitFor(t, func() bool { return prog.finishedCount(store.ScanImpactSweep) >= 1 })

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if got := countScanRuns(t, st, store.ScanImpactSweep); got < 1 {
		t.Fatalf("expected >=1 impact-sweep scan run, got %d", got)
	}
	// The drain actually did work: the seeded finding now carries a swept marker.
	got, err := st.GetFinding(context.Background(), seeded.ID)
	if err != nil {
		t.Fatalf("GetFinding: %v", err)
	}
	if got.SweptAt.IsZero() {
		t.Fatal("expected swept_at to be set after the impact-sweep drain")
	}
}

// TestDaemonVerifyDrainBudgetGated: when the day budget is exhausted, the
// verify-drain timer fires but the cycle is skipped — no verify-drain scan run.
func TestDaemonVerifyDrainBudgetGated(t *testing.T) {
	fr := newFixtureRepo(t)
	fr.write(fixtureFile, "package p\n\nfunc f(x *int) int { return *x }\n")
	base := fr.commit("init")

	st := openStore(t)
	markPriorSweep(t, st, base)
	seedPendingCandidate(t, st, base)
	// Pre-record spend over the per-day budget.
	if _, err := st.RecordSpend(context.Background(), store.Spend{
		Role: "finder", InputTokens: 600_000,
	}); err != nil {
		t.Fatal(err)
	}

	llmc := newFakeLLM(emptyJSON, notRefutedJSON)
	clk := newFakeClock(mustTime(t, testStart))
	prog := &recordingProgSink{}
	cfg := DaemonConfig{
		PollInterval:        time.Hour,
		SweepInterval:       time.Hour,
		PerCycleTokens:      100_000,
		PerDayTokens:        500_000, // already exceeded by the pre-recorded 600k
		VerifyDrainInterval: 10 * time.Millisecond,
		ImpactSweepInterval: time.Hour,
	}
	d := buildDrainDaemon(t, fr, st, llmc, cfg, clk, prog)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runInBackground(ctx, d)

	if !clk.fire(ctx, t) {
		t.Fatal("clock fire failed")
	}
	// The gated cycle emits no cycle-finished, but the loop comes back around and
	// emits a second KindCycleScheduled — a deterministic "skip processed" signal.
	waitFor(t, func() bool { return prog.scheduledCount() >= 2 })

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if got := countScanRuns(t, st, store.ScanVerifyDrain); got != 0 {
		t.Fatalf("budget-exhausted verify drain must not open a scan run, got %d", got)
	}
	if llmc.callCount() != 0 {
		t.Fatalf("budget-exhausted verify drain must make zero LLM calls, got %d", llmc.callCount())
	}
	if got := prog.finishedCount(store.ScanVerifyDrain); got != 0 {
		t.Fatalf("budget-exhausted verify drain must not log a finished cycle, got %d", got)
	}
}

// TestDaemonImpactSweepBudgetGated: when the day budget is exhausted, the
// impact-sweep timer fires but the cycle is skipped — no impact-sweep scan run,
// and the seeded finding stays unswept.
func TestDaemonImpactSweepBudgetGated(t *testing.T) {
	fr := newFixtureRepo(t)
	fr.write(fixtureFile, "package p\n\nfunc f(x *int) int { return *x }\n")
	base := fr.commit("init")

	st := openStore(t)
	markPriorSweep(t, st, base)
	seeded := seedFinding(t, st, "unswept finding", 2, "", false)
	if _, err := st.RecordSpend(context.Background(), store.Spend{
		Role: "finder", InputTokens: 600_000,
	}); err != nil {
		t.Fatal(err)
	}

	llmc := newFakeLLM(emptyJSON, notRefutedJSON)
	clk := newFakeClock(mustTime(t, testStart))
	prog := &recordingProgSink{}
	cfg := DaemonConfig{
		PollInterval:        time.Hour,
		SweepInterval:       time.Hour,
		PerCycleTokens:      100_000,
		PerDayTokens:        500_000,
		VerifyDrainInterval: time.Hour,
		ImpactSweepInterval: 10 * time.Millisecond,
	}
	d := buildDrainDaemon(t, fr, st, llmc, cfg, clk, prog)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runInBackground(ctx, d)

	if !clk.fire(ctx, t) {
		t.Fatal("clock fire failed")
	}
	waitFor(t, func() bool { return prog.scheduledCount() >= 2 })

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if got := countScanRuns(t, st, store.ScanImpactSweep); got != 0 {
		t.Fatalf("budget-exhausted impact sweep must not open a scan run, got %d", got)
	}
	got, err := st.GetFinding(context.Background(), seeded.ID)
	if err != nil {
		t.Fatalf("GetFinding: %v", err)
	}
	if !got.SweptAt.IsZero() {
		t.Fatal("budget-exhausted impact sweep must not set swept_at")
	}
}

// TestSchedule_EarliestPriority pins the priority-ordered deadline race: the
// nearest deadline always wins, and on an exact tie the higher-priority timer
// wins in the order sweep > backlog > verify-drain > impact-sweep > poll.
// Disabled (nil) pointer timers are excluded entirely.
func TestSchedule_EarliestPriority(t *testing.T) {
	base := mustTime(t, testStart)
	at := func(d time.Duration) *time.Time { tm := base.Add(d); return &tm }

	tests := []struct {
		name string
		s    schedule
		want timerKind
	}{
		{
			name: "all equal -> sweep wins",
			s:    schedule{nextPoll: base, nextSweep: base, nextBacklog: at(0), nextVerifyDrain: at(0), nextImpactSweep: at(0)},
			want: timerSweep,
		},
		{
			name: "tie below sweep -> backlog wins",
			s:    schedule{nextPoll: base, nextSweep: base.Add(time.Hour), nextBacklog: at(0), nextVerifyDrain: at(0), nextImpactSweep: at(0)},
			want: timerBacklog,
		},
		{
			name: "tie, no backlog -> verify-drain wins over impact/poll",
			s:    schedule{nextPoll: base, nextSweep: base.Add(time.Hour), nextVerifyDrain: at(0), nextImpactSweep: at(0)},
			want: timerVerifyDrain,
		},
		{
			name: "tie, only impact-sweep -> beats poll",
			s:    schedule{nextPoll: base, nextSweep: base.Add(time.Hour), nextImpactSweep: at(0)},
			want: timerImpactSweep,
		},
		{
			name: "drains nil -> poll when sweep far",
			s:    schedule{nextPoll: base, nextSweep: base.Add(time.Hour)},
			want: timerPoll,
		},
		{
			name: "strictly-earlier poll beats higher-priority drains",
			s:    schedule{nextPoll: base, nextSweep: base.Add(time.Hour), nextVerifyDrain: at(time.Minute), nextImpactSweep: at(time.Minute)},
			want: timerPoll,
		},
		{
			name: "verify-drain genuinely earliest",
			s:    schedule{nextPoll: base.Add(time.Hour), nextSweep: base.Add(time.Hour), nextVerifyDrain: at(time.Minute), nextImpactSweep: at(2 * time.Minute)},
			want: timerVerifyDrain,
		},
		{
			name: "impact-sweep genuinely earliest",
			s:    schedule{nextPoll: base.Add(time.Hour), nextSweep: base.Add(time.Hour), nextVerifyDrain: at(2 * time.Minute), nextImpactSweep: at(time.Minute)},
			want: timerImpactSweep,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, got := tt.s.earliest()
			if got != tt.want {
				t.Errorf("earliest() = %v, want %v", got, tt.want)
			}
		})
	}
}
