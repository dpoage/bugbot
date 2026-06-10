package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
)

// ErrConnDead reports that the transport to the language server is gone (the
// process exited or its pipes closed). The manager uses it to decide whether a
// failed query warrants a server restart.
var ErrConnDead = errors.New("language server connection is dead")

// conn is a JSON-RPC 2.0 connection speaking LSP base-protocol framing over a
// reader/writer pair (in practice a child process's stdout/stdin). It supports
// concurrent outgoing requests, answers server->client requests with minimal
// default responses so the server never stalls waiting on us, and discards
// server notifications (diagnostics, log messages) that read-only navigation
// does not need.
type conn struct {
	w      io.Writer
	wmu    sync.Mutex // serializes frame writes
	nextID atomic.Int64

	pmu     sync.Mutex
	pending map[string]chan *rpcMessage // key: raw JSON of the request ID

	dead    chan struct{} // closed when the read loop exits
	deadErr error         // set before dead is closed
	once    sync.Once
}

// newConn starts a connection over r/w and launches its read loop. The caller
// owns the underlying transport's lifetime; when r reaches EOF (or errors) the
// conn marks itself dead and fails all in-flight calls.
func newConn(r io.Reader, w io.Writer) *conn {
	c := &conn{
		w:       w,
		pending: make(map[string]chan *rpcMessage),
		dead:    make(chan struct{}),
	}
	go c.readLoop(bufio.NewReaderSize(r, 64*1024))
	return c
}

// readLoop dispatches incoming frames until the transport fails.
func (c *conn) readLoop(br *bufio.Reader) {
	for {
		msg, err := readFrame(br)
		if err != nil {
			c.markDead(fmt.Errorf("%w: %v", ErrConnDead, err))
			return
		}
		switch {
		case msg.isResponse():
			c.pmu.Lock()
			ch := c.pending[string(msg.ID)]
			delete(c.pending, string(msg.ID))
			c.pmu.Unlock()
			if ch != nil {
				ch <- msg
			}
		case msg.isRequest():
			// Reply immediately so the server never blocks on us. Write errors
			// here surface naturally on the next outgoing call.
			_ = c.respondToServer(msg)
		default:
			// Notification (publishDiagnostics, logMessage, progress, ...):
			// irrelevant to read-only navigation; drop it.
		}
	}
}

// respondToServer answers a server->client request with the minimal response
// that keeps the server happy: workspace/configuration gets one null per
// requested item, a few known administrative requests get a null result, and
// anything else gets a MethodNotFound error (the protocol-correct reply for an
// unsupported method).
func (c *conn) respondToServer(req *rpcMessage) error {
	resp := &rpcMessage{ID: req.ID}
	switch req.Method {
	case "workspace/configuration":
		var params struct {
			Items []json.RawMessage `json:"items"`
		}
		_ = json.Unmarshal(req.Params, &params)
		nulls := make([]json.RawMessage, len(params.Items))
		for i := range nulls {
			nulls[i] = json.RawMessage("null")
		}
		out, _ := json.Marshal(nulls)
		resp.Result = out
	case "client/registerCapability", "client/unregisterCapability",
		"window/workDoneProgress/create", "window/showMessageRequest":
		resp.Result = json.RawMessage("null")
	case "workspace/applyEdit":
		resp.Result = json.RawMessage(`{"applied":false}`)
	default:
		resp.Error = &rpcError{Code: methodNotFound, Message: "method not supported by bugbot client"}
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return writeFrame(c.w, resp)
}

// call sends a request and waits for its response, honoring ctx cancellation
// and connection death. result, when non-nil, receives the unmarshaled result.
func (c *conn) call(ctx context.Context, method string, params, result any) error {
	id := c.nextID.Add(1)
	idRaw := json.RawMessage(strconv.FormatInt(id, 10))

	ch := make(chan *rpcMessage, 1)
	c.pmu.Lock()
	c.pending[string(idRaw)] = ch
	c.pmu.Unlock()
	defer func() {
		c.pmu.Lock()
		delete(c.pending, string(idRaw))
		c.pmu.Unlock()
	}()

	if err := c.send(&rpcMessage{ID: idRaw, Method: method}, params); err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.dead:
		return c.deadErr
	case resp := <-ch:
		if resp.Error != nil {
			return fmt.Errorf("lsp: %s: %w", method, resp.Error)
		}
		if result != nil && len(resp.Result) > 0 && string(resp.Result) != "null" {
			if err := json.Unmarshal(resp.Result, result); err != nil {
				return fmt.Errorf("lsp: %s: decode result: %w", method, err)
			}
		}
		return nil
	}
}

// callRaw is call but returns the raw result for union-typed responses.
func (c *conn) callRaw(ctx context.Context, method string, params any) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.call(ctx, method, params, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// notify sends a notification (no response expected).
func (c *conn) notify(method string, params any) error {
	return c.send(&rpcMessage{Method: method}, params)
}

// send marshals params into msg and writes the frame, mapping write failures on
// a dead transport to ErrConnDead.
func (c *conn) send(msg *rpcMessage, params any) error {
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("lsp: marshal %s params: %w", msg.Method, err)
		}
		msg.Params = raw
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := writeFrame(c.w, msg); err != nil {
		select {
		case <-c.dead:
			return c.deadErr
		default:
		}
		return fmt.Errorf("%w: write %s: %v", ErrConnDead, msg.Method, err)
	}
	return nil
}

// markDead records the terminal error and releases every waiter exactly once.
func (c *conn) markDead(err error) {
	c.once.Do(func() {
		c.deadErr = err
		close(c.dead)
	})
}

// isDead reports whether the transport has failed.
func (c *conn) isDead() bool {
	select {
	case <-c.dead:
		return true
	default:
		return false
	}
}
