package engine

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/store"
)

// testConfig returns a minimal config pointing Storage.Path at a fresh
// on-disk database under t.TempDir().
func testConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Storage.Path = filepath.Join(t.TempDir(), "state.db")
	return cfg
}

// seedActiveForeignRun creates a scan_runs row with a fresh heartbeat and a
// foreign pid (so checkScanLock, used by ensureOwner, treats it as a real
// conflict rather than a same-process re-entrant run), then closes its setup
// store handle before returning so it does not contend with Open's probe.
func seedActiveForeignRun(t *testing.T, ctx context.Context, cfg config.Config) {
	t.Helper()
	seed, err := store.Open(ctx, cfg.Storage.Path)
	if err != nil {
		t.Fatalf("seed store.Open() error = %v", err)
	}
	id, err := seed.BeginScanRun(ctx, store.ScanOneshot, "abc")
	if err != nil {
		t.Fatalf("BeginScanRun() error = %v", err)
	}
	if err := seed.UpdateHeartbeat(ctx, id); err != nil {
		t.Fatalf("UpdateHeartbeat() error = %v", err)
	}
	if _, err := seed.DB().ExecContext(ctx, `UPDATE scan_runs SET pid = 99999 WHERE id = ?`, id); err != nil {
		t.Fatalf("set foreign pid: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("seed store.Close() error = %v", err)
	}
}

// TestOpen_OwnerWhenLockFree verifies that Open selects Owner mode against a
// fresh store with no scan_runs rows, and that it can dispatch a verb without
// hitting ErrObserver.
func TestOpen_OwnerWhenLockFree(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)

	d, err := Open(ctx, cfg, nil)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = d.Close() }()

	if d.Mode() != Owner {
		t.Fatalf("Mode() = %v, want Owner", d.Mode())
	}

	// Sweep's ensureOwner gate must not refuse in Owner mode. It will fail
	// downstream (no LLM providers configured in this test), but that error
	// must NOT be ErrObserver.
	_, verbErr := d.Sweep(ctx, SweepOpts{Target: t.TempDir(), Out: io.Discard, ErrOut: io.Discard})
	if errors.Is(verbErr, ErrObserver) {
		t.Errorf("Sweep() in Owner mode returned ErrObserver: %v", verbErr)
	}
}

// TestOpen_ObserverWhenScanActive verifies that Open selects Observer mode
// when a live scan_run heartbeat exists, and that dispatch verbs gated by the
// advisory lock (Sweep/Verify, which had --force in main) refuse with
// ErrObserver, while Repro (which had NO lock gate in main) escalates to
// Owner unconditionally and proceeds instead of refusing.
func TestOpen_ObserverWhenScanActive(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	seedActiveForeignRun(t, ctx, cfg)

	d, err := Open(ctx, cfg, nil)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = d.Close() }()

	if d.Mode() != Observer {
		t.Fatalf("Mode() = %v, want Observer", d.Mode())
	}

	if _, err := d.Sweep(ctx, SweepOpts{Target: t.TempDir(), Out: io.Discard, ErrOut: io.Discard}); !errors.Is(err, ErrObserver) {
		t.Errorf("Sweep() in Observer mode error = %v, want ErrObserver", err)
	}
	if _, err := d.Verify(ctx, VerifyOpts{Target: t.TempDir(), Out: io.Discard}); !errors.Is(err, ErrObserver) {
		t.Errorf("Verify() in Observer mode error = %v, want ErrObserver", err)
	}
}

// TestRepro_EscalatesUnconditionally verifies that Dispatcher.Repro never
// refuses with ErrObserver: main's `bugbot repro` had no advisory-lock gate
// at all, so Repro must escalate Observer -> Owner unconditionally (like a
// bare store.Open) rather than honoring the ActiveScanRuns heuristic.
func TestRepro_EscalatesUnconditionally(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	seedActiveForeignRun(t, ctx, cfg)

	d, err := Open(ctx, cfg, nil)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = d.Close() }()

	if d.Mode() != Observer {
		t.Fatalf("Mode() = %v, want Observer", d.Mode())
	}

	if _, err := d.Repro(ctx, ReproOpts{Target: t.TempDir(), Out: io.Discard}); errors.Is(err, ErrObserver) {
		t.Errorf("Repro() returned ErrObserver, want unconditional escalation (main had no lock gate): %v", err)
	}
	if d.Mode() != Owner {
		t.Errorf("Mode() after Repro() = %v, want Owner (Repro must escalate unconditionally)", d.Mode())
	}
}

// TestEnsureOwner_ForceEscalates verifies that Force=true escalates an
// Observer Dispatcher to Owner (mirroring checkScanLock's force bypass) so a
// subsequent dispatch is not refused with ErrObserver.
func TestEnsureOwner_ForceEscalates(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	seedActiveForeignRun(t, ctx, cfg)

	d, err := Open(ctx, cfg, nil)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = d.Close() }()

	if d.Mode() != Observer {
		t.Fatalf("Mode() = %v, want Observer", d.Mode())
	}

	if err := d.ensureOwner(ctx, true); err != nil {
		t.Fatalf("ensureOwner(force=true) error = %v", err)
	}
	if d.Mode() != Owner {
		t.Errorf("Mode() after forced ensureOwner = %v, want Owner", d.Mode())
	}
}
