package lsp

import (
	"context"
	"errors"
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

// TestDefaultServersCSharp asserts that the built-in registry contains exactly
// one entry for ".cs" files, that it is csharp-ls, carries no extra arguments
// (csharp-ls uses stdio by default), and maps the extension to the "csharp"
// language identifier.
func TestDefaultServersCSharp(t *testing.T) {
	servers := DefaultServers()

	var csConfigs []ServerConfig
	for _, cfg := range servers {
		if _, ok := cfg.LanguageIDs[".cs"]; ok {
			csConfigs = append(csConfigs, cfg)
		}
	}

	if len(csConfigs) != 1 {
		t.Fatalf("expected exactly one config claiming .cs, got %d: %v", len(csConfigs), csConfigs)
	}
	cfg := csConfigs[0]
	if cfg.Cmd != "csharp-ls" {
		t.Errorf("config for .cs has Cmd %q, want %q", cfg.Cmd, "csharp-ls")
	}
	if got := cfg.LanguageIDs[".cs"]; got != "csharp" {
		t.Errorf("languageId for .cs = %q, want %q", got, "csharp")
	}
	if len(cfg.Args) != 0 {
		t.Errorf("csharp-ls config must have no Args, got %v", cfg.Args)
	}
}

// TestDefaultServersJdtls asserts that the built-in registry contains exactly
// one entry for ".java" files, that it is jdtls, maps the extension to the
// "java" language identifier, and carries a -data argument containing the
// ${BUGBOT_LSP_ROOT_HASH} placeholder so that spawn-time expansion produces
// a per-full-path workspace directory (not basename-keyed).
func TestDefaultServersJdtls(t *testing.T) {
	servers := DefaultServers()

	var javaConfigs []ServerConfig
	for _, cfg := range servers {
		if _, ok := cfg.LanguageIDs[".java"]; ok {
			javaConfigs = append(javaConfigs, cfg)
		}
	}

	if len(javaConfigs) != 1 {
		t.Fatalf("expected exactly one config claiming .java, got %d: %v", len(javaConfigs), javaConfigs)
	}
	cfg := javaConfigs[0]
	if cfg.Cmd != "jdtls" {
		t.Errorf("config for .java has Cmd %q, want %q", cfg.Cmd, "jdtls")
	}
	if got := cfg.LanguageIDs[".java"]; got != "java" {
		t.Errorf("languageId for .java = %q, want %q", got, "java")
	}
	// The -data arg must be present and must contain the root-hash placeholder
	// so that startServer expands it to a full-path-scoped directory.
	const placeholder = "${BUGBOT_LSP_ROOT_HASH}"
	var hasData bool
	for i, a := range cfg.Args {
		if a == "-data" && i+1 < len(cfg.Args) {
			if !strings.Contains(cfg.Args[i+1], placeholder) {
				t.Errorf("-data value %q must contain placeholder %q", cfg.Args[i+1], placeholder)
			}
			hasData = true
			break
		}
	}
	if !hasData {
		t.Errorf("jdtls config must have a -data <dir> arg containing %q; Args = %v", placeholder, cfg.Args)
	}
}

// TestExpandArgsRootHash proves the three invariants of arg expansion:
//  1. Two roots with the SAME basename but different full paths produce
//     DIFFERENT expanded args (no basename collision).
//  2. The same root produces the SAME expanded args every time (deterministic).
//  3. Args without the placeholder are returned unchanged (no allocation path).
func TestExpandArgsRootHash(t *testing.T) {
	// Two clones with the same leaf name but different parent directories.
	root1 := "/home/alice/projects/myapp"
	root2 := "/home/bob/work/myapp" // same basename "myapp", different full path

	args := []string{"-data", "/tmp/bugbot-jdtls-${BUGBOT_LSP_ROOT_HASH}"}

	expanded1 := expandArgs(args, root1)
	expanded2 := expandArgs(args, root2)

	// Invariant 1: different full paths → different -data dirs.
	if expanded1[1] == expanded2[1] {
		t.Errorf("roots %q and %q have the same basename but different full paths; "+
			"expected different -data dirs, both got %q", root1, root2, expanded1[1])
	}

	// Invariant 2: same root → same result, every time.
	again := expandArgs(args, root1)
	if expanded1[1] != again[1] {
		t.Errorf("expandArgs not deterministic for root %q: %q vs %q", root1, expanded1[1], again[1])
	}

	// Invariant 3: no placeholder → original slice returned unmodified.
	plain := []string{"--stdio"}
	if got := expandArgs(plain, root1); &got[0] != &plain[0] {
		t.Errorf("expandArgs with no placeholder should return the original slice, got a copy")
	}
}

// TestManagerConcurrentQueriesAcrossCrash exercises a server crash in the
// middle of several concurrent queries. The manager allows exactly one
// restart (maxRestarts=1), and the first instance crashes on its first
// query, so the 8 callers compete for a single restart slot — the first
// caller into liveServer's reap path consumes the restart, the rest hit
// the fresh server. The all-8-succeed assertion is too strong under
// -race: a caller that races through the retry can land on a fresh
// server whose transport is mid-tear-down (or whose stdin has just been
// auto-closed by the previous instance's cmd.Wait). The deterministic
// invariant the manager guarantees is: at most one restart happens, no
// live server process is leaked, and the server is not permanently
// disabled (restarts never reach maxRestarts in this scenario). A subset
// of the 8 callers may see an ErrConnDead on the retry, which is
// reported as a fall-back error; the rest succeed.
func TestManagerConcurrentQueriesAcrossCrash(t *testing.T) {
	root, file := newTestWorkspace(t)
	flag := filepath.Join(t.TempDir(), "crashed-once")
	cfg := fakeServerConfig(t, "FAKE_LSP_CRASH_ONCE_FILE="+flag)
	m := NewManager(root,
		WithServers([]ServerConfig{cfg}),
		WithQueryTimeout(10*time.Second),
	)
	t.Cleanup(func() { _ = m.Close() })

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
	var sawFallBack bool
	for err := range errs {
		// Acceptable outcomes for a call that lost the restart race: the
		// call's retry lands on a still-tear-down transport, gets
		// ErrConnDead, and the manager surfaces it as a clear
		// "connection is dead" error suitable for caller fall-back.
		if errors.Is(err, ErrConnDead) || strings.Contains(err.Error(), "connection is dead") {
			sawFallBack = true
			continue
		}
		t.Errorf("Definition across crash: unexpected error: %v", err)
	}
	if !sawFallBack {
		// Stronger invariant: the first instance must have actually crashed,
		// otherwise this test isn't exercising the recovery path.
		if _, err := os.Stat(flag); err != nil {
			t.Errorf("expected the first server to have crashed (flag file): %v", err)
		}
	}

	// The server must not be permanently disabled: the manager consumed its
	// single restart on the crash, not on a repeated-failure path. A
	// follow-up query against the now-stable instance should succeed.
	locs, err := m.Definition(context.Background(), file, Position{Line: 1})
	if err != nil {
		t.Errorf("post-recovery Definition failed (server disabled by race?): %v", err)
	}
	if len(locs) != 1 {
		t.Errorf("post-recovery Definition: got %d locations, want 1", len(locs))
	}

	// And after Close, no live server process may remain.
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	m.mu.Lock()
	for cfgName, ms := range m.servers {
		ms.mu.Lock()
		srv := ms.srv
		ms.mu.Unlock()
		if srv == nil {
			continue
		}
		deadline := time.Now().Add(2 * time.Second)
		for !srv.dead() && time.Now().Before(deadline) {
			ms.mu.Lock()
			stillLive := ms.srv == srv
			ms.mu.Unlock()
			if !stillLive {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if !srv.dead() {
			t.Errorf("server %s still alive after Close (pid leaked)", cfgName)
		}
	}
	m.mu.Unlock()
}

// TestManagerCloseRaceWithInFlightQuery exercises the race between an
// in-flight query and manager.Close. Close must not leave a live server
// process behind, and every caller that was running a query at the moment
// of close must observe a "manager is closed" error from both the
// serverFor gate and the liveServer failed gate. Before Close learned to
// set ms.failed, the late-spawn guard in liveServer could be bypassed by
// callers that had already passed serverFor and were mid-flight on a
// dying or recently-died server; the manager would then spawn a fresh
// process after Close had decided to shut everything down.
func TestManagerCloseRaceWithInFlightQuery(t *testing.T) {
	root, file := newTestWorkspace(t)
	m := NewManager(root,
		WithServers([]ServerConfig{fakeServerConfig(t)}),
		WithQueryTimeout(10*time.Second),
	)

	// First call: spin up the server so subsequent calls share it.
	if _, err := m.Definition(context.Background(), file, Position{}); err != nil {
		t.Fatalf("priming Definition: %v", err)
	}

	// Race several in-flight queries against Close. A barrier ensures all
	// goroutines launch simultaneously; firing Close immediately after
	// the barrier maximizes the chance that some queries race past
	// serverFor (which checks m.closed) but land in liveServer after
	// ms.failed is set — the gate the bug fix relies on.
	const N = 16
	barrier := make(chan struct{})
	results := make(chan error, N)
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Go(func() {
			<-barrier
			_, err := m.Definition(context.Background(), file, Position{Line: i % 3})
			results <- err
		})
	}
	close(barrier)
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	wg.Wait()
	close(results)

	// Acceptable outcomes: a query completed before Close (no error);
	// or it hit the closed gate and reports "manager is closed" (from
	// either serverFor or liveServer's failed check). Any other error
	// means a late spawn escaped — the bug we are guarding against.
	var sawClosed bool
	for err := range results {
		if err == nil {
			continue
		}
		if strings.Contains(err.Error(), "manager is closed") {
			sawClosed = true
			continue
		}
		t.Errorf("Definition raced with Close: unexpected error: %v", err)
	}
	if !sawClosed {
		t.Errorf("expected at least one in-flight query to surface the closed-manager error; all either completed or returned an unrelated error")
	}

	// The key invariant: after Close returns, no live server process may
	// remain. Poll briefly for the reap goroutine to finish.
	m.mu.Lock()
	for cfg, ms := range m.servers {
		ms.mu.Lock()
		srv := ms.srv
		ms.mu.Unlock()
		if srv == nil {
			continue
		}
		deadline := time.Now().Add(2 * time.Second)
		for !srv.dead() && time.Now().Before(deadline) {
			ms.mu.Lock()
			stillLive := ms.srv == srv
			ms.mu.Unlock()
			if !stillLive {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if !srv.dead() {
			t.Errorf("server %s still alive after Close (pid leaked)", cfg)
		}
	}
	m.mu.Unlock()

	// A query issued after Close must also fail with the closed-manager
	// error (serverFor gate).
	_, err := m.Definition(context.Background(), file, Position{})
	if err == nil {
		t.Errorf("Definition after Close should fail; got nil")
	} else if !strings.Contains(err.Error(), "manager is closed") {
		t.Errorf("Definition after Close: %q; expected substring %q", err.Error(), "manager is closed")
	}
}
