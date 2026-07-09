package daemon

// reconcile_test.go covers the bugbot-ezmx.4 daemon-level acceptance
// criteria for the backlog-reconcile timer: it fires on its own cadence, is
// idempotent across repeated firings, and is gated on both storeHealthy and
// the day budget exactly like the other maintenance drains.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/report"
	"github.com/dpoage/bugbot/internal/store"
)

// closeDescA / closeDescB are a SimilarFinding-close pair (same file/window,
// jaccard well above funnel.DefaultMergeWindow's threshold): worded almost
// identically so the deterministic reconcile pre-gate nominates them.
const (
	reconcileDescA = "the config pointer may be nil and is dereferenced without a guard in Greeting"
	reconcileDescB = "the config pointer may be nil and is dereferenced without any guard inside Greeting"
)

// mustUpsertFinding upserts f into st, failing the test on error.
func mustUpsertFinding(t *testing.T, st *store.Store, f domain.Finding) domain.Finding {
	t.Helper()
	got, err := st.UpsertFinding(context.Background(), f)
	if err != nil {
		t.Fatalf("UpsertFinding %q: %v", f.Title, err)
	}
	return got
}

// seedReconcilePair seeds two OPEN findings at fixtureFile, same/unknown
// defect_kind, SimilarFinding-close descriptions, within the merge window --
// a pair the reconcile cycle's deterministic pre-gate must nominate. older
// is seeded first so it sorts as the canonical row.
func seedReconcilePair(t *testing.T, st *store.Store) (older, newer domain.Finding) {
	t.Helper()
	if !funnel.SimilarFinding(fixtureFile, 10, reconcileDescA, fixtureFile, 12, reconcileDescB) {
		t.Fatal("fixture broken: reconcileDescA/B at lines 10/12 are not SimilarFinding-close")
	}
	older = mustUpsertFinding(t, st, domain.Finding{
		Fingerprint: domain.Fingerprint("nil-safety", fixtureFile, "10|older dup"),
		Title:       "older dup", Description: reconcileDescA, Severity: "high", Tier: 2,
		Status: domain.StatusOpen, Lens: "nil-safety", File: fixtureFile, Line: 10,
		DefectKind: domain.DefectNilDeref, Subject: "older dup",
	})
	newer = mustUpsertFinding(t, st, domain.Finding{
		Fingerprint: domain.Fingerprint("resource-leaks", fixtureFile, "12|newer dup"),
		Title:       "newer dup", Description: reconcileDescB, Severity: "high", Tier: 2,
		Status: domain.StatusOpen, Lens: "resource-leaks", File: fixtureFile, Line: 12,
		DefectKind: domain.DefectNilDeref, Subject: "newer dup",
	})
	return older, newer
}

// TestDaemonReconcileFiresOnCadence: with a short reconcile interval and
// every other timer parked, the reconcile timer fires, merges the seeded
// duplicate pair (arbiter "yes"), and closes the newer row with a reason
// referencing the canonical (older) fingerprint.
func TestDaemonReconcileFiresOnCadence(t *testing.T) {
	fr := newFixtureRepo(t)
	fr.write(fixtureFile, "package p\n\nfunc f(x *int) int { return *x }\n")
	base := fr.commit("init")

	st := openStore(t)
	markPriorSweep(t, st, base)
	older, newer := seedReconcilePair(t, st)

	llmc := newFakeLLM(emptyJSON, notRefutedJSON)
	llmc.dedupBody = dedupVerdictJSON("yes", "same nil-deref defect, worded differently")
	clk := newFakeClock(mustTime(t, testStart))
	prog := &recordingProgSink{}
	cfg := DaemonConfig{
		PollInterval:        time.Hour,
		SweepInterval:       time.Hour,
		PerCycleTokens:      1_000_000,
		PerDayTokens:        10_000_000,
		VerifyDrainInterval: time.Hour,
		ImpactSweepInterval: time.Hour,
		ReconcileInterval:   10 * time.Millisecond, // nearest deadline -> fires first
	}
	d := buildDrainDaemon(t, fr, st, llmc, cfg, clk, prog)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runInBackground(ctx, d)

	if !clk.fire(ctx, t) {
		t.Fatal("clock fire failed")
	}
	waitFor(t, func() bool { return prog.finishedCount(store.ScanReconcile) >= 1 })

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if got := countScanRuns(t, st, store.ScanReconcile); got < 1 {
		t.Fatalf("expected >=1 reconcile scan run, got %d", got)
	}

	gotNewer, err := st.GetFinding(context.Background(), newer.ID)
	if err != nil {
		t.Fatalf("GetFinding(newer): %v", err)
	}
	if gotNewer.Status != domain.StatusSuperseded {
		t.Fatalf("newer.Status = %q, want %q", gotNewer.Status, domain.StatusSuperseded)
	}
	if gotNewer.SupersededBy != older.Fingerprint {
		t.Fatalf("newer.SupersededBy = %q, want canonical fingerprint %q", gotNewer.SupersededBy, older.Fingerprint)
	}

	gotOlder, err := st.GetFinding(context.Background(), older.ID)
	if err != nil {
		t.Fatalf("GetFinding(older): %v", err)
	}
	if gotOlder.Status != domain.StatusOpen {
		t.Fatalf("older.Status = %q, want unchanged %q (canonical survives)", gotOlder.Status, domain.StatusOpen)
	}
}

