package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/lsp"
	"github.com/dpoage/bugbot/internal/treesitter"
)

// countingLSP is a fake LSP-tier navigator that returns a scripted result/error
// and records how many times each method was called, so tests can assert the
// tree-sitter tier was (or was not) consulted.
type countingLSP struct {
	res   navResult
	err   error
	calls int
}

func (c *countingLSP) Definition(context.Context, string, lsp.Position) (navResult, error) {
	c.calls++
	return c.res, c.err
}
func (c *countingLSP) References(context.Context, string, lsp.Position) (navResult, error) {
	c.calls++
	return c.res, c.err
}
func (c *countingLSP) Implementation(context.Context, string, lsp.Position) (navResult, error) {
	c.calls++
	return c.res, c.err
}
func (c *countingLSP) Close() error { return nil }

// newTieredCodeNav builds a CodeNav whose navigator is a tieredNavigator over a
// scripted LSP tier and the real tree-sitter backend rooted at the temp repo.
func newTieredCodeNav(t *testing.T, files map[string]string, lspNav navigator) (*CodeNav, string) {
	t.Helper()
	c, dir := newTestCodeNav(t, files, nil)
	_ = c.nav.Close()
	c.nav = newTieredNavigator(lspNav, treesitter.New(dir), dir)
	t.Cleanup(func() { _ = c.nav.Close() })
	return c, dir
}

