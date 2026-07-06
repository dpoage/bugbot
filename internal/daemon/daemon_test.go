package daemon

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/report"
	"github.com/dpoage/bugbot/internal/repro"
	"github.com/dpoage/bugbot/internal/store"
)

// ---------------------------------------------------------------------------
// Test doubles: capturing sink and fake promoter.
// ---------------------------------------------------------------------------

// captureSink records every Report it receives, for assertions on emitted
// findings. Safe for concurrent use (the daemon emits from one goroutine, but
// tests read from another).
type captureSink struct {
	mu      sync.Mutex
	reports []report.Report
}

func (s *captureSink) Name() string { return "capture" }

func (s *captureSink) Write(_ context.Context, r report.Report) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reports = append(s.reports, r)
	return nil
}

func (s *captureSink) last() (report.Report, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.reports) == 0 {
		return report.Report{}, false
	}
	return s.reports[len(s.reports)-1], true
}

// fakePromoter records the findings handed to it and promotes none, so tests can
// assert the daemon offered the right inputs without a sandbox.
type fakePromoter struct {
	mu       sync.Mutex
	attempts [][]domain.Finding
}

func (p *fakePromoter) PromoteAll(_ context.Context, _ *store.Store, findings []domain.Finding) (*repro.Summary, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := make([]domain.Finding, len(findings))
	copy(cp, findings)
	p.attempts = append(p.attempts, cp)
	return &repro.Summary{Attempted: len(findings)}, nil
}

func (p *fakePromoter) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.attempts)
}