// TestDaemonReconcile_IdempotentOnRepeatedFire: firing the reconcile timer a
// second time after the first merge must be a clean no-op -- exactly one
// scan run and exactly one arbiter call total across both firings, matching
// funnel.ReconcileDedup's idempotent-replay contract.
func TestDaemonReconcile_IdempotentOnRepeatedFire(t *testing.T) {
	fr := newFixtureRepo(t)
	fr.write(fixtureFile, "package p\n\nfunc f(x *int) int { return *x }\n")
	base := fr.commit("init")

	st := openStore(t)
	markPriorSweep(t, st, base)
	seedReconcilePair(t, st)

	llmc := newFakeLLM(emptyJSON, notRefutedJSON)
	llmc.dedupBody = dedupVerdictJSON("yes", "same nil-deref defect, worded differently")
	clk := newFakeClock(mustTime(t, testStart))
	prog := &recordingProgSink{}
	cfg := DaemonConfig{
		PollInterval:        time.Hour,
		SweepInterval:       time.Hour,
		PerCycleTokens:      1_000_000,
		PerDayTokens:        10_000_000,
		VerifyDrainInterval: time.Hour,
		ImpactSweepInterval: time.Hour,
		ReconcileInterval:   10 * time.Millisecond,
	}
	d := buildDrainDaemon(t, fr, st, llmc, cfg, clk, prog)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runInBackground(ctx, d)

	if !clk.fire(ctx, t) {
		t.Fatal("first clock fire failed")
	}
	waitFor(t, func() bool { return prog.finishedCount(store.ScanReconcile) >= 1 })

	if !clk.fire(ctx, t) {
		t.Fatal("second clock fire failed")
	}
	waitFor(t, func() bool { return prog.finishedCount(store.ScanReconcile) >= 2 })

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if got := countScanRuns(t, st, store.ScanReconcile); got != 1 {
		t.Fatalf("expected exactly 1 reconcile scan run across two firings (idempotent replay), got %d", got)
	}
	if llmc.callCount() != 1 {
		t.Fatalf("expected exactly 1 arbiter call across two firings, got %d", llmc.callCount())
	}
}

// TestDaemonReconcile_BudgetGated: when the day budget is exhausted, the
// reconcile timer fires but the cycle is skipped -- no scan run, no arbiter
// calls, and the seeded duplicate stays open (unmerged).
func TestDaemonReconcile_BudgetGated(t *testing.T) {
	fr := newFixtureRepo(t)
	fr.write(fixtureFile, "package p\n\nfunc f(x *int) int { return *x }\n")
	base := fr.commit("init")

	st := openStore(t)
	markPriorSweep(t, st, base)
	_, newer := seedReconcilePair(t, st)
	if _, err := st.RecordSpend(context.Background(), store.Spend{
		Role: "finder", InputTokens: 600_000,
	}); err != nil {
		t.Fatal(err)
	}

	llmc := newFakeLLM(emptyJSON, notRefutedJSON)
	llmc.dedupBody = dedupVerdictJSON("yes", "same nil-deref defect")
	clk := newFakeClock(mustTime(t, testStart))
	prog := &recordingProgSink{}
	cfg := DaemonConfig{
		PollInterval:        time.Hour,
		SweepInterval:       time.Hour,
		PerCycleTokens:      100_000,
		PerDayTokens:        500_000, // already exceeded by the pre-recorded 600k
		VerifyDrainInterval: time.Hour,
		ImpactSweepInterval: time.Hour,
		ReconcileInterval:   10 * time.Millisecond,
	}
	d := buildDrainDaemon(t, fr, st, llmc, cfg, clk, prog)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runInBackground(ctx, d)

	if !clk.fire(ctx, t) {
		t.Fatal("clock fire failed")
	}
	// The gated cycle emits no cycle-finished, but the loop comes back around
	// and emits a second KindCycleScheduled -- a deterministic "skip processed"
	// signal, mirroring TestDaemonVerifyDrainBudgetGated.
	waitFor(t, func() bool { return prog.scheduledCount() >= 2 })

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if got := countScanRuns(t, st, store.ScanReconcile); got != 0 {
		t.Fatalf("budget-exhausted reconcile must not open a scan run, got %d", got)
	}
	if llmc.callCount() != 0 {
		t.Fatalf("budget-exhausted reconcile must make zero LLM calls, got %d", llmc.callCount())
	}
	if got := prog.finishedCount(store.ScanReconcile); got != 0 {
		t.Fatalf("budget-exhausted reconcile must not log a finished cycle, got %d", got)
	}
	got, err := st.GetFinding(context.Background(), newer.ID)
	if err != nil {
		t.Fatalf("GetFinding: %v", err)
	}
	if got.Status != domain.StatusOpen {
		t.Fatalf("budget-exhausted reconcile must leave the duplicate open, got status %q", got.Status)
	}
}

