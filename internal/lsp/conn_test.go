package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// pipeServer is an in-process scripted peer for conn tests: the test plays the
// language server over io.Pipes.
type pipeServer struct {
	r *bufio.Reader // frames the client sent us
	w io.Writer     // frames we send to the client
}

func newPipeConn(t *testing.T) (*conn, *pipeServer) {
	t.Helper()
	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()
	t.Cleanup(func() {
		_ = serverW.Close()
		_ = clientW.Close()
	})
	c := newConn(clientR, clientW)
	return c, &pipeServer{r: bufio.NewReader(serverR), w: serverW}
}

func (p *pipeServer) read(t *testing.T) *rpcMessage {
	t.Helper()
	msg, err := readFrame(p.r)
	if err != nil {
		t.Fatalf("server readFrame: %v", err)
	}
	return msg
}

func (p *pipeServer) write(t *testing.T, msg *rpcMessage) {
	t.Helper()
	if err := writeFrame(p.w, msg); err != nil {
		t.Fatalf("server writeFrame: %v", err)
	}
}

func TestConnCallRoundTrip(t *testing.T) {
	c, srv := newPipeConn(t)

	go func() {
		req := srv.read(t)
		if req.Method != "test/echo" {
			t.Errorf("got method %q", req.Method)
		}
		srv.write(t, &rpcMessage{ID: req.ID, Result: json.RawMessage(`{"ok":true}`)})
	}()

	var result struct {
		OK bool `json:"ok"`
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.call(ctx, "test/echo", map[string]any{"x": 1}, &result); err != nil {
		t.Fatalf("call: %v", err)
	}
	if !result.OK {
		t.Error("result not decoded")
	}
}

func TestConnCallServerError(t *testing.T) {
	c, srv := newPipeConn(t)

	go func() {
		req := srv.read(t)
		srv.write(t, &rpcMessage{ID: req.ID, Error: &rpcError{Code: -32600, Message: "nope"}})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := c.call(ctx, "test/fail", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("expected server error, got %v", err)
	}
}

func TestConnAnswersServerRequests(t *testing.T) {
	c, srv := newPipeConn(t)
	_ = c

	// workspace/configuration must get one null per item.
	srv.write(t, &rpcMessage{
		ID:     json.RawMessage("100"),
		Method: "workspace/configuration",
		Params: json.RawMessage(`{"items":[{"section":"gopls"},{"section":"other"}]}`),
	})
	resp := srv.read(t)
	if string(resp.ID) != "100" {
		t.Fatalf("response id = %s", resp.ID)
	}
	var nulls []json.RawMessage
	if err := json.Unmarshal(resp.Result, &nulls); err != nil || len(nulls) != 2 {
		t.Fatalf("workspace/configuration result = %s (err %v), want 2 nulls", resp.Result, err)
	}

	// An unknown method gets a MethodNotFound error, not silence.
	srv.write(t, &rpcMessage{ID: json.RawMessage(`"abc"`), Method: "window/fancyNewThing"})
	resp = srv.read(t)
	if string(resp.ID) != `"abc"` || resp.Error == nil || resp.Error.Code != methodNotFound {
		t.Fatalf("unknown method response = %+v", resp)
	}

	// Known administrative requests get null results.
	srv.write(t, &rpcMessage{ID: json.RawMessage("101"), Method: "client/registerCapability", Params: json.RawMessage(`{}`)})
	resp = srv.read(t)
	if resp.Error != nil || string(resp.Result) != "null" {
		t.Fatalf("registerCapability response = %+v", resp)
	}
}

func TestConnConcurrentCalls(t *testing.T) {
	c, srv := newPipeConn(t)

	// The scripted server answers in deliberately shuffled order (pairs
	// reversed), echoing each request's payload so every caller can verify it
	// received its own response. Under -race this exercises the pending-map,
	// ID-allocation, and write-mutex interleavings concurrent runners create.
	const goroutines, callsEach = 16, 4
	go func() {
		total := goroutines * callsEach
		for served := 0; served < total; served += 2 {
			a, err := readFrame(srv.r)
			if err != nil {
				return
			}
			b, err := readFrame(srv.r)
			if err != nil {
				return
			}
			for _, req := range []*rpcMessage{b, a} {
				var p struct {
					X int `json:"x"`
				}
				_ = json.Unmarshal(req.Params, &p)
				result := json.RawMessage(fmt.Sprintf(`{"x":%d}`, p.X))
				if err := writeFrame(srv.w, &rpcMessage{ID: req.ID, Result: result}); err != nil {
					return
				}
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, goroutines*callsEach)
	for g := range goroutines {
		wg.Go(func() {
			for i := range callsEach {
				x := g*callsEach + i
				var result struct {
					X int `json:"x"`
				}
				if err := c.call(ctx, "test/echo", map[string]any{"x": x}, &result); err != nil {
					errs <- err
					return
				}
				if result.X != x {
					errs <- fmt.Errorf("call %d got response for %d (misrouted)", x, result.X)
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestConnDeadFailsConcurrentCalls(t *testing.T) {
	clientR, serverW := io.Pipe()
	drainR, clientW := io.Pipe()
	go func() { _, _ = io.Copy(io.Discard, drainR) }()
	c := newConn(clientR, clientW)
	t.Cleanup(func() { _ = clientW.Close() })

	// Many calls in flight against a server that never answers; killing the
	// transport must fail every one of them with ErrConnDead, racing markDead
	// against concurrent senders.
	const callers = 16
	started := make(chan struct{}, callers)
	errs := make(chan error, callers)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for range callers {
		go func() {
			started <- struct{}{}
			errs <- c.call(ctx, "test/never", nil, nil)
		}()
	}
	for range callers {
		<-started
	}
	_ = serverW.Close() // transport death

	for range callers {
		err := <-errs
		if err == nil || !errors.Is(err, ErrConnDead) {
			t.Errorf("expected ErrConnDead, got %v", err)
		}
	}
}

func TestConnDeadFailsPendingCalls(t *testing.T) {
	clientR, serverW := io.Pipe()
	drainR, clientW := io.Pipe()
	// io.Pipe writes block until read; drain the client's outgoing frames so
	// the call under test can proceed to waiting for its response.
	go func() { _, _ = io.Copy(io.Discard, drainR) }()
	c := newConn(clientR, clientW)

	errCh := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		errCh <- c.call(ctx, "test/hang", nil, nil)
	}()

	// Give the call a moment to register, then kill the transport.
	time.Sleep(50 * time.Millisecond)
	_ = serverW.Close()

	select {
	case err := <-errCh:
		if !errors.Is(err, ErrConnDead) {
			t.Fatalf("expected ErrConnDead, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pending call did not fail after transport death")
	}

	// Subsequent calls fail fast too.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.call(ctx, "test/after", nil, nil); !errors.Is(err, ErrConnDead) {
		t.Fatalf("post-death call: expected ErrConnDead, got %v", err)
	}
}

func TestConnNotificationsAreDropped(t *testing.T) {
	c, srv := newPipeConn(t)

	// A notification from the server must not disturb a pending call.
	go func() {
		req := srv.read(t)
		srv.write(t, &rpcMessage{Method: "textDocument/publishDiagnostics", Params: json.RawMessage(`{}`)})
		srv.write(t, &rpcMessage{ID: req.ID, Result: json.RawMessage(`42`)})
	}()

	var n int
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.call(ctx, "test/n", nil, &n); err != nil {
		t.Fatalf("call: %v", err)
	}
	if n != 42 {
		t.Errorf("n = %d", n)
	}
}

// TestConnReplyWriteDoesNotStallReadLoop verifies FIX 1: a server that stops
// reading our stdin while still sending us server->client requests must not
// wedge the read loop. The reply write-backs block on the stalled output pipe,
// but readLoop keeps draining incoming frames (replies are written off the read
// loop), so an in-flight call's response is still delivered. A regression that
// reintroduces an inline reply write under wmu in readLoop hangs here, caught
// by the per-step timeouts.
func TestConnReplyWriteDoesNotStallReadLoop(t *testing.T) {
	// r: server->client frames (we script the server side).
	clientR, serverW := io.Pipe()
	// w: client->server frames. The fake server reads exactly the first client
	// frame (our in-flight call's request) and then stops reading its stdin.
	// io.Pipe is unbuffered, so every later client write — the server-reply
	// write-backs — blocks indefinitely, modeling a peer wedged on stdin.
	serverR, clientW := io.Pipe()
	c := newConn(clientR, clientW)

	stop := make(chan struct{})
	t.Cleanup(func() {
		close(stop)         // unpark the fake server goroutine
		_ = serverR.Close() // unblock replyWriter's stalled write
		_ = serverW.Close() // EOF the read loop
	})

	gotCallReq := make(chan json.RawMessage, 1)
	go func() {
		br := bufio.NewReader(serverR)
		req, err := readFrame(br)
		if err != nil {
			return
		}
		gotCallReq <- req.ID
		<-stop // stop reading our stdin: every later client write blocks forever
	}()

	// Issue an in-flight call; its request write must get through before the
	// peer wedges.
	callErr := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() {
		var n int
		callErr <- c.call(ctx, "in/flight", nil, &n)
	}()

	var callID json.RawMessage
	select {
	case callID = <-gotCallReq:
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight call request never reached the server")
	}

	// Flood the client with server->client requests it must answer. Each reply
	// write-back blocks on the wedged output pipe; with FIX 1 the read loop
	// keeps draining (serverReplies fills, then respondToServer drops), so all
	// frames flow through. The flood (50) far exceeds the reply buffer (16),
	// proving draining continues past buffer saturation.
	const flood = 50
	floodDone := make(chan struct{})
	go func() {
		for i := 0; i < flood; i++ {
			req := &rpcMessage{
				ID:     json.RawMessage(fmt.Sprintf("%d", 2000+i)),
				Method: "window/workDoneProgress/create",
			}
			if err := writeFrame(serverW, req); err != nil {
				return
			}
		}
		// Finally, deliver the in-flight call's response behind the flood.
		_ = writeFrame(serverW, &rpcMessage{ID: callID, Result: json.RawMessage(`7`)})
		close(floodDone)
	}()

	select {
	case <-floodDone:
	case <-time.After(5 * time.Second):
		t.Fatal("read loop wedged: server->client request flood not drained while the reply writer was blocked")
	}

	select {
	case err := <-callErr:
		if err != nil {
			t.Fatalf("in-flight call failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight call response not delivered: read loop wedged behind a blocked reply write")
	}
}

// newDrainedConn builds a conn whose outgoing frames are drained (so calls can
// send successfully) and whose incoming side never reaches EOF on its own, so a
// test drives connection death explicitly via markDeadTransport/markDeadCause.
func newDrainedConn(t *testing.T) *conn {
	t.Helper()
	clientR, serverW := io.Pipe() // server->client; never written, so readLoop blocks (no spontaneous EOF)
	drainR, clientW := io.Pipe()  // client->server; drained so sends succeed
	go func() { _, _ = io.Copy(io.Discard, drainR) }()
	c := newConn(clientR, clientW)
	t.Cleanup(func() {
		_ = serverW.Close()
		_ = clientW.Close()
	})
	return c
}

// waitPending blocks until at least one outgoing call has registered in c's
// pending table, so a test can kill the transport with a call genuinely blocked
// on it rather than relying on a fixed sleep.
func waitPending(t *testing.T, c *conn) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.pmu.Lock()
		n := len(c.pending)
		c.pmu.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("call never registered in pending table")
}

// TestConnCrashCauseWinsOverTransportEOF verifies FIX 2: when a server crash
// races a bare transport EOF (markDeadTransport, as the read loop records) and
// the authoritative cause carrying the stderr tail (markDeadCause, as the
// cmd.Wait goroutine records), callers deterministically observe the cause —
// regardless of which signal fired first.
func TestConnCrashCauseWinsOverTransportEOF(t *testing.T) {
	const tail = "boom: language server panicked"
	cause := fmt.Errorf("%w: fakelsp exited: exit status 3 (stderr: %s)", ErrConnDead, tail)
	transport := fmt.Errorf("%w: %v", ErrConnDead, io.EOF)

	assertCause := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("call returned nil; want the crash cause")
		}
		if !errors.Is(err, ErrConnDead) {
			t.Errorf("error must wrap ErrConnDead: %v", err)
		}
		if !strings.Contains(err.Error(), tail) {
			t.Fatalf("want error containing stderr tail %q, got %v", tail, err)
		}
	}

	// Production ordering: pipe EOF observed first, cmd.Wait cause second. The
	// bounded grace in terminalErr lets the cause supersede the bare EOF.
	t.Run("transport then cause", func(t *testing.T) {
		c := newDrainedConn(t)
		errCh := make(chan error, 1)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			errCh <- c.call(ctx, "test/blocked", nil, nil)
		}()
		waitPending(t, c)
		c.markDeadTransport(transport)
		c.markDeadCause(cause)
		select {
		case err := <-errCh:
			assertCause(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("blocked call did not return after connection death")
		}
	})

	// Reverse ordering: the cause is recorded first; a late transport EOF must
	// not clobber it.
	t.Run("cause then transport", func(t *testing.T) {
		c := newDrainedConn(t)
		errCh := make(chan error, 1)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			errCh <- c.call(ctx, "test/blocked", nil, nil)
		}()
		waitPending(t, c)
		c.markDeadCause(cause)
		c.markDeadTransport(transport)
		select {
		case err := <-errCh:
			assertCause(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("blocked call did not return after connection death")
		}
	})
}
