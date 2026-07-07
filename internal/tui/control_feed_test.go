package tui

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/control"
	"github.com/dpoage/bugbot/internal/progress"
)

// testControlServer starts a real control.Server on a unix socket in
// t.TempDir and returns it plus its path, cleaned up automatically.
func testControlServer(t *testing.T, dispatch control.DispatchFunc) (*control.Server, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "daemon.sock")
	srv, err := control.Listen(path, dispatch, nil)
	if err != nil {
		t.Fatalf("control.Listen() error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		_ = srv.Close()
	})
	return srv, path
}

// TestControlSocketFeed_SatisfiesFeed is the compile-time-adjacent runtime
// check mirroring live_feed.go's var _ Feed assertion, plus Mode().
func TestControlSocketFeed_SatisfiesFeed(t *testing.T) {
	var _ Feed = (*ControlSocketFeed)(nil)

	_, path := testControlServer(t, nil)
	client, err := control.Dial(path)
	if err != nil {
		t.Fatalf("control.Dial() error: %v", err)
	}
	f := NewControlSocketFeed(client)
	f.interval = 20 * time.Millisecond
	defer f.Close()

	if f.Mode() != Attach {
		t.Errorf("Mode() = %v, want Attach", f.Mode())
	}
}

// TestControlSocketFeed_WireEventBecomesFrameMsg drives a real event through
// the socket server and asserts Next()'s tea.Cmd resolves to a FrameMsg
// reflecting the folded status — mirroring live_feed_test.go's pattern for
// LiveFeed but over the wire instead of a direct Handle call.
func TestControlSocketFeed_WireEventBecomesFrameMsg(t *testing.T) {
	srv, path := testControlServer(t, nil)
	client, err := control.Dial(path)
	if err != nil {
		t.Fatalf("control.Dial() error: %v", err)
	}
	f := NewControlSocketFeed(client)
	f.interval = 5 * time.Second // long enough that only the wakeup resolves Next
	defer f.Close()

	msgCh := runAsync(f.Next())

	// Give the readLoop a moment to be parked on the client's Frames channel.
	time.Sleep(20 * time.Millisecond)
	srv.Handle(progress.Event{Kind: progress.KindScanStarted, ScanKind: "sweep", Commit: "cafebabe"})

	select {
	case msg := <-msgCh:
		fr, ok := msg.(FrameMsg)
		if !ok {
			t.Fatalf("Next() resolved to %T, want FrameMsg", msg)
		}
		if fr.Snapshot.Commit != "cafebabe" || fr.Snapshot.ScanKind != "sweep" {
			t.Errorf("FrameMsg.Snapshot = %+v, want Commit=cafebabe ScanKind=sweep", fr.Snapshot)
		}
		if !fr.HasSnapshot || fr.Stale {
			t.Errorf("FrameMsg HasSnapshot=%v Stale=%v, want true/false", fr.HasSnapshot, fr.Stale)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Next() did not resolve after a wire event")
	}
}

// TestControlSocketFeed_CloseUnblocksPendingNext mirrors LiveFeed's same
// -named test: Close must unblock a Next() cmd parked waiting for a wakeup
// or the idle ticker, not leak the goroutine for the full interval.
func TestControlSocketFeed_CloseUnblocksPendingNext(t *testing.T) {
	_, path := testControlServer(t, nil)
	client, err := control.Dial(path)
	if err != nil {
		t.Fatalf("control.Dial() error: %v", err)
	}
	f := NewControlSocketFeed(client)
	f.interval = time.Hour

	msgCh := runAsync(f.Next())
	time.Sleep(10 * time.Millisecond)

	if err := f.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}

	select {
	case msg := <-msgCh:
		if msg != nil {
			t.Errorf("Next() after Close() = %v, want nil", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close() did not unblock a pending Next()")
	}
}

// TestTryAttach_ConnectsWhenSocketDialable and its sibling below exercise
// selectFeed's Attach branch contract directly via tryAttach, without
// needing a full daemon: dialable -> ok=true with a usable Feed/dispatcher;
// not dialable -> ok=false so the caller falls back to Observer.
func TestTryAttach_ConnectsWhenSocketDialable(t *testing.T) {
	_, path := testControlServer(t, func(_ context.Context, verb control.Verb, _ control.DispatchOpts) (control.DispatchSummary, error) {
		return control.DispatchSummary{}, nil
	})
	cfg := runTestConfig(t)
	cfg.Daemon.ControlSocket.Path = path

	feed, disp, cleanup, ok := tryAttach(context.Background(), cfg)
	if !ok {
		t.Fatal("tryAttach() ok = false, want true for a dialable socket")
	}
	defer cleanup()

	if feed == nil || feed.Mode() != Attach {
		t.Errorf("tryAttach() feed.Mode() = %v, want Attach", feed.Mode())
	}
	if disp == nil {
		t.Fatal("tryAttach() dispatcher = nil, want non-nil")
	}
}

func TestTryAttach_FalseWhenSocketNotDialable(t *testing.T) {
	cfg := runTestConfig(t)
	cfg.Daemon.ControlSocket.Path = filepath.Join(t.TempDir(), "nope.sock")

	_, _, _, ok := tryAttach(context.Background(), cfg)
	if ok {
		t.Fatal("tryAttach() ok = true for a nonexistent socket, want false")
	}
}

// TestSelectFeed_AttachWhenLockedAndSocketDialable is the end-to-end mode
// -selection check (acceptance criterion e): a store locked by another
// process (ErrLocked path) with a dialable control socket selects Attach,
// not Observer.
func TestSelectFeed_AttachWhenLockedAndSocketDialable(t *testing.T) {
	ctx := context.Background()
	cfg := runTestConfig(t)
	seedExistingStore(t, ctx, cfg)
	seedActiveForeignRun(t, ctx, cfg)

	_, path := testControlServer(t, nil)
	cfg.Daemon.ControlSocket.Path = path

	feed, disp, cleanup, err := selectFeed(ctx, cfg)
	if err != nil {
		t.Fatalf("selectFeed() error = %v", err)
	}
	defer cleanup()

	if _, ok := feed.(*ControlSocketFeed); !ok {
		t.Fatalf("selectFeed() feed = %T, want *ControlSocketFeed", feed)
	}
	if feed.Mode() != Attach {
		t.Errorf("feed.Mode() = %v, want Attach", feed.Mode())
	}
	if disp == nil {
		t.Fatal("selectFeed() dispatcher = nil, want non-nil in Attach mode")
	}
}

// TestSelectFeed_ObserverWhenLockedAndSocketNotDialable verifies the
// existing Observer fallback still applies when no control socket is
// listening — Attach is an upgrade, never a requirement.
func TestSelectFeed_ObserverWhenLockedAndSocketNotDialable(t *testing.T) {
	ctx := context.Background()
	cfg := runTestConfig(t)
	seedExistingStore(t, ctx, cfg)
	seedActiveForeignRun(t, ctx, cfg)
	feed, disp, cleanup, err := selectFeed(ctx, cfg)
	if err != nil {
		t.Fatalf("selectFeed() error = %v", err)
	}
	defer cleanup()

	if _, ok := feed.(*SnapshotFeed); !ok {
		t.Fatalf("selectFeed() feed = %T, want *SnapshotFeed", feed)
	}
	if disp != nil {
		t.Errorf("selectFeed() dispatcher = %v, want nil in Observer fallback", disp)
	}
}