// discardLogger returns a logger that drops output (tests assert on state, not
// log text).
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// buildDaemon assembles a Daemon over the given fixture/store with the supplied
// fake LLM, a fake clock, and a capture sink. It returns the daemon and the
// pieces the test asserts against.
func buildDaemon(t *testing.T, fr *fixtureRepo, st *store.Store, llmc *fakeLLM, cfg DaemonConfig, clk *fakeClock) (*Daemon, *captureSink) {
	t.Helper()
	sink := &captureSink{}
	d, err := New(Deps{
		Repo:    fr.open(),
		Store:   st,
		Clients: funnel.RoleClients{Finder: llmc, Verifier: llmc},
		// One refuter keeps the verify stage to a single, deterministic vote.
		FunnelOpts: funnel.Options{Limits: funnel.StageLimits{Refuters: 1, MaxParallel: 2}},
		Sinks:      []report.Sink{sink},
		Logger:     discardLogger(),
	}, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.clock = clk
	return d, sink
}

// runInBackground starts d.Run in a goroutine and returns a cancel func plus a
// done channel that closes when Run returns. The caller drives the loop with the
// fake clock and then cancels to stop.
func runInBackground(ctx context.Context, d *Daemon) <-chan error {
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	return done
}

const testStart = "2026-06-09T12:00:00Z"

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

// ---------------------------------------------------------------------------
// 1. New-commit flow: a commit mid-run triggers a targeted scan that records a
//    targeted scan_run and emits findings to the sink.
// ---------------------------------------------------------------------------

func TestDaemonNewCommitTriggersTargetedScan(t *testing.T) {
	fr := newFixtureRepo(t)
	// Seed an initial commit so the poller has a baseline tip, and pre-seed the
	// sweep watermark so the startup sweep is skipped (we test poll in isolation).
	fr.write(fixtureFile, "package p\n\nfunc f(x *int) int { return *x }\n")
	fr.write("README.md", "seed\n")
	base := fr.commit("init")

	st := openStore(t)
	// Mark a prior sweep so startup does not sweep immediately.
	if _, err := st.BeginScanRun(context.Background(), store.ScanSweep, base); err != nil {
		t.Fatal(err)
	}
	// Establish the last-seen baseline at the initial commit so the next poll
	// detects only the NEW commit we add below.
	if err := st.UpsertFileStates(context.Background(), []store.FileState{{
		Path: lastSeenSentinel, ContentHash: base, LastScannedCommit: base,
	}}); err != nil {
		t.Fatal(err)
	}

	llmc := newFakeLLM(candidateJSON, notRefutedJSON)
	clk := newFakeClock(mustTime(t, testStart))
	cfg := DaemonConfig{
		PollInterval:   10 * time.Millisecond,
		SweepInterval:  time.Hour, // far in the future; we only exercise polls
		PerCycleTokens: 1_000_000,
		PerDayTokens:   10_000_000,
	}
	d, sink := buildDaemon(t, fr, st, llmc, cfg, clk)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runInBackground(ctx, d)

	// Add a new commit that modifies the in-scope file.
	fr.write(fixtureFile, "package p\n\nfunc f(x *int) int { y := *x; return y }\n")
	fr.commit("modify bug.go")

	// Fire one poll: detects the new commit, runs a targeted scan.
	if !clk.fire(ctx, t) {
		t.Fatal("clock fire failed")
	}

	// Wait for the cycle to complete through report emission (the last step), so
	// both the targeted scan_run and the emitted report are observable.
	waitFor(t, func() bool {
		_, ok := sink.last()
		return ok && countScanRuns(t, st, store.ScanTargeted) >= 1
	})

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if got := countScanRuns(t, st, store.ScanTargeted); got < 1 {
		t.Fatalf("expected at least one targeted scan_run, got %d", got)
	}
	rep, ok := sink.last()
	if !ok {
		t.Fatal("sink received no report")
	}
	if len(rep.Findings) == 0 {
		t.Fatal("expected the emitted report to contain the new finding")
	}
}

// ---------------------------------------------------------------------------
// 2. Idle backoff: with no new commits, the poll delay stretches each idle tick.
// ---------------------------------------------------------------------------

func TestDaemonIdleBackoffStretchesPollCadence(t *testing.T) {
	fr := newFixtureRepo(t)
	fr.write(fixtureFile, "package p\n\nfunc f() {}\n")
	base := fr.commit("init")

	st := openStore(t)
	if _, err := st.BeginScanRun(context.Background(), store.ScanSweep, base); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertFileStates(context.Background(), []store.FileState{{
		Path: lastSeenSentinel, ContentHash: base, LastScannedCommit: base,
	}}); err != nil {
		t.Fatal(err)
	}

	llmc := newFakeLLM(candidateJSON, notRefutedJSON)
	clk := newFakeClock(mustTime(t, testStart))
	cfg := DaemonConfig{
		PollInterval:   10 * time.Millisecond,
		IdleBackoff:    10 * time.Millisecond,
		SweepInterval:  time.Hour,
		PerCycleTokens: 1_000_000,
		PerDayTokens:   10_000_000,
	}
	d, _ := buildDaemon(t, fr, st, llmc, cfg, clk)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runInBackground(ctx, d)

	// Three idle polls: multiplier should climb 1, 2, 3, with no LLM calls.
	for want := 1; want <= 3; want++ {
		if !clk.fire(ctx, t) {
			t.Fatal("clock fire failed")
		}
		waitFor(t, func() bool { return readIdleMult(d) >= want })
	}

	if llmc.callCount() != 0 {
		t.Fatalf("idle repo must make zero LLM calls, got %d", llmc.callCount())
	}
	if got := readIdleMult(d); got < 3 {
		t.Fatalf("idle multiplier should have grown to >=3, got %d", got)
	}
	// nextPollDelay must exceed the base interval once backoff is engaged.
	if delay := readNextPollDelay(d); delay <= cfg.PollInterval {
		t.Fatalf("backoff should stretch poll delay beyond base %s, got %s", cfg.PollInterval, delay)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 3. Day-budget exhaustion: pre-recorded spend >= per-day budget skips the
//    cycle entirely — the funnel is never invoked (zero LLM calls).
// ---------------------------------------------------------------------------

func TestDaemonDayBudgetExhaustionSkipsCycle(t *testing.T) {
	fr := newFixtureRepo(t)
	fr.write(fixtureFile, "package p\n\nfunc f(x *int) int { return *x }\n")
	base := fr.commit("init")

	st := openStore(t)
	// Pre-record spend at/over the per-day budget within today's window.
	if _, err := st.RecordSpend(context.Background(), store.Spend{
		Role: "finder", InputTokens: 600_000, OutputTokens: 0,
	}); err != nil {
		t.Fatal(err)
	}

	llmc := newFakeLLM(candidateJSON, notRefutedJSON)
	clk := newFakeClock(mustTime(t, testStart))
	cfg := DaemonConfig{
		PollInterval:   10 * time.Millisecond,
		SweepInterval:  10 * time.Millisecond, // sweep due at startup (no prior sweep run)
		PerCycleTokens: 100_000,
		PerDayTokens:   500_000, // already exceeded by the pre-recorded 600k
	}
	d, _ := buildDaemon(t, fr, st, llmc, cfg, clk)
	_ = base

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runInBackground(ctx, d)

	// Fire the startup sweep: it must be skipped on budget, invoking no funnel.
	if !clk.fire(ctx, t) {
		t.Fatal("clock fire failed")
	}
	// Give the cycle a moment to (not) run, then assert no scan_run and no calls.
	waitFor(t, func() bool { return true }) // settle one scheduler iteration
	time.Sleep(20 * time.Millisecond)

	if llmc.callCount() != 0 {
		t.Fatalf("day-budget-exhausted cycle must make zero LLM calls, got %d", llmc.callCount())
	}
	if got := countScanRuns(t, st, store.ScanSweep); got != 0 {
		t.Fatalf("day-budget-exhausted cycle must not begin a scan run, got %d", got)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 4. Auto-close: an open finding whose file is removed is marked fixed after a
//    cycle's re-verification pass.
// ---------------------------------------------------------------------------

func TestDaemonAutoCloseOnFileRemoved(t *testing.T) {
	fr := newFixtureRepo(t)
	fr.write(fixtureFile, "package p\n\nfunc f(x *int) int { return *x }\n")
	fr.write("keep.go", "package p\n")
	base := fr.commit("init")

	st := openStore(t)
	if _, err := st.BeginScanRun(context.Background(), store.ScanSweep, base); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertFileStates(context.Background(), []store.FileState{{
		Path: lastSeenSentinel, ContentHash: base, LastScannedCommit: base,
	}}); err != nil {
		t.Fatal(err)
	}

	// Seed an open finding anchored to the fixture file.
	fp := domain.Fingerprint("nil-deref", fixtureFile, funnel.NewLocusResolver(fr.dir).Resolve(fixtureFile, fixtureLine))
	seeded, err := st.UpsertFinding(context.Background(), domain.Finding{
		Fingerprint: fp,
		Title:       "possible nil dereference",
		Severity:    "high",
		Tier:        2,
		Status:      domain.StatusOpen,
		Lens:        "nil-deref",
		File:        fixtureFile,
		Line:        fixtureLine,
		CommitSHA:   base,
		FileHash:    "deadbeef", // mismatched on purpose so re-verify engages
	})
	if err != nil {
		t.Fatal(err)
	}

	// Remove the file and commit, so polling detects a change and re-verification
	// finds the implicated file gone.
	fr.remove(fixtureFile)
	fr.commit("remove bug.go")

	llmc := newFakeLLM(emptyJSON, refutedJSON)
	clk := newFakeClock(mustTime(t, testStart))
	cfg := DaemonConfig{
		PollInterval:   10 * time.Millisecond,
		SweepInterval:  time.Hour,
		PerCycleTokens: 1_000_000,
		PerDayTokens:   10_000_000,
	}
	d, _ := buildDaemon(t, fr, st, llmc, cfg, clk)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runInBackground(ctx, d)

	if !clk.fire(ctx, t) {
		t.Fatal("clock fire failed")
	}

	waitFor(t, func() bool {
		f, gerr := st.GetFinding(context.Background(), seeded.ID)
		return gerr == nil && f.Status == domain.StatusFixed
	})

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	f, err := st.GetFinding(context.Background(), seeded.ID)
	if err != nil {
		t.Fatal(err)
	}
	if f.Status != domain.StatusFixed {
		t.Fatalf("expected finding auto-closed to %q, got %q", domain.StatusFixed, f.Status)
	}
}

// ---------------------------------------------------------------------------
// 4b. T3 promotion: a budget-orphaned tier-3 finding that survives a full
//     refuter vote on re-verification is promoted to tier 2.
// ---------------------------------------------------------------------------

func TestDaemonReverifyPromotesSurvivingT3(t *testing.T) {
	fr := newFixtureRepo(t)
	fr.write(fixtureFile, "package p\n\nfunc f(x *int) int { return *x }\n")
	base := fr.commit("init")

	st := openStore(t)
	if _, err := st.BeginScanRun(context.Background(), store.ScanSweep, base); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertFileStates(context.Background(), []store.FileState{{
		Path: lastSeenSentinel, ContentHash: base, LastScannedCommit: base,
	}}); err != nil {
		t.Fatal(err)
	}

	// Seed a tier-3 suspected finding (verification skipped at budget stop)
	// anchored to a stale hash so the next change re-verifies it.
	fp := domain.Fingerprint("nil-deref", fixtureFile, funnel.NewLocusResolver(fr.dir).Resolve(fixtureFile, fixtureLine))
	seeded, err := st.UpsertFinding(context.Background(), domain.Finding{
		Fingerprint: fp,
		Title:       "possible nil dereference",
		Severity:    "high",
		Tier:        3,
		Status:      domain.StatusOpen,
		Lens:        "nil-deref",
		File:        fixtureFile,
		Line:        fixtureLine,
		CommitSHA:   base,
		FileHash:    "deadbeef", // mismatched on purpose so re-verify engages
	})
	if err != nil {
		t.Fatal(err)
	}

	// Touch the implicated file so polling detects a change; the refuters vote
	// "not refuted", so the finding survives the full verification it never got.
	fr.write(fixtureFile, "package p\n\nfunc f(x *int) int { return *x } // touched\n")
	fr.commit("touch bug.go")

	llmc := newFakeLLM(emptyJSON, notRefutedJSON)
	clk := newFakeClock(mustTime(t, testStart))
	cfg := DaemonConfig{
		PollInterval:   10 * time.Millisecond,
		SweepInterval:  time.Hour,
		PerCycleTokens: 1_000_000,
		PerDayTokens:   10_000_000,
	}
	d, _ := buildDaemon(t, fr, st, llmc, cfg, clk)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runInBackground(ctx, d)

	if !clk.fire(ctx, t) {
		t.Fatal("clock fire failed")
	}

	waitFor(t, func() bool {
		f, gerr := st.GetFinding(context.Background(), seeded.ID)
		return gerr == nil && f.Tier == 2
	})

	f, err := st.GetFinding(context.Background(), seeded.ID)
	if err != nil {
		t.Fatal(err)
	}
	if f.Tier != 2 || f.Status != domain.StatusOpen {
		t.Fatalf("expected open tier-2 after surviving re-verification, got tier=%d status=%q", f.Tier, f.Status)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 5. Graceful shutdown: cancelling mid-idle returns from Run promptly.
// ---------------------------------------------------------------------------

func TestDaemonGracefulShutdown(t *testing.T) {
	fr := newFixtureRepo(t)
	fr.write(fixtureFile, "package p\n")
	base := fr.commit("init")

	st := openStore(t)
	if _, err := st.BeginScanRun(context.Background(), store.ScanSweep, base); err != nil {
		t.Fatal(err)
	}

	llmc := newFakeLLM(emptyJSON, notRefutedJSON)
	clk := newFakeClock(mustTime(t, testStart))
	cfg := DaemonConfig{
		PollInterval:   time.Hour, // never fires during the test
		SweepInterval:  time.Hour,
		PerCycleTokens: 1_000_000,
		PerDayTokens:   10_000_000,
	}
	d, _ := buildDaemon(t, fr, st, llmc, cfg, clk)

	ctx, cancel := context.WithCancel(context.Background())
	done := runInBackground(ctx, d)

	// The scheduler is parked on its first timer. Cancel; Run must return promptly
	// without firing the timer.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error on shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of cancellation")
	}
}

// ---------------------------------------------------------------------------
// 6. Repro promotion: when enabled with a promoter, a sweep's new T2 findings
//    are handed to PromoteAll.
// ---------------------------------------------------------------------------

func TestDaemonReproPromotionHandsT2Findings(t *testing.T) {
	fr := newFixtureRepo(t)
	fr.write(fixtureFile, "package p\n\nfunc f(x *int) int { return *x }\n")
	fr.commit("init") // no prior sweep -> startup sweep fires

	st := openStore(t)
	llmc := newFakeLLM(candidateJSON, notRefutedJSON)
	clk := newFakeClock(mustTime(t, testStart))
	cfg := DaemonConfig{
		PollInterval:   time.Hour,
		SweepInterval:  10 * time.Millisecond,
		PerCycleTokens: 1_000_000,
		PerDayTokens:   10_000_000,
		EnableRepro:    true,
	}
	prom := &fakePromoter{}
	sink := &captureSink{}
	d, err := New(Deps{
		Repo:       fr.open(),
		Store:      st,
		Clients:    funnel.RoleClients{Finder: llmc, Verifier: llmc},
		Reproducer: prom,
		FunnelOpts: funnel.Options{Limits: funnel.StageLimits{Refuters: 1, MaxParallel: 2}},
		Sinks:      []report.Sink{sink},
		Logger:     discardLogger(),
	}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	d.clock = clk

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runInBackground(ctx, d)

	if !clk.fire(ctx, t) { // startup sweep
		t.Fatal("clock fire failed")
	}
	waitFor(t, func() bool { return prom.calls() >= 1 })

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if prom.calls() < 1 {
		t.Fatal("expected PromoteAll to be called with the sweep's T2 findings")
	}
	if got := prom.attempts[0]; len(got) == 0 {
		t.Fatal("expected at least one T2 finding handed to PromoteAll")
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// waitFor polls cond until true or a 2s deadline, failing the test on timeout.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !cond() {
		t.Fatal("condition not met within 2s")
	}
}

func countScanRuns(t *testing.T, st *store.Store, kind store.ScanKind) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(1) FROM scan_runs WHERE kind = ?`, string(kind)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// readIdleMult / readNextPollDelay read scheduler state. idleMultiplier is an
// atomic, so cross-goroutine reads are race-free; nextPollDelay derives from it.
func readIdleMult(d *Daemon) int                { return int(d.idleMultiplier.Load()) }
func readNextPollDelay(d *Daemon) time.Duration { return d.nextPollDelay() }
