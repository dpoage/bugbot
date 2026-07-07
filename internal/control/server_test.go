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

// waitForCond polls cond until true or a bounded deadline, failing the test
// on timeout. Used instead of a single fixed sleep so these regression tests
// assert on observable server state, not on timing.
func waitForCond(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !cond() {
		t.Fatal("condition never became true within 2s")
	}
}

// TestServer_ClientDisconnectDoesNotLeakConnection is a regression test for
// a goroutine/entry leak: clientConn.run's wg.Wait() used to block forever
// after the read loop broke on disconnect, because writeLoop only exits on
// c.done (closed by the deferred c.close(), which ran AFTER wg.Wait() —
// a deadlock cycle) or a write error. That left the client stuck in
// s.clients and its reader/writer goroutines alive until the next broadcast
// happened to hit a write error. Asserts the server's client registry
// empties out promptly (bounded poll, not a blind sleep) after the client
// closes its end.
func TestServer_ClientDisconnectDoesNotLeakConnection(t *testing.T) {
	srv, path := testServer(t, nil)

	cl, err := Dial(path)
	if err != nil {
		t.Fatalf("Dial() error: %v", err)
	}

	waitForCond(t, func() bool {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		return len(srv.clients) == 1
	})

	if err := cl.Close(); err != nil {
		t.Fatalf("Client.Close() error: %v", err)
	}

	waitForCond(t, func() bool {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		return len(srv.clients) == 0
	})
}

// TestClient_DispatchFailsPromptlyWhenServerDies is a regression test for a
// client-side hang: readLoop used to only close c.frames on a decode error,
// never c.closed, so an in-flight Dispatch (whose select races the reply
// channel against c.closed) would block until the CALLER's ctx expired —
// even though the connection was already dead. Uses a ctx with a long
// deadline (2 minutes) so a pass can only mean closeConn's teardown path
// unblocked Dispatch, not the ctx timing out on its own.
func TestClient_DispatchFailsPromptlyWhenServerDies(t *testing.T) {
	blockDispatch := make(chan struct{}) // never closed: the verb never replies
	dispatch := func(ctx context.Context, verb Verb, opts DispatchOpts) (DispatchSummary, error) {
		<-blockDispatch
		return DispatchSummary{}, nil
	}
	srv, path := testServer(t, dispatch)

	cl, err := Dial(path)
	if err != nil {
		t.Fatalf("Dial() error: %v", err)
	}
	defer cl.Close()

	result := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_, dErr := cl.Dispatch(ctx, VerbScan, DispatchOpts{})
		result <- dErr
	}()

	// Give the request a moment to reach the server and start blocking in
	// the fake dispatch func, then kill the server out from under the
	// client — simulating a daemon crash/exit.
	time.Sleep(50 * time.Millisecond)
	if err := srv.Close(); err != nil {
		t.Fatalf("Server.Close() error: %v", err)
	}

	select {
	case err := <-result:
		if err == nil {
			t.Error("Dispatch() error = nil after server died, want an error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Dispatch() did not return promptly after the server died (client-side hang)")
	}
	close(blockDispatch)
}
