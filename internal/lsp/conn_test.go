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
