package agent

import (
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/lsp"
	"github.com/dpoage/bugbot/internal/treesitter"
)

// TestBrokenBuildResolvesViaTreeSitter is the headline guarantee of this tier:
// on a repo whose build is broken (here a go.mod with an invalid module
// directive and an unresolvable import) and with no working language server,
// find_definition still resolves a symbol — because tree-sitter parses source
// directly and needs no build system.
//
// The LSP tier is wired to a server binary that does not exist, so it fails
// with a "not installed" error exactly as a missing/non-working gopls would,
// driving the fallback deterministically without depending on whether gopls is
// installed on the host.
func TestBrokenBuildResolvesViaTreeSitter(t *testing.T) {
	files := map[string]string{
		// Deliberately broken build: malformed module path and an import that
		// cannot resolve. gopls would refuse to index this; tree-sitter ignores
		// it entirely.
		"go.mod":     "module !!!not a valid module!!!\n\ngo 1.22\n",
		"greeter.go": "package broken\n\nimport \"example.com/does/not/exist\"\n\nfunc Hello() string { return exist.Thing() }\n",
		"use.go":     "package broken\n\nfunc caller() string { return Hello() }\n",
	}
	c, dir := newTestCodeNav(t, files, nil)
	_ = c.nav.Close()

	// LSP tier points at a binary that is guaranteed absent.
	mgr := lsp.NewManager(dir, lsp.WithServers([]lsp.ServerConfig{{
		Cmd:         "bugbot-nonexistent-language-server",
		LanguageIDs: map[string]string{".go": "go"},
	}}))
	c.nav = newTieredNavigator(&lspNavigator{mgr: mgr}, treesitter.New(dir), dir)

	out, err := runTool(t, toolByName(t, c, "find_definition"),
		codeNavArgs{File: "use.go", Line: 3, Symbol: "Hello"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "greeter.go:5:") {
		t.Errorf("definition must resolve via tree-sitter despite the broken build:\n%s", out)
	}
	if !strings.Contains(out, "syntactic match") {
		t.Errorf("result must be labeled as a syntactic-tier match:\n%s", out)
	}
}
