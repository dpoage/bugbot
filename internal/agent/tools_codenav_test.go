package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/lsp"
)

// fakeNavigator scripts lsp.Manager responses and records the positions it was
// asked about.
type fakeNavigator struct {
	locs   []lsp.Location
	err    error
	gotPos lsp.Position
	gotAbs string
	closed bool
}

func (f *fakeNavigator) Definition(_ context.Context, path string, pos lsp.Position) ([]lsp.Location, error) {
	f.gotAbs, f.gotPos = path, pos
	return f.locs, f.err
}

func (f *fakeNavigator) References(_ context.Context, path string, pos lsp.Position) ([]lsp.Location, error) {
	f.gotAbs, f.gotPos = path, pos
	return f.locs, f.err
}

func (f *fakeNavigator) Implementation(_ context.Context, path string, pos lsp.Position) ([]lsp.Location, error) {
	f.gotAbs, f.gotPos = path, pos
	return f.locs, f.err
}

func (f *fakeNavigator) Close() error {
	f.closed = true
	return nil
}

// newTestCodeNav builds a CodeNav over a temp repo with the given files and a
// scripted navigator.
func newTestCodeNav(t *testing.T, files map[string]string, nav navigator) (*CodeNav, string) {
	t.Helper()
	dir := t.TempDir()
	// Resolve symlinks so position assertions match fsRoot's canonical root
	// (macOS /tmp and friends).
	resolved, err := filepath.EvalSymlinks(dir)
	if err == nil {
		dir = resolved
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c, err := NewCodeNav(dir)
	if err != nil {
		t.Fatalf("NewCodeNav: %v", err)
	}
	if nav != nil {
		_ = c.nav.Close()
		c.nav = nav
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, dir
}

// toolByName fetches one of the bundle's tools.
func toolByName(t *testing.T, c *CodeNav, name string) Tool {
	t.Helper()
	for _, tool := range c.Tools() {
		if tool.Def().Name == name {
			return tool
		}
	}
	t.Fatalf("tool %q not found", name)
	return nil
}

func runTool(t *testing.T, tool Tool, args any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return tool.Run(context.Background(), raw)
}

func TestCodeNavToolDefs(t *testing.T) {
	c, _ := newTestCodeNav(t, nil, &fakeNavigator{})
	tools := c.Tools()
	if len(tools) != 3 {
		t.Fatalf("got %d tools, want 3", len(tools))
	}
	want := map[string]bool{"find_definition": true, "find_references": true, "find_implementations": true}
	for _, tool := range tools {
		def := tool.Def()
		if !want[def.Name] {
			t.Errorf("unexpected tool %q", def.Name)
		}
		if def.Description == "" || len(def.Parameters) == 0 {
			t.Errorf("tool %q missing description or parameters", def.Name)
		}
		var schema map[string]any
		if err := json.Unmarshal(def.Parameters, &schema); err != nil {
			t.Errorf("tool %q parameters are not valid JSON: %v", def.Name, err)
		}
	}
}

func TestCodeNavPositionConversion(t *testing.T) {
	// Non-ASCII content BEFORE the symbol on the line: the LSP character
	// offset must count UTF-16 units, not bytes or runes.
	line := `	x := "héllo🎉中" + Hello()`
	fake := &fakeNavigator{}
	c, dir := newTestCodeNav(t, map[string]string{
		"main.go": "package main\n" + line + "\n",
	}, fake)

	tool := toolByName(t, c, "find_definition")
	out, err := runTool(t, tool, codeNavArgs{File: "main.go", Line: 2, Symbol: "Hello"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	_ = out

	if fake.gotAbs != filepath.Join(dir, "main.go") {
		t.Errorf("queried path = %q", fake.gotAbs)
	}
	if fake.gotPos.Line != 1 {
		t.Errorf("queried line = %d, want 1 (0-based)", fake.gotPos.Line)
	}
	byteOff := strings.Index(line, "Hello")
	wantChar := lsp.UTF16Col(line, byteOff)
	if fake.gotPos.Character != wantChar {
		t.Errorf("queried character = %d, want %d (UTF-16)", fake.gotPos.Character, wantChar)
	}
	// Sanity: bytes and UTF-16 must actually differ on this line, or the test
	// proves nothing.
	if byteOff == wantChar {
		t.Fatalf("fixture defect: byte offset %d equals UTF-16 offset", byteOff)
	}
}

func TestCodeNavRendersRepoRelativeResults(t *testing.T) {
	files := map[string]string{
		"a/def.go": "package a\n\nfunc Hello() {}\n",
		"b/use.go": "package b\n\nvar _ = Hello\n",
	}
	c, dir := newTestCodeNav(t, files, nil)
	fake := &fakeNavigator{locs: []lsp.Location{
		{URI: lsp.URIFromPath(filepath.Join(dir, "a/def.go")), Range: lsp.Range{Start: lsp.Position{Line: 2, Character: 5}}},
		{URI: lsp.URIFromPath("/usr/lib/go/src/fmt/print.go"), Range: lsp.Range{Start: lsp.Position{Line: 99}}},
	}}
	_ = c.nav.Close()
	c.nav = fake

	tool := toolByName(t, c, "find_references")
	out, err := runTool(t, tool, codeNavArgs{File: "b/use.go", Line: 3, Symbol: "Hello"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "a/def.go:3:func Hello() {}") {
		t.Errorf("missing repo-relative result with snippet:\n%s", out)
	}
	if !strings.Contains(out, "outside repository") {
		t.Errorf("external location not labeled:\n%s", out)
	}
	if strings.Contains(out, "print.go:100:package") || strings.Contains(out, "func Fprintln") {
		t.Errorf("external file content must not be excerpted:\n%s", out)
	}
}

func TestCodeNavSymlinkedResultNotExcerpted(t *testing.T) {
	// A symlink inside the repo pointing outside it makes a result path that
	// passes lexical containment; the renderer must still refuse to excerpt the
	// resolved-external content.
	files := map[string]string{"main.go": "package main\n\nfunc Hello() {}\n"}
	c, dir := newTestCodeNav(t, files, nil)

	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.go")
	if err := os.WriteFile(secret, []byte("package secret // CANARY-DO-NOT-LEAK\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "vendor-link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	fake := &fakeNavigator{locs: []lsp.Location{{
		URI:   lsp.URIFromPath(filepath.Join(dir, "vendor-link", "secret.go")),
		Range: lsp.Range{Start: lsp.Position{Line: 0}},
	}}}
	_ = c.nav.Close()
	c.nav = fake

	out, err := runTool(t, toolByName(t, c, "find_references"), codeNavArgs{File: "main.go", Line: 3, Symbol: "Hello"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(out, "CANARY-DO-NOT-LEAK") {
		t.Errorf("symlink-escaped file content was excerpted:\n%s", out)
	}
	if !strings.Contains(out, "outside repository") {
		t.Errorf("symlink-escaped location not labeled as outside:\n%s", out)
	}
}

func TestCodeNavEmptyResults(t *testing.T) {
	files := map[string]string{"main.go": "package main\n\nfunc Hello() {}\n"}
	c, _ := newTestCodeNav(t, files, &fakeNavigator{})

	for name, want := range map[string]string{
		"find_definition":      "no definition",
		"find_references":      "no references",
		"find_implementations": "no implementations",
	} {
		out, err := runTool(t, toolByName(t, c, name), codeNavArgs{File: "main.go", Line: 3, Symbol: "Hello"})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(out, want) {
			t.Errorf("%s empty output = %q, want mention of %q", name, out, want)
		}
	}
}

func TestCodeNavArgumentErrors(t *testing.T) {
	files := map[string]string{"main.go": "package main\n\nfunc Hello() {}\n"}
	c, _ := newTestCodeNav(t, files, &fakeNavigator{})
	tool := toolByName(t, c, "find_definition")

	tests := []struct {
		name string
		args codeNavArgs
		want string
	}{
		{"missing file", codeNavArgs{Line: 1, Symbol: "x"}, "file is required"},
		{"bad line", codeNavArgs{File: "main.go", Line: 0, Symbol: "x"}, "1-based"},
		{"missing symbol", codeNavArgs{File: "main.go", Line: 1}, "symbol is required"},
		{"escape", codeNavArgs{File: "../etc/passwd", Line: 1, Symbol: "x"}, "escapes"},
		{"absolute path", codeNavArgs{File: "/etc/passwd", Line: 1, Symbol: "x"}, "absolute"},
		{"no such file", codeNavArgs{File: "nope.go", Line: 1, Symbol: "x"}, "cannot read"},
		{"line past EOF", codeNavArgs{File: "main.go", Line: 99, Symbol: "Hello"}, "past the end"},
		{"symbol not on line", codeNavArgs{File: "main.go", Line: 1, Symbol: "Hello"}, "not found on the line"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := runTool(t, tool, tt.args)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestCodeNavServerErrorPropagates(t *testing.T) {
	files := map[string]string{"main.go": "package main\n\nfunc Hello() {}\n"}
	fake := &fakeNavigator{err: errors.New("language server \"gopls\" is not installed (not found in PATH) — fall back to grep")}
	c, _ := newTestCodeNav(t, files, fake)

	_, err := runTool(t, toolByName(t, c, "find_references"), codeNavArgs{File: "main.go", Line: 3, Symbol: "Hello"})
	if err == nil || !strings.Contains(err.Error(), "fall back to grep") {
		t.Errorf("expected degradation error to surface to the model, got %v", err)
	}
}

func TestCodeNavTruncatesAndDedupes(t *testing.T) {
	files := map[string]string{"main.go": "package main\n\nfunc Hello() {}\n"}
	c, dir := newTestCodeNav(t, files, nil)

	var locs []lsp.Location
	// A duplicated first location plus enough distinct ones to cross the cap.
	for i := 0; i < codeNavMaxResults+50; i++ {
		locs = append(locs, lsp.Location{
			URI:   lsp.URIFromPath(filepath.Join(dir, "main.go")),
			Range: lsp.Range{Start: lsp.Position{Line: i}},
		})
	}
	locs = append([]lsp.Location{locs[0]}, locs...)
	fake := &fakeNavigator{locs: locs}
	_ = c.nav.Close()
	c.nav = fake

	out, err := runTool(t, toolByName(t, c, "find_references"), codeNavArgs{File: "main.go", Line: 3, Symbol: "Hello"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "truncated") {
		t.Errorf("expected truncation note:\n%s", out[:200])
	}
	if n := strings.Count(out, "main.go:1:"); n != 1 {
		t.Errorf("duplicate location rendered %d times, want 1", n)
	}
}

func TestSymbolColumn(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		symbol  string
		want    int
		wantErr bool
	}{
		{"plain", "x := Hello()", "Hello", 5, false},
		{"trailing parens stripped", "x := Hello()", "Hello()", 5, false},
		{"qualified points at segment", "v := pkg.Hello(1)", "pkg.Hello", 9, false},
		{"qualified falls back to segment", "v := other.Hello(1)", "pkg.Hello", 11, false},
		{"skips longer identifier", "sayHello := Hello", "Hello", 12, false},
		{"first standalone occurrence", "Hello = Hello + 1", "Hello", 0, false},
		{"absent", "x := World()", "Hello", 0, true},
		{"substring only", "sayHelloAgain()", "Hello", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := symbolColumn(tt.line, tt.symbol)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Errorf("symbolColumn(%q, %q) = %d, want %d", tt.line, tt.symbol, got, tt.want)
			}
		})
	}
}

func TestCodeNavCloseIsIdempotent(t *testing.T) {
	fake := &fakeNavigator{}
	c, _ := newTestCodeNav(t, nil, fake)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !fake.closed {
		t.Error("Close did not reach the navigator")
	}
	if err := c.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// Ensure the real constructor wires an actual manager (compile-time and
// runtime sanity; no server is spawned).
func TestNewCodeNavRealManager(t *testing.T) {
	dir := t.TempDir()
	c, err := NewCodeNav(dir)
	if err != nil {
		t.Fatalf("NewCodeNav: %v", err)
	}
	if c.nav == nil {
		t.Fatal("nil navigator")
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}
