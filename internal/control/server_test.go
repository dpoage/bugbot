package control

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/progress"
)

func testServer(t *testing.T, dispatch DispatchFunc) (*Server, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "daemon.sock")
	srv, err := Listen(path, dispatch, nil)
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		_ = srv.Close()
	})
	return srv, path
}

// TestServer_FansOutEventsToRealClient verifies (a): a Handle call on the
// server reaches a real unix socket client (dialed in t.TempDir) as an
// event frame followed by a status frame.
func TestServer_FansOutEventsToRealClient(t *testing.T) {
	srv, path := testServer(t, nil)

	cl, err := Dial(path)
	if err != nil {
		t.Fatalf("Dial() error: %v", err)
	}
	defer cl.Close()

	// Give the server a moment to register the accepted connection before
	// broadcasting, otherwise Handle may race the Accept goroutine.
	time.Sleep(20 * time.Millisecond)

	srv.Handle(progress.Event{Kind: progress.KindScanStarted, ScanKind: "sweep", Commit: "deadbeef"})

	var gotEvent, gotStatus bool
	deadline := time.After(2 * time.Second)
	for !gotEvent || !gotStatus {
		select {
		case fr := <-cl.Frames():
			switch fr.Kind {
			case FrameKindEvent:
				if fr.Event == nil || fr.Event.Kind != progress.KindScanStarted || fr.Event.Commit != "deadbeef" {
					t.Fatalf("unexpected event frame: %+v", fr.Event)
				}
				gotEvent = true
			case FrameKindStatus:
				if fr.Status == nil || fr.Status.Commit != "deadbeef" {
					t.Fatalf("unexpected status frame: %+v", fr.Status)
				}
				gotStatus = true
			}
		case <-deadline:
			t.Fatalf("timed out waiting for event=%v status=%v", gotEvent, gotStatus)
		}
	}
}

// TestServer_StalledClientNeverBlocksDeliveryOrOtherClients verifies (b): a
// client that never reads its socket does not block Handle (which must
// return promptly regardless of client backpressure) nor delivery to a
// second, healthy client.
func TestServer_StalledClientNeverBlocksDeliveryOrOtherClients(t *testing.T) {
	srv, path := testServer(t, nil)

	stalled, err := Dial(path)
	if err != nil {
		t.Fatalf("Dial() stalled client error: %v", err)
	}
	defer stalled.Close()
	// Deliberately never drain stalled.Frames().

	healthy, err := Dial(path)
	if err != nil {
		t.Fatalf("Dial() healthy client error: %v", err)
	}
	defer healthy.Close()

	time.Sleep(20 * time.Millisecond)

	// Flood far past clientOutboxSize so the stalled client's outbox fills
	// and starts dropping. Each Handle call must still return promptly.
	const n = clientOutboxSize * 4
	start := time.Now()
	for i := 0; i < n; i++ {
		srv.Handle(progress.Event{Kind: progress.KindSpendTick, InputTokens: int64(i)})
	}
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("Handle blocked: %d events took %s (stalled client should never slow delivery)", n, elapsed)
	}

	// The healthy client must still see at least one frame promptly.
	select {
	case fr := <-healthy.Frames():
		if fr.Kind != FrameKindEvent && fr.Kind != FrameKindStatus {
			t.Fatalf("unexpected frame kind on healthy client: %v", fr.Kind)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("healthy client received nothing despite stalled sibling")
	}
}

// TestServer_DispatchRPCRoundTrip verifies (c): a dispatch RPC reaches the
// fake DispatchFunc with the sent verb/opts, and the reply correlates by ID
// back to the calling Client.Dispatch.
func TestServer_DispatchRPCRoundTrip(t *testing.T) {
	var mu sync.Mutex
	var gotVerb Verb
	var gotOpts DispatchOpts

	dispatch := func(_ context.Context, verb Verb, opts DispatchOpts) (DispatchSummary, error) {
		mu.Lock()
		gotVerb, gotOpts = verb, opts
		mu.Unlock()
		return DispatchSummary{FindingCount: 3, HasResult: true}, nil
	}

	_, path := testServer(t, dispatch)

	cl, err := Dial(path)
	if err != nil {
		t.Fatalf("Dial() error: %v", err)
	}
	defer cl.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	sum, err := cl.Dispatch(ctx, VerbScan, DispatchOpts{Target: "./repro-target", Since: "HEAD~3"})
	if err != nil {
		t.Fatalf("Dispatch() error: %v", err)
	}
	if sum.FindingCount != 3 || !sum.HasResult {
		t.Errorf("Dispatch() summary = %+v, want FindingCount=3 HasResult=true", sum)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotVerb != VerbScan {
		t.Errorf("dispatch received verb %q, want %q", gotVerb, VerbScan)
	}
	if gotOpts.Target != "./repro-target" || gotOpts.Since != "HEAD~3" {
		t.Errorf("dispatch received opts %+v, want Target=./repro-target Since=HEAD~3", gotOpts)
	}
}

// TestServer_DispatchRPCError verifies an erroring DispatchFunc surfaces as
// a non-OK reply the client turns into an error, still correlated by ID.
func TestServer_DispatchRPCError(t *testing.T) {
	dispatch := func(_ context.Context, verb Verb, opts DispatchOpts) (DispatchSummary, error) {
		return DispatchSummary{}, errors.New("boom")
	}
	_, path := testServer(t, dispatch)

	cl, err := Dial(path)
	if err != nil {
		t.Fatalf("Dial() error: %v", err)
	}
	defer cl.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err = cl.Dispatch(ctx, VerbSweep, DispatchOpts{})
	if err == nil || err.Error() != "boom" {
		t.Fatalf("Dispatch() error = %v, want \"boom\"", err)
	}
}

// TestServer_DispatchDisabledByDefault verifies a nil DispatchFunc (control
// socket enabled but no dispatch executor wired) replies with an error
// rather than hanging.
func TestServer_DispatchDisabledByDefault(t *testing.T) {
	_, path := testServer(t, nil)

	cl, err := Dial(path)
	if err != nil {
		t.Fatalf("Dial() error: %v", err)
	}
	defer cl.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err = cl.Dispatch(ctx, VerbScan, DispatchOpts{})
	if err == nil {
		t.Fatal("Dispatch() error = nil, want dispatch-disabled error")
	}
}

// TestDialable verifies Dialable reports true for a live socket and false
// for a nonexistent path.
func TestDialable(t *testing.T) {
	_, path := testServer(t, nil)
	if !Dialable(path) {
		t.Error("Dialable() = false for a live socket, want true")
	}
	if Dialable(filepath.Join(t.TempDir(), "nope.sock")) {
		t.Error("Dialable() = true for a nonexistent socket, want false")
	}
}
