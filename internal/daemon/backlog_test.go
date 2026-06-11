package daemon

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/report"
	"github.com/dpoage/bugbot/internal/store"
)

// ---------------------------------------------------------------------------
// buildDaemonWithRepro is like buildDaemon but also wires a Reproducer and
// enables the backlog timer. The backlog interval and batch are caller-supplied
// so each test can tune them.
// ---------------------------------------------------------------------------

func buildDaemonWithRepro(
	t *testing.T,
	fr *fixtureRepo,
	st *store.Store,
	llmc *fakeLLM,
	prom *fakePromoter,
	backlogInterval time.Duration,
	batchSize int,
	clk *fakeClock,
) *Daemon {
	t.Helper()
	cfg := DaemonConfig{
		PollInterval:         time.Hour, // never fires during these tests
		SweepInterval:        time.Hour,
		PerCycleTokens:       1_000_000,
		PerDayTokens:         10_000_000,
		EnableRepro:          true,
		ReproBacklogInterval: backlogInterval,
		ReproBacklogBatch:    batchSize,
	}
	d, err := New(Deps{
		Repo:       fr.open(),
		Store:      st,
		Clients:    funnel.RoleClients{Finder: llmc, Verifier: llmc},
		Reproducer: prom,
		FunnelOpts: funnel.Options{Refuters: 1, MaxParallel: 2},
		Sinks:      []report.Sink{&captureSink{}},
		Logger:     discardLogger(),
	}, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.clock = clk
	return d
}

// seedFinding inserts a finding into st with the given tier, reproPath, and
// needsHuman flag. Status is always StatusOpen. Returns the upserted finding.
func seedFinding(t *testing.T, st *store.Store, title string, tier int, reproPath string, needsHuman bool) store.Finding {
	t.Helper()
	fp := store.Fingerprint("nil-deref", fixtureFile, fixtureLine, title)
	f, err := st.UpsertFinding(context.Background(), store.Finding{
		Fingerprint: fp,
		Title:       title,
		Severity:    "high",
		Tier:        tier,
		Status:      store.StatusOpen,
		Lens:        "nil-deref",
		File:        fixtureFile,
		Line:        fixtureLine,
		CommitSHA:   "abc123",
		FileHash:    "deadbeef",
		ReproPath:   reproPath,
		NeedsHuman:  needsHuman,
	})
	if err != nil {
		t.Fatalf("seedFinding %q: %v", title, err)
	}
	return f
}

// ---------------------------------------------------------------------------
// TestOpenBacklog_Filter: the helper returns exactly the eligible findings.
// ---------------------------------------------------------------------------

func TestOpenBacklog_Filter(t *testing.T) {
	st := openStore(t)

	// Eligible: T2, no repro path, no needs-human
	eligible1 := seedFinding(t, st, "eligible t2", 2, "", false)
	// Eligible: T3, no repro path, no needs-human
	eligible2 := seedFinding(t, st, "eligible t3", 3, "", false)
	// Ineligible: T2 but has a repro path
	seedFinding(t, st, "has repro", 2, "/some/path", false)
	// Ineligible: T2 but needs-human
	seedFinding(t, st, "needs human", 2, "", true)
	// Ineligible: T1 (already promoted)
	seedFinding(t, st, "already t1", 1, "/path", false)

	backlog, err := OpenBacklog(context.Background(), st)
	if err != nil {
		t.Fatalf("OpenBacklog: %v", err)
	}

	got := make(map[string]bool, len(backlog))
	for _, f := range backlog {
		got[f.ID] = true
	}
	if !got[eligible1.ID] {
		t.Errorf("expected T2 no-repro finding in backlog")
	}
	if !got[eligible2.ID] {
		t.Errorf("expected T3 no-repro finding in backlog")
	}
	if len(backlog) != 2 {
		t.Errorf("OpenBacklog returned %d findings, want 2", len(backlog))
	}
}

// ---------------------------------------------------------------------------
// TestOpenBacklog_DismissedExcluded: dismissed findings must not appear.
// ---------------------------------------------------------------------------

func TestOpenBacklog_DismissedExcluded(t *testing.T) {
	st := openStore(t)

	fp := store.Fingerprint("nil-deref", fixtureFile, fixtureLine, "dismissed finding")
	_, err := st.UpsertFinding(context.Background(), store.Finding{
		Fingerprint: fp,
		Title:       "dismissed finding",
		Tier:        2,
		Status:      store.StatusDismissed,
		File:        fixtureFile,
		Line:        fixtureLine,
	})
	if err != nil {
		t.Fatal(err)
	}

	backlog, err := OpenBacklog(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if len(backlog) != 0 {
		t.Errorf("expected empty backlog for dismissed findings, got %d", len(backlog))
	}
}

// ---------------------------------------------------------------------------
// TestDaemonReproBacklogFiresPromoter: seeded eligible findings are handed to
// PromoteAll when the backlog timer fires.
// ---------------------------------------------------------------------------

func TestDaemonReproBacklogFiresPromoter(t *testing.T) {
	fr := newFixtureRepo(t)
	fr.write(fixtureFile, "package p\n")
	base := fr.commit("init")

	st := openStore(t)
	// Mark a prior sweep so the startup sweep is skipped (poll+sweep are both
	// set to 1h, so only the backlog timer fires).
	if _, err := st.BeginScanRun(context.Background(), store.ScanSweep, base); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertFileStates(context.Background(), []store.FileState{{
		Path: lastSeenSentinel, ContentHash: base, LastScannedCommit: base,
	}}); err != nil {
		t.Fatal(err)
	}

	// Seed eligible: T2 no-repro, T3 no-repro.
	eligible1 := seedFinding(t, st, "eligible t2", 2, "", false)
	eligible2 := seedFinding(t, st, "eligible t3", 3, "", false)
	// Seed ineligible: T2 with repro path, T2 needs-human.
	seedFinding(t, st, "has repro", 2, "/some/path", false)
	seedFinding(t, st, "needs human", 2, "", true)

	llmc := newFakeLLM(emptyJSON, notRefutedJSON)
	clk := newFakeClock(mustTime(t, testStart))
	prom := &fakePromoter{}
	d := buildDaemonWithRepro(t, fr, st, llmc, prom, 10*time.Millisecond, 10, clk)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runInBackground(ctx, d)

	// The backlog timer fires first (10ms vs 1h for poll and sweep).
	if !clk.fire(ctx, t) {
		t.Fatal("clock fire failed")
	}
	waitFor(t, func() bool { return prom.calls() >= 1 })

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if prom.calls() < 1 {
		t.Fatal("expected PromoteAll to be called")
	}
	got := prom.attempts[0]
	gotIDs := make(map[string]bool, len(got))
	for _, f := range got {
		gotIDs[f.ID] = true
	}
	if !gotIDs[eligible1.ID] {
		t.Errorf("expected eligible T2 finding in backlog batch")
	}
	if !gotIDs[eligible2.ID] {
		t.Errorf("expected eligible T3 finding in backlog batch")
	}
	// Ineligible findings must not appear.
	for _, f := range got {
		if f.ReproPath != "" {
			t.Errorf("finding with ReproPath=%q should not be in backlog", f.ReproPath)
		}
		if f.NeedsHuman {
			t.Errorf("finding with NeedsHuman=true should not be in backlog")
		}
		if f.Tier < 2 || f.Tier > 3 {
			t.Errorf("backlog finding has unexpected tier %d (want 2 or 3)", f.Tier)
		}
	}
}

// ---------------------------------------------------------------------------
// TestDaemonReproBacklogBatchBound: when more eligible findings exist than the
// batch size, only batch-size findings are passed to PromoteAll.
// ---------------------------------------------------------------------------

func TestDaemonReproBacklogBatchBound(t *testing.T) {
	fr := newFixtureRepo(t)
	fr.write(fixtureFile, "package p\n")
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

	// Seed 5 eligible findings, batch size = 2.
	for i := 0; i < 5; i++ {
		seedFinding(t, st, fmt.Sprintf("finding-%d", i), 2, "", false)
	}

	llmc := newFakeLLM(emptyJSON, notRefutedJSON)
	clk := newFakeClock(mustTime(t, testStart))
	prom := &fakePromoter{}
	const batchSize = 2
	d := buildDaemonWithRepro(t, fr, st, llmc, prom, 10*time.Millisecond, batchSize, clk)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runInBackground(ctx, d)

	if !clk.fire(ctx, t) {
		t.Fatal("clock fire failed")
	}
	waitFor(t, func() bool { return prom.calls() >= 1 })

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if prom.calls() < 1 {
		t.Fatal("expected PromoteAll to be called")
	}
	if got := len(prom.attempts[0]); got != batchSize {
		t.Errorf("PromoteAll received %d findings, want %d (batch cap)", got, batchSize)
	}
}

// ---------------------------------------------------------------------------
// TestDaemonReproBacklogBudgetExhausted: when the day budget is exhausted, the
// backlog step is skipped and PromoteAll is never called.
// ---------------------------------------------------------------------------

func TestDaemonReproBacklogBudgetExhausted(t *testing.T) {
	fr := newFixtureRepo(t)
	fr.write(fixtureFile, "package p\n")
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

	// Pre-record spend over the per-day budget.
	if _, err := st.RecordSpend(context.Background(), store.Spend{
		Role: "finder", InputTokens: 600_000,
	}); err != nil {
		t.Fatal(err)
	}

	// Seed an eligible finding so the backlog is non-empty.
	seedFinding(t, st, "eligible", 2, "", false)

	llmc := newFakeLLM(emptyJSON, notRefutedJSON)
	clk := newFakeClock(mustTime(t, testStart))
	prom := &fakePromoter{}
	d := buildDaemonWithRepro(t, fr, st, llmc, prom, 10*time.Millisecond, 5, clk)
	// Set per-day budget below the pre-recorded spend.
	d.cfg.PerDayTokens = 500_000

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runInBackground(ctx, d)

	// Fire the backlog timer.
	if !clk.fire(ctx, t) {
		t.Fatal("clock fire failed")
	}

	// Give the scheduler a moment to process the skip, then assert.
	time.Sleep(20 * time.Millisecond)

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if prom.calls() != 0 {
		t.Fatalf("expected PromoteAll NOT called when budget exhausted, got %d calls", prom.calls())
	}
}

// ---------------------------------------------------------------------------
// TestDaemonReproBacklogEmptyBacklog: when no eligible findings exist, the
// backlog step is a no-op (PromoteAll is never called).
// ---------------------------------------------------------------------------

func TestDaemonReproBacklogEmptyBacklog(t *testing.T) {
	fr := newFixtureRepo(t)
	fr.write(fixtureFile, "package p\n")
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
	// Seed only an ineligible finding (already has repro path).
	seedFinding(t, st, "already reproduced", 2, "/some/artifact", false)

	llmc := newFakeLLM(emptyJSON, notRefutedJSON)
	clk := newFakeClock(mustTime(t, testStart))
	prom := &fakePromoter{}
	d := buildDaemonWithRepro(t, fr, st, llmc, prom, 10*time.Millisecond, 5, clk)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runInBackground(ctx, d)

	if !clk.fire(ctx, t) {
		t.Fatal("clock fire failed")
	}
	// Give the step a moment to complete, then stop.
	time.Sleep(20 * time.Millisecond)

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if prom.calls() != 0 {
		t.Fatalf("expected PromoteAll NOT called with empty backlog, got %d calls", prom.calls())
	}
}

// ---------------------------------------------------------------------------
// TestDaemonReproBacklogDisabled: when EnableRepro is false, no backlog timer
// churn occurs and PromoteAll is never called.
// ---------------------------------------------------------------------------

func TestDaemonReproBacklogDisabled(t *testing.T) {
	fr := newFixtureRepo(t)
	fr.write(fixtureFile, "package p\n")
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
	seedFinding(t, st, "eligible", 2, "", false)

	llmc := newFakeLLM(emptyJSON, notRefutedJSON)
	clk := newFakeClock(mustTime(t, testStart))
	prom := &fakePromoter{}

	// Build daemon with repro DISABLED; the backlog timer must never fire.
	cfg := DaemonConfig{
		PollInterval:   time.Hour,
		SweepInterval:  time.Hour,
		PerCycleTokens: 1_000_000,
		PerDayTokens:   10_000_000,
		EnableRepro:    false, // explicitly disabled
	}
	d, err := New(Deps{
		Repo:       fr.open(),
		Store:      st,
		Clients:    funnel.RoleClients{Finder: llmc, Verifier: llmc},
		Reproducer: prom, // wired but EnableRepro=false
		FunnelOpts: funnel.Options{Refuters: 1, MaxParallel: 2},
		Sinks:      []report.Sink{&captureSink{}},
		Logger:     discardLogger(),
	}, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.clock = clk

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel immediately: poll and sweep timers are 1h out; the scheduler is
	// parked. Since the backlog timer is disabled there is nothing that would
	// fire sooner.
	cancel()
	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if prom.calls() != 0 {
		t.Fatalf("expected PromoteAll NOT called when repro disabled, got %d calls", prom.calls())
	}
}
