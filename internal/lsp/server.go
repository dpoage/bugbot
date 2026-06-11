package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// ServerConfig describes how to launch one language server and which files it
// serves. Configs are matched by file extension in registry order.
type ServerConfig struct {
	// Cmd is the server binary name (resolved via exec.LookPath) or an
	// absolute path.
	Cmd string
	// Args are the command-line arguments (e.g. "--stdio").
	Args []string
	// Env, when non-empty, is appended to the inherited environment. Used by
	// tests to drive a fake server; empty for real servers.
	Env []string
	// LanguageIDs maps file extensions (with leading dot, lowercase) to the
	// LSP languageId used in didOpen.
	LanguageIDs map[string]string
}

// DefaultServers is the built-in server registry: gopls for Go,
// typescript-language-server for TS/JS, pyright (with pylsp as fallback) for
// Python, rust-analyzer for Rust, and clangd for C/C++. Order matters only for
// extensions served by multiple configs (Python): the first config whose binary
// is installed wins.
func DefaultServers() []ServerConfig {
	return []ServerConfig{
		{
			Cmd:         "gopls",
			LanguageIDs: map[string]string{".go": "go"},
		},
		{
			Cmd:  "typescript-language-server",
			Args: []string{"--stdio"},
			LanguageIDs: map[string]string{
				".ts": "typescript", ".tsx": "typescriptreact",
				".js": "javascript", ".jsx": "javascriptreact",
				".mjs": "javascript", ".cjs": "javascript",
			},
		},
		{
			Cmd:         "pyright-langserver",
			Args:        []string{"--stdio"},
			LanguageIDs: map[string]string{".py": "python"},
		},
		{
			Cmd:         "pylsp",
			LanguageIDs: map[string]string{".py": "python"},
		},
		{
			// rust-analyzer speaks LSP over stdio with no extra arguments.
			Cmd:         "rust-analyzer",
			LanguageIDs: map[string]string{".rs": "rust"},
		},
		{
			Cmd: "clangd",
			LanguageIDs: map[string]string{
				".c": "c", ".h": "c",
				".cc": "cpp", ".cpp": "cpp", ".cxx": "cpp", ".hpp": "cpp",
			},
		},
	}
}

// shutdownGrace bounds each step of the shutdown sequence (shutdown request,
// then process exit after the exit notification) before escalating to kill.
const shutdownGrace = 2 * time.Second

// server is one running language server process plus its handshake state.
type server struct {
	cfg     ServerConfig
	rootDir string

	cmd    *exec.Cmd
	conn   *conn
	stderr *tailBuffer
	exited chan struct{} // closed when the process has been reaped

	caps serverCaps

	mu     sync.Mutex
	opened map[string]bool // absolute paths sent via didOpen
}

// serverCaps records the subset of advertised server capabilities we gate
// queries on.
type serverCaps struct {
	definition     bool
	references     bool
	implementation bool
}

