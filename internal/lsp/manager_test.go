package lsp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
	// The fake answers initialize but we crash it on references... instead,
	// use NO_SHUTDOWN fake which simply never answers an unknown method?
	// Simpler: a server that never responds to references is simulated by
	// pointing references at a method the fake ignores. The fake answers all
	// three queries, so instead set an unreasonably small timeout and rely on
	// process spawn + handshake latency to exceed it.
	m := NewManager(root,
		WithServers([]ServerConfig{fakeServerConfig(t)}),
		WithQueryTimeout(time.Nanosecond),
	)
	t.Cleanup(func() { _ = m.Close() })

	_, err := m.Definition(context.Background(), file, Position{})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "deadline") && !strings.Contains(err.Error(), "timed out") {
		t.Errorf("expected a timeout-flavored error, got %v", err)
	}
}
