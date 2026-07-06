package tui

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/store"
)

// runTestConfig returns a minimal config pointing Storage.Path at a fresh
// on-disk database under t.TempDir()/.bugbot, mirroring internal/engine's
// own testConfig helper (unexported there, so duplicated here) but nested
// one level deeper — like the real ".bugbot/state.db" default — so tests
// can assert that a never-run repo has NO .bugbot directory at all, not
// just no state.db file (t.TempDir() itself always pre-exists).
func runTestConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Storage.Path = filepath.Join(t.TempDir(), ".bugbot", "state.db")
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

// seedExistingStore creates and closes an empty store at cfg's path,
// mirroring "bugbot has run here before" without leaving anything else
// behind (no scan_runs row, no findings) — the precondition selectFeed's
// storeExists gate requires before it will even consider Owner mode.
func seedExistingStore(t *testing.T, ctx context.Context, cfg config.Config) {
	t.Helper()
	seed, err := store.Open(ctx, cfg.Storage.Path)
	if err != nil {
		t.Fatalf("seed store.Open() error = %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("seed store.Close() error = %v", err)
	}
}

// TestSelectFeed_OwnerWhenLockFree verifies that with no active scan_runs
// row and the writer lock free, selectFeed chooses a LiveFeed (Owner mode)
// — once a store already exists (bugbot has run here before).
func TestSelectFeed_OwnerWhenLockFree(t *testing.T) {
	ctx := context.Background()
	cfg := runTestConfig(t)
	seedExistingStore(t, ctx, cfg)

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

// TestSelectFeed_FreshRepoStaysObserverAndCreatesNothing is the B1
// regression: launching the TUI against a config whose state DB has never
// been created must NOT scaffold a .bugbot directory or take the writer
// lock just because the lock happens to be free — engine.Open (and its
// underlying store.Open) must never even be called in this case. Mirrors
// the no-create-on-launch contract storeExists/NewSnapshotFeed have always
// enforced for the Observer path.
func TestSelectFeed_FreshRepoStaysObserverAndCreatesNothing(t *testing.T) {
	ctx := context.Background()
	cfg := runTestConfig(t)

	if _, err := os.Stat(cfg.Storage.Path); err == nil {
		t.Fatalf("state DB already exists before selectFeed ran: %s", cfg.Storage.Path)
	}

	feed, cleanup, err := selectFeed(ctx, cfg)
	if err != nil {
		t.Fatalf("selectFeed() error = %v", err)
	}
	defer cleanup()

	if _, ok := feed.(*SnapshotFeed); !ok {
		t.Fatalf("selectFeed() feed = %T, want *SnapshotFeed (no store yet => always Observer)", feed)
	}
	if feed.Mode() != Observer {
		t.Errorf("Mode() = %v, want Observer", feed.Mode())
	}
	if _, err := os.Stat(cfg.Storage.Path); err == nil {
		t.Errorf("selectFeed() created %s as a side effect of merely launching against a never-run repo", cfg.Storage.Path)
	}
	if _, err := os.Stat(filepath.Dir(cfg.Storage.Path)); err == nil {
		t.Errorf("selectFeed() created the storage directory %s as a side effect of merely launching", filepath.Dir(cfg.Storage.Path))
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
