package tui

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/store"
)

// runTestConfig returns a minimal config pointing Storage.Path at a fresh
// on-disk database under t.TempDir(), mirroring internal/engine's own
// testConfig helper (unexported there, so duplicated here).
func runTestConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Storage.Path = filepath.Join(t.TempDir(), "state.db")
	return cfg
}

// seedActiveForeignRun creates a scan_runs row with a fresh heartbeat and a
// foreign pid, mirroring internal/engine's seedActiveForeignRun (unexported
// there): this is what makes openStoreForMode's ActiveScanRuns probe treat
// the store as actively owned by another process.
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

// TestSelectFeed_OwnerWhenLockFree verifies that with no active scan_runs
// row and the writer lock free, selectFeed chooses a LiveFeed (Owner mode).
func TestSelectFeed_OwnerWhenLockFree(t *testing.T) {
	ctx := context.Background()
	cfg := runTestConfig(t)

	feed, cleanup, err := selectFeed(ctx, cfg)
	if err != nil {
		t.Fatalf("selectFeed() error = %v", err)
	}
	defer cleanup()

	if _, ok := feed.(*LiveFeed); !ok {
		t.Fatalf("selectFeed() feed = %T, want *LiveFeed", feed)
	}
	if feed.Mode() != Owner {
		t.Errorf("Mode() = %v, want Owner", feed.Mode())
	}
}

// TestSelectFeed_ObserverWhenScanActive verifies that a live foreign
// scan_runs heartbeat makes selectFeed fall back to a read-only SnapshotFeed
// (Observer mode) rather than a dispatch-capable LiveFeed.
func TestSelectFeed_ObserverWhenScanActive(t *testing.T) {
	ctx := context.Background()
	cfg := runTestConfig(t)
	seedActiveForeignRun(t, ctx, cfg)

	feed, cleanup, err := selectFeed(ctx, cfg)
	if err != nil {
		t.Fatalf("selectFeed() error = %v", err)
	}
	defer cleanup()

	if _, ok := feed.(*SnapshotFeed); !ok {
		t.Fatalf("selectFeed() feed = %T, want *SnapshotFeed", feed)
	}
	if feed.Mode() != Observer {
		t.Errorf("Mode() = %v, want Observer", feed.Mode())
	}
}

// TestSelectFeed_ErrLockedFallsBackToObserver verifies that when the writer
// lock is held (an idle Owner cockpit elsewhere: no active scan_runs row, so
// the ActiveScanRuns probe sees nothing, but store.Open itself hits the
// flock) selectFeed falls back to SnapshotFeed instead of propagating the
// error and crashing the TUI.
func TestSelectFeed_ErrLockedFallsBackToObserver(t *testing.T) {
	ctx := context.Background()
	cfg := runTestConfig(t)

	// Hold the writer lock ourselves, with no scan_runs row at all — the
	// idle-Owner-cockpit scenario the doc comment describes.
	holder, err := store.Open(ctx, cfg.Storage.Path)
	if err != nil {
		t.Fatalf("holder store.Open() error = %v", err)
	}
	defer func() { _ = holder.Close() }()

	feed, cleanup, err := selectFeed(ctx, cfg)
	if err != nil {
		t.Fatalf("selectFeed() error = %v, want fallback to Observer instead of an error", err)
	}
	defer cleanup()

	if _, ok := feed.(*SnapshotFeed); !ok {
		t.Fatalf("selectFeed() feed = %T, want *SnapshotFeed (ErrLocked fallback)", feed)
	}
	if feed.Mode() != Observer {
		t.Errorf("Mode() = %v, want Observer", feed.Mode())
	}
}
