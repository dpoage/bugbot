package lsp

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DefaultQueryTimeout bounds a single navigation query, including (for the
// first query against a language) server spawn, handshake, and initial
// indexing. Language servers like gopls can take a while to index a large
// repository before answering; we never hang on them — on timeout the caller
// gets a clear error telling it to fall back to text search.
const DefaultQueryTimeout = 60 * time.Second

// maxRestarts is how many times a crashed server is restarted before its
// language is marked permanently failed for the manager's lifetime.
const maxRestarts = 1

// Manager owns the per-language server processes for one workspace root. It
// spawns servers lazily on first query for their language, restarts a crashed
// server once, and shuts everything down on Close. It is safe for concurrent
// use by parallel agents.
type Manager struct {
	root    string
	configs []ServerConfig
	timeout time.Duration

	mu      sync.Mutex
	servers map[string]*managedServer // key: ServerConfig.Cmd
	closed  bool
}

// managedServer wraps one server's lifecycle state. Its mutex serializes
// spawn/restart decisions for that language; queries themselves run
// concurrently on the underlying connection.
type managedServer struct {
	cfg ServerConfig

	mu       sync.Mutex
	srv      *server
	restarts int
	failed   error // permanent: set when restarts are exhausted
}

// Option configures a Manager.
type Option func(*Manager)

// WithQueryTimeout overrides DefaultQueryTimeout.
func WithQueryTimeout(d time.Duration) Option {
	return func(m *Manager) {
		if d > 0 {
			m.timeout = d
		}
	}
}

// WithServers replaces the built-in server registry (used by tests to inject a
// fake server binary).
func WithServers(cfgs []ServerConfig) Option {
	return func(m *Manager) { m.configs = cfgs }
}

// NewManager creates a manager for the workspace rooted at root (must be an
// absolute path). No server processes are started until the first query.
func NewManager(root string, opts ...Option) *Manager {
	m := &Manager{
		root:    root,
		configs: DefaultServers(),
		timeout: DefaultQueryTimeout,
		servers: make(map[string]*managedServer),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Definition returns the definition location(s) of the symbol at pos in the
// absolute path. pos is an LSP position (zero-based line, UTF-16 character).
func (m *Manager) Definition(ctx context.Context, path string, pos Position) ([]Location, error) {
	return m.query(ctx, "textDocument/definition", path, pos)
}

// References returns all references to the symbol at pos, excluding its
// declaration.
func (m *Manager) References(ctx context.Context, path string, pos Position) ([]Location, error) {
	return m.query(ctx, "textDocument/references", path, pos)
}

// Implementation returns the implementations of the interface or abstract
// method at pos.
func (m *Manager) Implementation(ctx context.Context, path string, pos Position) ([]Location, error) {
	return m.query(ctx, "textDocument/implementation", path, pos)
}

// query routes one navigation request to the server for path's language,
// bounded by the manager's query timeout, restarting a crashed server once.
func (m *Manager) query(ctx context.Context, method, path string, pos Position) ([]Location, error) {
	ms, err := m.serverFor(path)
	if err != nil {
		return nil, err
	}

	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	locs, err := m.queryOnce(ctx, ms, method, path, pos)
	if err == nil {
		return locs, nil
	}
	// A dead transport (server crashed) warrants one restart-and-retry; any
	// other failure (timeout, server-side error) is reported as is.
	if errors.Is(err, ErrConnDead) {
		locs, err = m.queryOnce(ctx, ms, method, path, pos)
		if err == nil {
			return locs, nil
		}
	}
	// Only blame indexing when it was our own per-query timeout that fired; an
	// inherited parent deadline says nothing about the server's state.
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil && parent.Err() == nil {
		return nil, fmt.Errorf("%s timed out after %s — the language server may still be indexing; fall back to grep", method, m.timeout)
	}
	return nil, err
}

// queryOnce obtains (spawning or restarting as needed) the live server and
// issues a single query against it.
func (m *Manager) queryOnce(ctx context.Context, ms *managedServer, method, path string, pos Position) ([]Location, error) {
	srv, err := m.liveServer(ctx, ms)
	if err != nil {
		return nil, err
	}
	return srv.query(ctx, method, path, pos)
}

// serverFor picks the managed server responsible for path's extension,
// creating its (not yet started) state on first use. When several configs
// claim the extension (e.g. pyright and pylsp for .py), the first one whose
// binary is installed wins; an already-running server for the extension is
// always preferred.
func (m *Manager) serverFor(path string) (*managedServer, error) {
	ext := strings.ToLower(filepath.Ext(path))

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, fmt.Errorf("lsp: manager is closed")
	}

	var candidates []ServerConfig
	for _, cfg := range m.configs {
		if _, ok := cfg.LanguageIDs[ext]; !ok {
			continue
		}
		// Prefer a server already tracked for this extension.
		if ms, ok := m.servers[cfg.Cmd]; ok {
			return ms, nil
		}
		candidates = append(candidates, cfg)
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no language server is configured for %q files", ext)
	}

	var missing []string
	for _, cfg := range candidates {
		if _, err := exec.LookPath(cfg.Cmd); err != nil {
			missing = append(missing, cfg.Cmd)
			continue
		}
		ms := &managedServer{cfg: cfg}
		m.servers[cfg.Cmd] = ms
		return ms, nil
	}
	return nil, fmt.Errorf("language server not installed for %q files: %s not found in PATH — fall back to grep",
		ext, strings.Join(missing, ", "))
}

// liveServer returns ms's running server, starting or restarting it as
// allowed. A server that has crashed more than maxRestarts times is
// permanently failed for this manager.
//
// It checks m.closed under m.mu before spawning to close the race between a
// goroutine that obtained ms from serverFor (which gates on m.closed) and a
// concurrent Close() that sets m.closed then walks ms entries: without this
// second check a goroutine could pass the serverFor gate, lose the race to
// Close, and then enter liveServer before Close has set ms.failed — and start
// a new server process after the manager is already shut down.
func (m *Manager) liveServer(ctx context.Context, ms *managedServer) (*server, error) {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	if ms.failed != nil {
		return nil, ms.failed
	}

	// Secondary closed-check: a goroutine that passed serverFor's m.closed gate
	// may race with Close() before Close has had a chance to set ms.failed.
	// Check m.closed here (under m.mu, not ms.mu — no inversion: m.mu is
	// always taken before ms.mu in serverFor and Close) so we never spawn a
	// new server process after the manager is shut down.
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		closedErr := fmt.Errorf("lsp: manager is closed — fall back to grep")
		ms.failed = closedErr
		return nil, closedErr
	}

	if ms.srv != nil && !ms.srv.dead() {
		return ms.srv, nil
	}
	if ms.srv != nil {
		// Previous instance died. Reap it and decide whether to restart.
		ms.srv.kill()
		ms.srv = nil
		if ms.restarts >= maxRestarts {
			ms.failed = fmt.Errorf("lsp: %s crashed repeatedly and was disabled for this run — fall back to grep", ms.cfg.Cmd)
			return nil, ms.failed
		}
		ms.restarts++
	}

	srv, err := startServer(ctx, ms.cfg, m.root)
	if err != nil {
		var execErr *exec.Error
		if errors.As(err, &execErr) {
			return nil, fmt.Errorf("language server %q is not installed (not found in PATH) — fall back to grep", ms.cfg.Cmd)
		}
		return nil, err
	}
	ms.srv = srv
	return srv, nil
}

// Close shuts down every running server (shutdown request, exit notification,
// kill on timeout). The manager is unusable afterwards. Safe to call multiple
// times.
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	servers := make([]*managedServer, 0, len(m.servers))
	for _, ms := range m.servers {
		servers = append(servers, ms)
	}
	m.mu.Unlock()

	// Mark every tracked managed server as permanently failed. The existing
	// failed gate in liveServer then blocks late spawns for concurrent callers
	// on BOTH the serverFor route (which only blocks NEW extensions) and the
	// ErrConnDead retry route (which re-enters liveServer, where this gate
	// fires). The error message contains "manager is closed" so
	// shouldFallBack (internal/agent/tools_codenav_tiered.go) recognizes it.
	// We read m.closed and snapshot servers while holding m.mu, then drop it
	// before taking each ms.mu — holding m.mu while taking ms.mu would invert
	// the lock order taken by serverFor.
	closedErr := errors.New("lsp: manager is closed — fall back to grep")
	var wg sync.WaitGroup
	for _, ms := range servers {
		ms.mu.Lock()
		ms.failed = closedErr
		srv := ms.srv
		ms.srv = nil
		ms.mu.Unlock()
		if srv == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			srv.shutdown()
		}()
	}
	wg.Wait()
	return nil
}