// startServer launches the configured binary, performs the
// initialize/initialized handshake (bounded by ctx), and returns a ready
// server. A missing binary returns an *exec.Error from LookPath so callers can
// produce a graceful "not installed" message.
func startServer(ctx context.Context, cfg ServerConfig, rootDir string) (*server, error) {
	path, err := exec.LookPath(cfg.Cmd)
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(path, cfg.Args...)
	cmd.Dir = rootDir
	if len(cfg.Env) > 0 {
		cmd.Env = append(os.Environ(), cfg.Env...)
	}
	stderr := &tailBuffer{}
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("lsp: %s stdin: %w", cfg.Cmd, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("lsp: %s stdout: %w", cfg.Cmd, err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("lsp: start %s: %w", cfg.Cmd, err)
	}

	s := &server{
		cfg:     cfg,
		rootDir: rootDir,
		cmd:     cmd,
		conn:    newConn(stdout, stdin),
		stderr:  stderr,
		exited:  make(chan struct{}),
		opened:  make(map[string]bool),
	}
	go func() {
		err := cmd.Wait()
		s.conn.markDead(fmt.Errorf("%w: %s exited: %v (stderr: %s)",
			ErrConnDead, cfg.Cmd, err, stderr.String()))
		close(s.exited)
	}()

	if err := s.initialize(ctx); err != nil {
		s.kill()
		return nil, err
	}
	return s, nil
}

// initialize performs the LSP handshake and records the server capabilities.
func (s *server) initialize(ctx context.Context) error {
	rootURI := URIFromPath(s.rootDir)
	params := map[string]any{
		"processId": os.Getpid(),
		"clientInfo": map[string]any{
			"name": "bugbot",
		},
		"rootUri": rootURI,
		"workspaceFolders": []map[string]any{
			{"uri": rootURI, "name": filepath.Base(s.rootDir)},
		},
		"capabilities": map[string]any{
			"textDocument": map[string]any{
				"definition":     map[string]any{},
				"references":     map[string]any{},
				"implementation": map[string]any{},
			},
			"workspace": map[string]any{
				"workspaceFolders": true,
				"configuration":    true,
			},
		},
	}

	var result struct {
		Capabilities struct {
			DefinitionProvider     json.RawMessage `json:"definitionProvider"`
			ReferencesProvider     json.RawMessage `json:"referencesProvider"`
			ImplementationProvider json.RawMessage `json:"implementationProvider"`
		} `json:"capabilities"`
	}
	if err := s.conn.call(ctx, "initialize", params, &result); err != nil {
		return fmt.Errorf("lsp: initialize %s: %w", s.cfg.Cmd, err)
	}
	s.caps = serverCaps{
		definition:     capEnabled(result.Capabilities.DefinitionProvider),
		references:     capEnabled(result.Capabilities.ReferencesProvider),
		implementation: capEnabled(result.Capabilities.ImplementationProvider),
	}
	if err := s.conn.notify("initialized", map[string]any{}); err != nil {
		return fmt.Errorf("lsp: initialized %s: %w", s.cfg.Cmd, err)
	}
	return nil
}

// capEnabled interprets the bool-or-object capability union: absent, null, and
// false mean disabled; true or any object means enabled.
func capEnabled(raw json.RawMessage) bool {
	switch string(raw) {
	case "", "null", "false":
		return false
	}
	return true
}

// ensureOpen sends textDocument/didOpen for path (with its on-disk content)
// the first time it is queried. Read-only navigation never edits, so a single
// version-1 open per file is all the sync a server needs from us.
func (s *server) ensureOpen(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.opened[path] {
		return nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("lsp: read %s for didOpen: %w", path, err)
	}
	langID := s.cfg.LanguageIDs[filepath.Ext(path)]
	if langID == "" {
		langID = "plaintext"
	}
	err = s.conn.notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{
			"uri":        URIFromPath(path),
			"languageId": langID,
			"version":    1,
			"text":       string(content),
		},
	})
	if err != nil {
		return err
	}
	s.opened[path] = true
	return nil
}

// query issues one position query (textDocument/definition, references, or
// implementation) for the symbol at pos in path and decodes the locations.
func (s *server) query(ctx context.Context, method, path string, pos Position) ([]Location, error) {
	if err := s.checkCap(method); err != nil {
		return nil, err
	}
	if err := s.ensureOpen(path); err != nil {
		return nil, err
	}
	params := map[string]any{
		"textDocument": map[string]any{"uri": URIFromPath(path)},
		"position":     pos,
	}
	if method == "textDocument/references" {
		// Callers want call sites, not the declaration echoed back.
		params["context"] = map[string]any{"includeDeclaration": false}
	}
	raw, err := s.conn.callRaw(ctx, method, params)
	if err != nil {
		return nil, err
	}
	return decodeLocations(raw)
}

// checkCap gates a query on the capability the server advertised at
// initialize, so unsupported queries fail fast with a clear message instead of
// a server error.
func (s *server) checkCap(method string) error {
	ok := true
	switch method {
	case "textDocument/definition":
		ok = s.caps.definition
	case "textDocument/references":
		ok = s.caps.references
	case "textDocument/implementation":
		ok = s.caps.implementation
	}
	if !ok {
		return fmt.Errorf("lsp: %s does not support %s", s.cfg.Cmd, method)
	}
	return nil
}

// shutdown performs the polite LSP shutdown sequence — shutdown request, exit
// notification — escalating to SIGKILL if the server does not comply within
// shutdownGrace per step.
func (s *server) shutdown() {
	if s.conn.isDead() {
		s.kill()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	_ = s.conn.call(ctx, "shutdown", nil, nil)
	_ = s.conn.notify("exit", nil)

	select {
	case <-s.exited:
	case <-time.After(shutdownGrace):
		s.kill()
	}
}

// kill forcibly terminates the process and waits for it to be reaped.
func (s *server) kill() {
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	<-s.exited
}

// dead reports whether the server's transport has failed (process exit or
// broken pipes).
func (s *server) dead() bool { return s.conn.isDead() }

// tailBuffer keeps the last tailCap bytes written to it, for embedding a
// crashed server's stderr in error messages without unbounded growth.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

const tailCap = 4096

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > tailCap {
		t.buf = t.buf[len(t.buf)-tailCap:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
