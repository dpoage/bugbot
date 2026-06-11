package lsp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestWorkspace creates a workspace dir with one Go file for the fake
// server to "serve" and returns the dir and the file's absolute path.
func newTestWorkspace(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "main.go")
	if err := os.WriteFile(file, []byte("package main\n\nfunc Hello() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, file
}

func newTestManager(t *testing.T, cfg ServerConfig, root string) *Manager {
	t.Helper()
	m := NewManager(root,
		WithServers([]ServerConfig{cfg}),
		WithQueryTimeout(10*time.Second),
	)
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func TestManagerHandshakeAndQuery(t *testing.T) {
	root, file := newTestWorkspace(t)
	m := newTestManager(t, fakeServerConfig(t), root)

	ctx := context.Background()
	pos := Position{Line: 2, Character: 5}
	locs, err := m.Definition(ctx, file, pos)
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("got %d locations, want 1", len(locs))
	}
	// The fake echoes the wire position back; this proves handshake, didOpen
	// (the fake rejects queries on unopened docs), and position transport.
	if locs[0].Range.Start != pos {
		t.Errorf("echoed position = %+v, want %+v", locs[0].Range.Start, pos)
	}
	if got, ok := PathFromURI(locs[0].URI); !ok || got != file {
		t.Errorf("location uri = %q, want path %q", locs[0].URI, file)
	}

	// References and implementation ride the same plumbing.
	if _, err := m.References(ctx, file, pos); err != nil {
		t.Errorf("References: %v", err)
	}
	if _, err := m.Implementation(ctx, file, pos); err != nil {
		t.Errorf("Implementation: %v", err)
	}
}

func TestManagerMissingBinary(t *testing.T) {
	root, file := newTestWorkspace(t)
	cfg := ServerConfig{
		Cmd:         "bugbot-test-no-such-language-server",
		LanguageIDs: map[string]string{".go": "go"},
	}
	m := newTestManager(t, cfg, root)

	_, err := m.Definition(context.Background(), file, Position{})
	if err == nil {
		t.Fatal("expected error for missing binary")
	}
	if !strings.Contains(err.Error(), "bugbot-test-no-such-language-server") ||
		!strings.Contains(err.Error(), "not found in PATH") {
		t.Errorf("error should name the missing binary and degradation path: %v", err)
	}
}

func TestManagerNoServerForExtension(t *testing.T) {
	root, _ := newTestWorkspace(t)
	m := newTestManager(t, fakeServerConfig(t), root)

	_, err := m.Definition(context.Background(), filepath.Join(root, "data.csv"), Position{})
	if err == nil || !strings.Contains(err.Error(), "no language server is configured") {
		t.Errorf("expected unsupported-extension error, got %v", err)
	}
}

func TestManagerCrashRestart(t *testing.T) {
	root, file := newTestWorkspace(t)
	flag := filepath.Join(t.TempDir(), "crashed-once")
	cfg := fakeServerConfig(t, "FAKE_LSP_CRASH_ONCE_FILE="+flag)
	m := newTestManager(t, cfg, root)

	// First query: the server crashes mid-request, the manager restarts it
	// once, and the retried query succeeds against the new instance.
	locs, err := m.Definition(context.Background(), file, Position{Line: 1, Character: 0})
	if err != nil {
		t.Fatalf("Definition after crash+restart: %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("got %d locations, want 1", len(locs))
	}
	if _, err := os.Stat(flag); err != nil {
		t.Errorf("crash flag file should exist (server crashed once): %v", err)
	}
}

func TestManagerRepeatedCrashDisablesServer(t *testing.T) {
	root, file := newTestWorkspace(t)
	cfg := fakeServerConfig(t, "FAKE_LSP_CRASH_ON=textDocument/definition")
	m := newTestManager(t, cfg, root)

	// First call: crash, restart, crash again -> transport error.
	_, err := m.Definition(context.Background(), file, Position{})
	if err == nil {
		t.Fatal("expected error from repeatedly crashing server")
	}

	// Next call: restarts are exhausted; the language is permanently disabled
	// with a clear degradation message.
	_, err = m.Definition(context.Background(), file, Position{})
	if err == nil || !strings.Contains(err.Error(), "crashed repeatedly") {
		t.Errorf("expected permanent-failure error, got %v", err)
	}
}

func TestManagerCapabilityGate(t *testing.T) {
	root, file := newTestWorkspace(t)
	cfg := fakeServerConfig(t, `FAKE_LSP_CAPS={"definitionProvider":true,"referencesProvider":true}`)
	m := newTestManager(t, cfg, root)

	if _, err := m.Definition(context.Background(), file, Position{}); err != nil {
		t.Fatalf("Definition should be supported: %v", err)
	}
	_, err := m.Implementation(context.Background(), file, Position{})
	if err == nil || !strings.Contains(err.Error(), "does not support") {
		t.Errorf("expected capability error, got %v", err)
	}
}

func TestManagerCleanShutdown(t *testing.T) {
	root, file := newTestWorkspace(t)
	m := NewManager(root,
		WithServers([]ServerConfig{fakeServerConfig(t)}),
		WithQueryTimeout(10*time.Second),
	)
	if _, err := m.Definition(context.Background(), file, Position{}); err != nil {
		t.Fatalf("Definition: %v", err)
	}

	// A polite shutdown (shutdown request honored by the server) completes
	// well within the kill-escalation grace period.
	start := time.Now()
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if d := time.Since(start); d > shutdownGrace {
		t.Errorf("polite shutdown took %s; server likely had to be killed", d)
	}

	// Closed managers refuse new queries and Close is idempotent.
	if _, err := m.Definition(context.Background(), file, Position{}); err == nil {
		t.Error("query after Close should fail")
	}
	if err := m.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestManagerKillsUnresponsiveServerOnClose(t *testing.T) {
	root, file := newTestWorkspace(t)
	cfg := fakeServerConfig(t, "FAKE_LSP_NO_SHUTDOWN=1")
	m := NewManager(root,
		WithServers([]ServerConfig{cfg}),
		WithQueryTimeout(10*time.Second),
	)
	if _, err := m.Definition(context.Background(), file, Position{}); err != nil {
		t.Fatalf("Definition: %v", err)
	}

	// The server ignores shutdown and exit; Close must escalate to kill and
	// still return (bounded by the two grace periods) rather than hang.
	done := make(chan struct{})
	go func() {
		_ = m.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * shutdownGrace):
		t.Fatal("Close hung on an unresponsive server")
	}
}

func TestManagerQueryTimeout(t *testing.T) {
	root, file := newTestWorkspace(t)
	// The server completes the handshake but never answers definition queries,
	// simulating a server stuck indexing; the manager's own query timeout must
	// fire and blame indexing.
	cfg := fakeServerConfig(t, "FAKE_LSP_STALL_ON=textDocument/definition")
	m := NewManager(root,
		WithServers([]ServerConfig{cfg}),
		WithQueryTimeout(300*time.Millisecond),
	)
	t.Cleanup(func() { _ = m.Close() })

	_, err := m.Definition(context.Background(), file, Position{})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "may still be indexing") {
		t.Errorf("expected the indexing-timeout message, got %v", err)
	}

	// An already-expired parent deadline must NOT be blamed on indexing: the
	// inherited cancellation is reported as is.
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, err = m.References(ctx, file, Position{})
	if err == nil {
		t.Fatal("expected error from expired parent context")
	}
	if strings.Contains(err.Error(), "may still be indexing") {
		t.Errorf("parent-deadline error must not blame indexing: %v", err)
	}
}

func TestManagerConcurrentQueries(t *testing.T) {
	root, file := newTestWorkspace(t)
	m := newTestManager(t, fakeServerConfig(t), root)

	// Many goroutines race the lazy first spawn and then share one server;
	// under -race this exercises the spawn/pending-map/restart interleavings
	// the Tool contract requires to be safe.
	var wg sync.WaitGroup
	errs := make(chan error, 16*3)
	for g := range 16 {
		wg.Go(func() {
			pos := Position{Line: 2, Character: g % 7}
			for range 3 {
				if _, err := m.Definition(context.Background(), file, pos); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent Definition: %v", err)
	}
}

// TestDefaultServersRustAnalyzer asserts that the built-in registry contains
// exactly one entry for ".rs" files, that it is rust-analyzer, and that it
// carries no extra arguments (rust-analyzer uses stdio by default).
func TestDefaultServersRustAnalyzer(t *testing.T) {
	servers := DefaultServers()

	var rsConfigs []ServerConfig
	for _, cfg := range servers {
		if _, ok := cfg.LanguageIDs[".rs"]; ok {
			rsConfigs = append(rsConfigs, cfg)
		}
	}

	if len(rsConfigs) != 1 {
		t.Fatalf("expected exactly one config claiming .rs, got %d: %v", len(rsConfigs), rsConfigs)
	}
	cfg := rsConfigs[0]
	if cfg.Cmd != "rust-analyzer" {
		t.Errorf("config for .rs has Cmd %q, want %q", cfg.Cmd, "rust-analyzer")
	}
	if got := cfg.LanguageIDs[".rs"]; got != "rust" {
		t.Errorf("languageId for .rs = %q, want %q", got, "rust")
	}
	if len(cfg.Args) != 0 {
		t.Errorf("rust-analyzer config must have no Args, got %v", cfg.Args)
	}
}

func TestManagerConcurrentQueriesAcrossCrash(t *testing.T) {
	root, file := newTestWorkspace(t)
	flag := filepath.Join(t.TempDir(), "crashed-once")
	cfg := fakeServerConfig(t, "FAKE_LSP_CRASH_ONCE_FILE="+flag)
	m := newTestManager(t, cfg, root)

	// The first instance crashes on its first query while several queries are
	// in flight; every caller retries once against the restarted instance, so
	// all must succeed and the restart accounting must not disable the server.
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			if _, err := m.Definition(context.Background(), file, Position{Line: 1}); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("Definition across crash: %v", err)
	}
}