// TestDaemonReconcile_StoreHealthyGate_NoWrites proves the integrity gate
// bugbot-ezmx.4 explicitly asked for (unlike runVerifyDrain's known
// bugbot-lcr0 gap): a corrupt state db skips runReconcile entirely -- zero
// arbiter calls, zero scan runs -- before any write is attempted. This calls
// d.runReconcile directly rather than driving the full scheduler loop,
// because hasSweepRun (Run's startup query) would itself fail against a
// corrupted store before the reconcile timer ever got a chance to fire.
func TestDaemonReconcile_StoreHealthyGate_NoWrites(t *testing.T) {
	fr := newFixtureRepo(t)
	fr.write(fixtureFile, "package p\n\nfunc f(x *int) int { return *x }\n")
	fr.commit("init")

	path := filepath.Join(t.TempDir(), "state.db")
	seedSt, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	seedReconcilePair(t, seedSt)
	// Filler rows so the file has enough pages that corrupting the back half
	// (below) cannot also clip the front schema page out of existence --
	// mirrors store.TestCheck_DetectsCorruptionAsErrCorrupt's 60-row fixture,
	// which exists for exactly this reason (a too-small db can fail to REOPEN
	// at all, which is a valid but less specific proof than Check returning
	// ErrCorrupt on a live handle).
	for i := 0; i < 60; i++ {
		mustUpsertFinding(t, seedSt, domain.Finding{
			Fingerprint: domain.Fingerprint("logic", "filler.go", fmt.Sprintf("%d", i)),
			Title:       fmt.Sprintf("filler %d", i), Description: "filler row to pad the db file",
			Severity: "low", Tier: 3, Status: domain.StatusOpen,
			Lens: "logic", File: "filler.go", Line: i + 1, DefectKind: domain.DefectLogic,
		})
	}
	if err := seedSt.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Corrupt the back half of the file, mirroring
	// store.TestCheck_DetectsCorruptionAsErrCorrupt: every page there is live
	// table/index data, so this reliably trips quick_check while the header
	// and schema (at the front) stay intact enough for the file to still open.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	fh, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open for corruption: %v", err)
	}
	garbage := make([]byte, int(info.Size()-info.Size()/2))
	for i := range garbage {
		garbage[i] = 0xFF
	}
	if _, err := fh.WriteAt(garbage, info.Size()/2); err != nil {
		t.Fatalf("corrupt: %v", err)
	}
	_ = fh.Close()

	st2, openErr := store.OpenReadOnly(context.Background(), path)
	if openErr != nil {
		t.Skipf("db too damaged to reopen (%v); cannot exercise the storeHealthy gate this run", openErr)
	}
	defer func() { _ = st2.Close() }()
	if checkErr := st2.Check(context.Background()); checkErr == nil || !errors.Is(checkErr, store.ErrCorrupt) {
		t.Fatalf("fixture broken: st2.Check() = %v, want errors.Is(_, store.ErrCorrupt)", checkErr)
	}

	llmc := newFakeLLM(emptyJSON, notRefutedJSON)
	llmc.dedupBody = dedupVerdictJSON("yes", "same nil-deref defect")
	prog := &recordingProgSink{}
	d, err := New(Deps{
		Repo:       fr.open(),
		Store:      st2,
		Clients:    funnel.RoleClients{Finder: llmc, Verifier: llmc},
		FunnelOpts: funnel.Options{Limits: funnel.StageLimits{Refuters: 1, MaxParallel: 2}},
		Sinks:      []report.Sink{&captureSink{}},
		Logger:     discardLogger(),
		Progress:   prog,
	}, DaemonConfig{
		PollInterval:        time.Hour,
		SweepInterval:       time.Hour,
		PerCycleTokens:      1_000_000,
		PerDayTokens:        10_000_000,
		VerifyDrainInterval: time.Hour,
		ImpactSweepInterval: time.Hour,
		ReconcileInterval:   time.Hour,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.clock = newFakeClock(mustTime(t, testStart))

	d.runReconcile(context.Background())

	if llmc.callCount() != 0 {
		t.Fatalf("storeHealthy=false must make zero LLM calls, got %d", llmc.callCount())
	}
	if got := countScanRuns(t, st2, store.ScanReconcile); got != 0 {
		t.Fatalf("storeHealthy=false must not open a scan run, got %d", got)
	}
	if got := prog.finishedCount(store.ScanReconcile); got != 0 {
		t.Fatalf("storeHealthy=false must not log a finished cycle, got %d", got)
	}
}
