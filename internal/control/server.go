package control

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"sync"

	"github.com/dpoage/bugbot/internal/progress"
)

// clientOutboxSize bounds each connected client's outbound frame buffer.
// Sized generously enough that a healthy client (draining at terminal/network
// speed) never drops a frame in practice, while a stalled one fills it and
// starts dropping within a fraction of a second of event volume — never
// blocking the sender (the daemon's scheduler goroutine, via Handle).
const clientOutboxSize = 256

// DispatchFunc executes one dispatch verb to completion and returns its
// reduced summary (or an error). The daemon supplies the real implementation
// (see internal/daemon.Daemon.SubmitDispatch); it MUST NOT be called
// directly by anything on the daemon's own scheduler goroutine — Server
// calls it from a per-request goroutine, and the daemon-side implementation
// is responsible for serializing execution against its own cycle loop.
type DispatchFunc func(ctx context.Context, verb Verb, opts DispatchOpts) (DispatchSummary, error)

// Server is the daemon-side control socket: a Unix listener that fans out
// progress events (and periodic status snapshots) to every connected client
// and accepts dispatch RPCs, per the package doc's wire protocol.
//
// Server implements progress.EventSink so it can be registered directly in
// the daemon's progress.Multi fanout alongside the slog renderer and
// SnapshotSink — Handle folds each event into Server's own
// progress.StatusAccumulator (so a freshly-connected client gets a coherent
// status.json-equivalent snapshot without needing store access) and
// broadcasts both the raw event and the refreshed status to every client.
type Server struct {
	path string
	ln   net.Listener
	log  *slog.Logger

	dispatch DispatchFunc

	acc *progress.StatusAccumulator

	mu      sync.Mutex
	clients map[*clientConn]struct{}
	closed  bool
}

var _ progress.EventSink = (*Server)(nil)

// Listen creates the Unix socket at path (unlinking a stale one left behind
// by a crashed prior daemon) and returns a Server ready for Serve. Perms are
// set to 0600 (owner read/write only) after listening — a bare net.Listen on
// a unix socket path otherwise inherits the process umask.
func Listen(path string, dispatch DispatchFunc, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	// Best-effort: remove a stale socket file from a prior (crashed) daemon.
	// A live daemon still listening on it would make the subsequent Listen
	// fail with "address already in use", which is the correct outcome —
	// we must never silently steal a socket a live process still owns.
	_ = os.Remove(path)

	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return &Server{
		path:     path,
		ln:       ln,
		log:      log,
		dispatch: dispatch,
		acc:      progress.NewStatusAccumulator(),
		clients:  make(map[*clientConn]struct{}),
	}, nil
}

// Serve accepts connections until ctx is cancelled or Close is called.
// Returns nil on graceful shutdown. Run this in its own goroutine — it
// blocks for the daemon's lifetime.
func (s *Server) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		_ = s.Close()
	}()

	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if s.isClosed() {
				return nil
			}
			return err
		}
		c := newClientConn(conn, s)
		s.addClient(c)
		go c.run(ctx)
	}
}

// Close stops accepting connections, closes every client connection, and
// removes the socket file. Safe to call more than once.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	clients := make([]*clientConn, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	s.clients = nil
	s.mu.Unlock()

	err := s.ln.Close()
	for _, c := range clients {
		c.close()
	}
	_ = os.Remove(s.path)
	return err
}

func (s *Server) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *Server) addClient(c *clientConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		c.close()
		return
	}
	s.clients[c] = struct{}{}
}

func (s *Server) removeClient(c *clientConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.clients, c)
}

// Handle implements progress.EventSink: folds ev into the shared status
// accumulator, then broadcasts both the raw event and the refreshed status
// snapshot to every connected client. Never blocks: broadcast uses each
// client's non-blocking, drop-on-full send (see clientConn.send).
func (s *Server) Handle(ev progress.Event) {
	s.acc.Apply(ev)
	status := s.acc.Snapshot()

	evCopy := ev
	s.broadcast(Frame{V: ProtocolVersion, Kind: FrameKindEvent, Event: &evCopy})
	s.broadcast(Frame{V: ProtocolVersion, Kind: FrameKindStatus, Status: &status})
}

func (s *Server) broadcast(fr Frame) {
	s.mu.Lock()
	clients := make([]*clientConn, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.Unlock()
	for _, c := range clients {
		c.send(fr)
	}
}

// clientConn is one connected socket client: an outbound drop-on-full
// channel drained by a writer goroutine, and a reader goroutine decoding
// inbound Request frames and dispatching them.
type clientConn struct {
	conn net.Conn
	srv  *Server

	out chan Frame

	closeOnce sync.Once
	done      chan struct{}
}

func newClientConn(conn net.Conn, srv *Server) *clientConn {
	return &clientConn{
		conn: conn,
		srv:  srv,
		out:  make(chan Frame, clientOutboxSize),
		done: make(chan struct{}),
	}
}

// send enqueues fr for delivery, dropping it silently if the client's
// outbox is full — the backpressure invariant that keeps a stalled client
// from ever costing the server (or, transitively, the daemon loop that
// calls Handle) anything beyond one non-blocking channel send.
func (c *clientConn) send(fr Frame) {
	select {
	case c.out <- fr:
	default:
	}
}

// run drives both directions of the connection until it closes or ctx is
// cancelled: a writer goroutine drains c.out to the wire, while run itself
// reads inbound Request frames and dispatches them (each request handled in
// its own goroutine so a slow verb never stalls reading the next request).
func (c *clientConn) run(ctx context.Context) {
	defer c.srv.removeClient(c)
	defer c.close()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.writeLoop()
	}()

	dec := json.NewDecoder(c.conn)
	for {
		var req Request
		if err := dec.Decode(&req); err != nil {
			break
		}
		wg.Add(1)
		go func(req Request) {
			defer wg.Done()
			c.handleRequest(ctx, req)
		}(req)
	}
	wg.Wait()
}

func (c *clientConn) writeLoop() {
	enc := json.NewEncoder(c.conn)
	for {
		select {
		case fr, ok := <-c.out:
			if !ok {
				return
			}
			if err := enc.Encode(fr); err != nil {
				return
			}
		case <-c.done:
			return
		}
	}
}

func (c *clientConn) handleRequest(ctx context.Context, req Request) {
	reply := DispatchReply{ID: req.ID}
	if c.srv.dispatch == nil {
		reply.Error = "control: dispatch not enabled on this daemon"
	} else if !req.Verb.Known() {
		reply.Error = "control: unknown verb " + string(req.Verb)
	} else {
		summary, err := c.srv.dispatch(ctx, req.Verb, req.Opts)
		if err != nil {
			reply.Error = err.Error()
		} else {
			reply.OK = true
			s := summary
			reply.Summary = &s
		}
	}
	c.send(Frame{V: ProtocolVersion, Kind: FrameKindReply, Reply: &reply})
}

func (c *clientConn) close() {
	c.closeOnce.Do(func() {
		close(c.done)
		_ = c.conn.Close()
	})
}
