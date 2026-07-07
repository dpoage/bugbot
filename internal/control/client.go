package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// dialTimeout bounds how long Dial waits for the Unix socket to accept a
// connection — long enough to tolerate a daemon mid-startup, short enough
// that mode selection (tui.Run deciding Attach vs Observer) never hangs the
// TUI's launch.
const dialTimeout = 2 * time.Second

// Client is the tui-side connection to a daemon's control socket: it decodes
// inbound Frames onto Frames() and lets callers issue dispatch RPCs via
// Dispatch. Safe for concurrent use.
type Client struct {
	conn net.Conn

	frames chan Frame

	mu      sync.Mutex
	pending map[string]chan DispatchReply

	nextID atomic.Uint64

	closeOnce sync.Once
	closed    chan struct{}
}

// Dial connects to the control socket at path. It performs no protocol
// handshake beyond the connection itself (frames are self-describing via
// their "v" field) — a caller that needs to confirm the daemon is actually
// speaking this protocol should watch for its first Frame.
func Dial(path string) (*Client, error) {
	conn, err := net.DialTimeout("unix", path, dialTimeout)
	if err != nil {
		return nil, err
	}
	c := &Client{
		conn:    conn,
		frames:  make(chan Frame, clientOutboxSize),
		pending: make(map[string]chan DispatchReply),
		closed:  make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

// Dialable reports whether a control socket at path currently accepts
// connections, without keeping the connection open. Used by tui's mode
// selection to decide between Attach and Observer without committing to a
// full Dial/Client lifecycle when nothing is listening.
func Dialable(path string) bool {
	conn, err := net.DialTimeout("unix", path, dialTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// Frames returns the channel of inbound server->client Frames (event/status
// broadcasts; dispatch replies are delivered via Dispatch instead and never
// appear here). Closed when the connection closes.
func (c *Client) Frames() <-chan Frame { return c.frames }

// Dispatch sends a Request for verb/opts and blocks until the matching
// Reply arrives, ctx is cancelled, or the connection closes. Concurrent
// calls are safe; each gets its own correlation ID.
func (c *Client) Dispatch(ctx context.Context, verb Verb, opts DispatchOpts) (DispatchSummary, error) {
	id := strconv.FormatUint(c.nextID.Add(1), 10)
	replyCh := make(chan DispatchReply, 1)

	c.mu.Lock()
	c.pending[id] = replyCh
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	req := Request{V: ProtocolVersion, ID: id, Verb: verb, Opts: opts}
	enc := json.NewEncoder(c.conn)
	if err := enc.Encode(req); err != nil {
		return DispatchSummary{}, fmt.Errorf("control: send request: %w", err)
	}

	select {
	case rep := <-replyCh:
		if !rep.OK {
			return DispatchSummary{}, errors.New(rep.Error)
		}
		if rep.Summary == nil {
			return DispatchSummary{}, nil
		}
		return *rep.Summary, nil
	case <-ctx.Done():
		return DispatchSummary{}, ctx.Err()
	case <-c.closed:
		return DispatchSummary{}, errors.New("control: connection closed")
	}
}

// Close closes the underlying connection and unblocks Frames()/any pending
// Dispatch calls.
func (c *Client) Close() error {
	c.closeConn()
	return nil
}

// closeConn is the shared, idempotent teardown both Close (caller-initiated)
// and readLoop (connection-died-on-its-own, e.g. the daemon exited) drive
// through: it closes c.closed exactly once, which unblocks every pending
// Dispatch call's select (each already races <-c.closed against its reply
// channel) with a "connection closed" error, and closes the socket. Without
// this shared path, a naturally-dead connection (daemon crash/exit) would
// leave any in-flight Dispatch call blocked forever — the palette would show
// "running..." indefinitely instead of surfacing an error.
func (c *Client) closeConn() {
	c.closeOnce.Do(func() {
		close(c.closed)
		_ = c.conn.Close()
	})
}

func (c *Client) readLoop() {
	defer close(c.frames)
	defer c.closeConn()
	dec := json.NewDecoder(c.conn)
	for {
		var fr Frame
		if err := dec.Decode(&fr); err != nil {
			return
		}
		if fr.V != ProtocolVersion {
			// Refuse to misparse a future incompatible frame shape; simply
			// skip it rather than tearing down the whole connection over one
			// unrecognized frame.
			continue
		}
		if fr.Kind == FrameKindReply && fr.Reply != nil {
			c.mu.Lock()
			ch := c.pending[fr.Reply.ID]
			c.mu.Unlock()
			if ch != nil {
				ch <- *fr.Reply
			}
			continue
		}
		select {
		case c.frames <- fr:
		case <-c.closed:
			return
		}
	}
}
