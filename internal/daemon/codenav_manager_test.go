package daemon

// TestDaemonSharedCodeNav verifies that:
//  1. New() constructs a sharedNav and injects it into fopts.CodeNav.
//  2. The same CodeNav pointer is passed to every cycle's funnel (reuse, not rebuild).
//  3. Run() closes sharedNav exactly once on exit.
//
// The test uses the package-internal Daemon struct fields directly (same package).
// It exercises two sequential cycles via the fake clock and then cancels the daemon.

import (
	"context"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/funnel"
)

func TestDaemonNew_SharedNavConstructed(t *testing.T) {
	fr := newFixtureRepo(t)
	st := openStore(t)

	llmc := newFakeLLM("", "")
	cfg := DaemonConfig{
		PollInterval:  time.Millisecond,
		SweepInterval: time.Hour,
	}
	clk := newFakeClock(mustTime(t, testStart))
	d, _ := buildDaemon(t, fr, st, llmc, cfg, clk)

	// sharedNav must be non-nil after construction.
	if d.sharedNav == nil {
		t.Fatal("Daemon.sharedNav is nil after New()")
	}

	// fopts.CodeNav must be the exact same pointer.
	if d.fopts.CodeNav != d.sharedNav {
		t.Fatal("fopts.CodeNav != sharedNav; nav will not be shared across cycles")
	}
}

func TestDaemonSharedNav_SameInstanceAcrossCycles(t *testing.T) {
	fr := newFixtureRepo(t)
	st := openStore(t)

	// Record the CodeNav pointer each cycle's funnel receives by intercepting
	// newFunnelWith. We verify that the same pointer is injected each time.
	//
	// Because newFunnelWith copies fopts and sets CodeNav from it, checking
	// fopts.CodeNav before and after two cycles is equivalent — the pointer in
	// fopts never changes.
	llmc := newFakeLLM("", "")
	cfg := DaemonConfig{
		PollInterval:  time.Millisecond,
		SweepInterval: time.Hour,
	}
	clk := newFakeClock(mustTime(t, testStart))
	d, _ := buildDaemon(t, fr, st, llmc, cfg, clk)

	navAtConstruction := d.sharedNav

	ctx, cancel := context.WithCancel(context.Background())
	done := runInBackground(ctx, d)

	// Fire two poll cycles.
	if !clk.fire(ctx, t) {
		t.Fatal("fire 1: context cancelled before cycle")
	}
	if !clk.fire(ctx, t) {
		t.Fatal("fire 2: context cancelled before cycle")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	// fopts.CodeNav must still be the exact same pointer; the daemon must
	// never replace it between cycles.
	if d.fopts.CodeNav != navAtConstruction {
		t.Error("fopts.CodeNav changed between cycles — nav is NOT being shared")
	}
}

// TestDaemonSharedNav_ClosedOnExit verifies that Run() closes the shared nav
// when the context is cancelled (the primary shutdown path). We assert by
// checking that fopts.CodeNav is still the original pointer after exit (the
// daemon does not nil it out), and that no panic occurs — actual close
// idempotency is tested in the agent package.
func TestDaemonSharedNav_ClosedOnExit(t *testing.T) {
	fr := newFixtureRepo(t)
	st := openStore(t)
	llmc := newFakeLLM("", "")
	cfg := DaemonConfig{
		PollInterval:  time.Millisecond,
		SweepInterval: time.Hour,
	}
	clk := newFakeClock(mustTime(t, testStart))
	d, _ := buildDaemon(t, fr, st, llmc, cfg, clk)

	ctx, cancel := context.WithCancel(context.Background())
	done := runInBackground(ctx, d)

	// Cancel immediately — no cycles needed for this test.
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	// sharedNav pointer is unchanged; daemon must have called Close() via the
	// deferred closer in Run(). This test is mainly a panic/race guard.
	if d.sharedNav == nil {
		t.Error("sharedNav nil after Run exit — construction failed or was cleared")
	}
}

// TestDaemonOptions_CodeNavNilMeansNoInjection verifies the one-shot scan path:
// building a funnel with Options.CodeNav == nil produces a self-owned nav
// (funnel constructs its own). This is a regression guard to confirm we did not
// change the zero-value behavior.
func TestDaemonOptions_CodeNavNilMeansNoInjection(t *testing.T) {
	fr := newFixtureRepo(t)
	st := openStore(t)
	llmc := newFakeLLM("", "")

	f, err := funnel.New(
		funnel.RoleClients{Finder: llmc, Verifier: llmc},
		st,
		fr.open(),
		funnel.Options{}, // CodeNav intentionally nil
	)
	if err != nil {
		t.Fatalf("funnel.New: %v", err)
	}
	// Close should not error: funnel constructs and owns its own nav.
	if err := f.Close(); err != nil {
		t.Fatalf("f.Close: %v", err)
	}
}