// TestTierLSPHealthyShortCircuits proves that when the LSP tier answers (no
// error), the tree-sitter tier is never consulted and no syntactic caveat
// leaks into the output.
func TestTierLSPHealthyShortCircuits(t *testing.T) {
	files := map[string]string{"main.go": "package main\n\nfunc Hello() {}\n"}
	lspNav := &countingLSP{}
	c, dir := newTieredCodeNav(t, files, lspNav)
	lspNav.res = navResult{Locations: []lsp.Location{{
		URI:   lsp.URIFromPath(dir + "/main.go"),
		Range: lsp.Range{Start: lsp.Position{Line: 2}},
	}}}

	out, err := runTool(t, toolByName(t, c, "find_definition"),
		codeNavArgs{File: "main.go", Line: 3, Symbol: "Hello"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(out, "syntactic") {
		t.Errorf("healthy LSP result must not carry a syntactic caveat:\n%s", out)
	}
	if !strings.Contains(out, "main.go:3") {
		t.Errorf("expected LSP-tier location, got:\n%s", out)
	}
}

// TestTierFallsBackOnMissingBinary proves a "not installed" LSP error routes to
// the tree-sitter tier, which resolves the definition syntactically and labels
// the result with a tier caveat.
func TestTierFallsBackOnMissingBinary(t *testing.T) {
	files := map[string]string{
		"greeter.go": "package main\n\nfunc Hello() string { return \"hi\" }\n",
		"use.go":     "package main\n\nvar _ = Hello\n",
	}
	lspNav := &countingLSP{err: errors.New("language server \"gopls\" is not installed (not found in PATH) — fall back to grep")}
	c, _ := newTieredCodeNav(t, files, lspNav)

	out, err := runTool(t, toolByName(t, c, "find_definition"),
		codeNavArgs{File: "use.go", Line: 3, Symbol: "Hello"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "greeter.go:3:") {
		t.Errorf("tree-sitter tier should resolve the definition:\n%s", out)
	}
	if !strings.Contains(out, "syntactic match") {
		t.Errorf("fallback result must carry a syntactic caveat:\n%s", out)
	}
}

// TestTierDoesNotFallBackOnTimeout proves an indexing timeout keeps the LSP
// tier: the timeout error surfaces unchanged and tree-sitter is not consulted.
func TestTierDoesNotFallBackOnTimeout(t *testing.T) {
	files := map[string]string{"main.go": "package main\n\nfunc Hello() {}\n"}
	lspNav := &countingLSP{err: errors.New("textDocument/definition timed out after 60s — the language server may still be indexing; fall back to grep")}
	c, _ := newTieredCodeNav(t, files, lspNav)

	_, err := runTool(t, toolByName(t, c, "find_definition"),
		codeNavArgs{File: "main.go", Line: 3, Symbol: "Hello"})
	if err == nil || !strings.Contains(err.Error(), "still be indexing") {
		t.Fatalf("indexing timeout must surface unchanged, got %v", err)
	}
}

// TestTierGrammarMissingDegrades proves that when the LSP tier is unavailable
// and no tree-sitter grammar exists for the language, the tool returns a clear
// "fall back to grep" degradation rather than silently empty results.
func TestTierGrammarMissingDegrades(t *testing.T) {
	files := map[string]string{"main.rb": "def hello\n  puts 'hi'\nend\n"}
	lspNav := &countingLSP{err: errors.New("no language server is configured for \".rb\" files")}
	c, _ := newTieredCodeNav(t, files, lspNav)

	_, err := runTool(t, toolByName(t, c, "find_definition"),
		codeNavArgs{File: "main.rb", Line: 1, Symbol: "hello"})
	if err == nil || !strings.Contains(err.Error(), "fall back to grep") {
		t.Fatalf("unsupported language must degrade with a grep hint, got %v", err)
	}
}

// TestTierReferencesFallback proves references fall back too, and that the
// syntactic tier excludes comment/string mentions even on the fallback path
// (the property that makes the tier worth more than grep).
func TestTierReferencesFallback(t *testing.T) {
	files := map[string]string{
		"main.go": "package main\n" +
			"\n" +
			"// Greet mentioned in a comment\n" +
			"func Greet() {}\n" +
			"\n" +
			"func caller() {\n" +
			"\tx := \"Greet in a string\"\n" +
			"\t_ = x\n" +
			"\tGreet()\n" +
			"}\n",
	}
	lspNav := &countingLSP{err: errors.New("lsp: gopls crashed repeatedly and was disabled for this run — fall back to grep")}
	c, _ := newTieredCodeNav(t, files, lspNav)

	out, err := runTool(t, toolByName(t, c, "find_references"),
		codeNavArgs{File: "main.go", Line: 4, Symbol: "Greet"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "main.go:9:") {
		t.Errorf("expected the call site at line 9:\n%s", out)
	}
	if strings.Contains(out, "main.go:3:") || strings.Contains(out, "main.go:7:") {
		t.Errorf("comment/string mention wrongly reported as a reference:\n%s", out)
	}
	if !strings.Contains(out, "syntactic match") {
		t.Errorf("fallback references must carry a tier caveat:\n%s", out)
	}
}

// TestTierImplementationsNoSyntacticEquivalent proves find_implementations does
// not fall back to tree-sitter (there is no syntactic interface-resolution) and
// surfaces the LSP degradation so the model uses grep.
func TestTierImplementationsNoSyntacticEquivalent(t *testing.T) {
	files := map[string]string{"main.go": "package main\n\ntype I interface{ M() }\n"}
	lspNav := &countingLSP{err: errors.New("language server \"gopls\" is not installed (not found in PATH) — fall back to grep")}
	c, _ := newTieredCodeNav(t, files, lspNav)

	_, err := runTool(t, toolByName(t, c, "find_implementations"),
		codeNavArgs{File: "main.go", Line: 3, Symbol: "I"})
	if err == nil || !strings.Contains(err.Error(), "fall back to grep") {
		t.Fatalf("implementations must surface the LSP degradation, got %v", err)
	}
}

// TestShouldFallBack tabulates the fallback decision for the LSP error messages
// the manager actually produces.
func TestShouldFallBack(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"", false},
		{"language server \"gopls\" is not installed (not found in PATH) — fall back to grep", true},
		{"no language server is configured for \".rb\" files", true},
		{"lsp: gopls crashed repeatedly and was disabled for this run — fall back to grep", true},
		{"lsp: gopls does not support textDocument/implementation", true},
		{"lsp: manager is closed", true},
		{"textDocument/definition timed out after 60s — the language server may still be indexing; fall back to grep", false},
		{"some transient JSON-RPC error", false},
	}
	for _, c := range cases {
		var err error
		if c.msg != "" {
			err = errors.New(c.msg)
		}
		if got := shouldFallBack(err); got != c.want {
			t.Errorf("shouldFallBack(%q) = %v, want %v", c.msg, got, c.want)
		}
	}
}
