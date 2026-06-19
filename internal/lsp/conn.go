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
	"time"
)

// ErrConnDead reports that the transport to the language server is gone (the
// process exited or its pipes closed). The manager uses it to decide whether a
// failed query warrants a server restart.
var ErrConnDead = errors.New("language server connection is dead")

// conn is a JSON-RPC 2.0 connection speaking LSP base-protocol framing over a
// reader/writer pair (in practice a child process's stdout/stdin). It supports
// concurrent outgoing requests, answers server->client requests with minimal
// default responses (written off the read loop by a dedicated reply writer, so
// a peer that stops reading our stdin can never wedge frame draining), and
// discards server notifications (diagnostics, log messages) that read-only
// navigation does not need.
type conn struct {
	w      io.Writer
	wmu    sync.Mutex // serializes frame writes
	nextID atomic.Int64

	pmu     sync.Mutex
	pending map[string]chan *rpcMessage // key: raw JSON of the request ID

	// serverReplies hands replies to server->client requests from the read loop
	// to replyWriter. A stalled output pipe then blocks only that writer (and
	// fills this buffer, after which replies are dropped) instead of stalling
	// the read loop.
	serverReplies chan *rpcMessage

	dead chan struct{} // closed exactly once when the connection dies

	deadMu  sync.Mutex // guards deadErr
	deadErr error      // the terminal error; set before dead is closed
	once    sync.Once  // guards close(dead)

	// cause is closed once the authoritative death reason (process exit with a
	// stderr tail) is recorded, letting waiters prefer it over the bare
	// transport error the read loop may record first on the same crash.
	cause     chan struct{}
	causeOnce sync.Once // guards close(cause)
}

// newConn starts a connection over r/w and launches its read loop and reply
// writer. The caller owns the underlying transport's lifetime; when r reaches
// EOF (or errors) the conn marks itself dead and fails all in-flight calls.
func newConn(r io.Reader, w io.Writer) *conn {
	c := &conn{
		w:             w,
		pending:       make(map[string]chan *rpcMessage),
		serverReplies: make(chan *rpcMessage, 16),
		dead:          make(chan struct{}),
		cause:         make(chan struct{}),
	}
	go c.readLoop(bufio.NewReaderSize(r, 64*1024))
	go c.replyWriter()
	return c
}

// readLoop dispatches incoming frames until the transport fails.
func (c *conn) readLoop(br *bufio.Reader) {
	for {
		msg, err := readFrame(br)
		if err != nil {
			c.markDeadTransport(fmt.Errorf("%w: %v", ErrConnDead, err))
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
			// Reply off the read loop so a peer that has stopped reading our
			// stdin cannot wedge frame draining (replyWriter absorbs the stall).
			c.respondToServer(msg)
		default:
			// Notification (publishDiagnostics, logMessage, progress, ...):
			// irrelevant to read-only navigation; drop it.
		}
	}
}

// respondToServer builds the minimal response that keeps the server happy —
// workspace/configuration gets one null per requested item, a few known
// administrative requests get a null result, and anything else gets a
// MethodNotFound error (the protocol-correct reply for an unsupported method) —
// then hands it to replyWriter. It never writes inline, so a stalled output
// pipe can never block the read loop.
func (c *conn) respondToServer(req *rpcMessage) {
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
	// Hand the reply to replyWriter without blocking: when the output pipe is
	// stalled and the buffer is full, drop this best-effort admin reply rather
	// than stall the read loop.
	select {
	case c.serverReplies <- resp:
	case <-c.dead:
	default:
	}
}

// replyWriter serializes replies to server->client requests onto the output
// transport, off the read loop. A peer that has stopped reading our stdin
// blocks only this goroutine (respondToServer then fills serverReplies and
// drops once it is full), so frame draining continues. It exits when the
// connection dies. Sharing wmu with send keeps all writes serialized.
func (c *conn) replyWriter() {
	for {
		select {
		case <-c.dead:
			return
		case resp := <-c.serverReplies:
			c.wmu.Lock()
			_ = writeFrame(c.w, resp)
			c.wmu.Unlock()
		}
	}
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
		return c.terminalErr(ctx)
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
	err := writeFrame(c.w, msg)
	c.wmu.Unlock()
	if err != nil {
		select {
		case <-c.dead:
			return c.terminalErr(context.Background())
		default:
		}
		return fmt.Errorf("%w: write %s: %v", ErrConnDead, msg.Method, err)
	}
	return nil
}

// markDeadTransport records a transport read failure (EOF or a malformed frame)
// as the terminal error and releases every waiter. It defers to an
// authoritative cause: it sets deadErr only if nothing has been recorded yet,
// so a crash reason captured by markDeadCause is never overwritten. Called by
// the read loop.
func (c *conn) markDeadTransport(err error) {
	c.deadMu.Lock()
	if c.deadErr == nil {
		c.deadErr = err
	}
	c.deadMu.Unlock()
	c.once.Do(func() { close(c.dead) })
}

// markDeadCause records the authoritative reason the connection died — the
// process exit status plus its stderr tail — as the terminal error, superseding
// any bare transport error the read loop recorded first on the same crash. It
// releases every waiter and signals (via cause) that the reason has landed.
// Called by the process-wait goroutine.
func (c *conn) markDeadCause(err error) {
	c.deadMu.Lock()
	c.deadErr = err
	c.deadMu.Unlock()
	c.once.Do(func() { close(c.dead) })
	c.causeOnce.Do(func() { close(c.cause) })
}

// causeGrace bounds how long terminalErr waits for the authoritative crash
// cause to supersede a bare transport error after the read loop observes EOF.
// On a real process death the cmd.Wait goroutine records the cause within
// microseconds of the pipe EOF, so this bound is only reached when no cause is
// coming (e.g. a non-process transport closed its end).
const causeGrace = 250 * time.Millisecond

// terminalErr returns the connection's terminal error, preferring the
// authoritative crash cause over a bare transport error when a crash races the
// two signals. Callers invoke it only after observing c.dead closed, so deadErr
// is already set. When the cause has not yet landed it waits a bounded grace
// (or until the cause arrives, or ctx is done) so the crash reason wins
// deterministically instead of depending on which signal fired first.
func (c *conn) terminalErr(ctx context.Context) error {
	select {
	case <-c.cause:
		// The authoritative cause is already recorded.
		c.deadMu.Lock()
		defer c.deadMu.Unlock()
		return c.deadErr
	default:
	}
	c.deadMu.Lock()
	err := c.deadErr
	c.deadMu.Unlock()
	// deadErr is the transport error and no cause has landed yet; give the
	// cause a bounded grace to arrive so a crash reason supersedes a bare EOF.
	select {
	case <-c.cause:
		c.deadMu.Lock()
		err = c.deadErr
		c.deadMu.Unlock()
	case <-time.After(causeGrace):
	case <-ctx.Done():
	}
	return err
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
