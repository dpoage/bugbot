package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/lsp"
)

// TestReadSymbolExactBodyGo is the happy path: a Go file with two functions.
// read_symbol must return ONLY the requested function's body, numbered with the
// file's real (absolute) line numbers, and not bleed into the neighboring
// function.
func TestReadSymbolExactBodyGo(t *testing.T) {
	src := "package main\n" + // 1
		"\n" + // 2
		"func Alpha() int {\n" + // 3
		"\tx := 1\n" + // 4
		"\treturn x\n" + // 5
		"}\n" + // 6
		"\n" + // 7
		"func Beta() int {\n" + // 8
		"\treturn 2\n" + // 9
		"}\n" // 10
	c, _ := newTestCodeNav(t, map[string]string{"main.go": src}, nil)
	tool := toolByName(t, c, "read_symbol")

	out, err := runTool(t, tool, codeNavArgs{File: "main.go", Line: 3, Symbol: "Alpha"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Header naming the located span.
	if !strings.Contains(out, "main.go:3-6") {
		t.Errorf("missing span header for Alpha:\n%s", out)
	}
	// Body lines, numbered with real line numbers.
	for _, want := range []string{"3\tfunc Alpha() int {", "4\t\tx := 1", "5\t\treturn x", "6\t}"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing body line %q:\n%s", want, out)
		}
	}
	// Beta must NOT be rendered — read_symbol returns one declaration, not the file.
	if strings.Contains(out, "func Beta") {
		t.Errorf("read_symbol bled into the neighboring function:\n%s", out)
	}
}

// TestReadSymbolExactBodyPython confirms the exact path works for a non-Go,
// indentation-significant language: a Python def renders with full lines
// including the leading indentation that a column-based slice would drop.
func TestReadSymbolExactBodyPython(t *testing.T) {
	src := "def top():\n" + // 1
		"    x = 1\n" + // 2
		"    return x\n" // 3
	c, _ := newTestCodeNav(t, map[string]string{"app.py": src}, nil)
	tool := toolByName(t, c, "read_symbol")

	out, err := runTool(t, tool, codeNavArgs{File: "app.py", Line: 1, Symbol: "top"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "app.py:1-3") {
		t.Errorf("missing span header:\n%s", out)
	}
	if !strings.Contains(out, "2\t    x = 1") {
		t.Errorf("indented body line lost:\n%s", out)
	}
}

// TestReadSymbolDefinitionInOtherFile proves the common case the tool exists
// for: the symbol occurrence (the args) is in one file, but its definition lives
// in another — read_symbol renders from the definition's file.
func TestReadSymbolDefinitionInOtherFile(t *testing.T) {
	files := map[string]string{
		"use/caller.go": "package use\n\nfunc run() int { return Target() }\n",
		"def/target.go": "package def\n\nfunc Target() int {\n\treturn 42\n}\n",
	}
	c, _ := newTestCodeNav(t, files, nil)
	tool := toolByName(t, c, "read_symbol")

	out, err := runTool(t, tool, codeNavArgs{File: "use/caller.go", Line: 3, Symbol: "Target"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "def/target.go:3-5") {
		t.Errorf("definition not rendered from its own file:\n%s", out)
	}
	if !strings.Contains(out, "4\t\treturn 42") {
		t.Errorf("definition body missing:\n%s", out)
	}
}

// TestReadSymbolAmbiguous renders the best (proximity-ranked) match's body and
// lists the other same-named candidate locations so the model can re-query.
func TestReadSymbolAmbiguous(t *testing.T) {
	files := map[string]string{
		"a/here.go":  "package a\n\nfunc New() int {\n\treturn 1\n}\n",
		"a/use.go":   "package a\n\nvar _ = New()\n",
		"b/other.go": "package b\n\nfunc New() int {\n\treturn 2\n}\n",
	}
	c, _ := newTestCodeNav(t, files, nil)
	tool := toolByName(t, c, "read_symbol")

	// Query from a/use.go: the same-directory a/here.go must rank first.
	out, err := runTool(t, tool, codeNavArgs{File: "a/use.go", Line: 3, Symbol: "New"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "a/here.go:3-5") {
		t.Errorf("nearest candidate's body not rendered first:\n%s", out)
	}
	if !strings.Contains(out, "Other candidates") || !strings.Contains(out, "b/other.go:3") {
		t.Errorf("alternate candidate not listed:\n%s", out)
	}
}

// TestReadSymbolNotFound returns an actionable ERROR when the name has no
// definition in the repository.
func TestReadSymbolNotFound(t *testing.T) {
	c, _ := newTestCodeNav(t, map[string]string{"main.go": "package main\n\nvar _ = 1\n"}, nil)
	tool := toolByName(t, c, "read_symbol")

	out, err := runTool(t, tool, codeNavArgs{File: "main.go", Line: 3, Symbol: "Nonexistent"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.HasPrefix(out, "ERROR:") {
		t.Errorf("want ERROR result, got:\n%s", out)
	}
	if !strings.Contains(out, "grep") && !strings.Contains(out, "find_definition") {
		t.Errorf("ERROR should suggest a fallback:\n%s", out)
	}
}

// TestReadSymbolTruncates caps a pathologically large body and points the model
// at read_file to continue.
func TestReadSymbolTruncates(t *testing.T) {
	var b strings.Builder
	b.WriteString("package main\n\nfunc Big() {\n")
	for i := 0; i < readSymbolMaxLines+100; i++ {
		b.WriteString("\t_ = 1\n")
	}
	b.WriteString("}\n")
	c, _ := newTestCodeNav(t, map[string]string{"big.go": b.String()}, nil)
	tool := toolByName(t, c, "read_symbol")

	out, err := runTool(t, tool, codeNavArgs{File: "big.go", Line: 3, Symbol: "Big"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "truncated") || !strings.Contains(out, "read_file with offset=") {
		t.Errorf("expected actionable truncation note:\n%s", lastLines(out, 3))
	}
	// The rendered body must not exceed the line cap (plus header/notes).
	if n := strings.Count(out, "\t_ = 1"); n > readSymbolMaxLines {
		t.Errorf("rendered %d body lines, want <= cap %d", n, readSymbolMaxLines)
	}
}

// TestReadSymbolUnsupportedLanguageWindow covers the fallback path: a file whose
// language has no tree-sitter grammar resolves the definition via the navigator
// and renders a positional WINDOW with an explicit caveat.
func TestReadSymbolUnsupportedLanguageWindow(t *testing.T) {
	// .rb is not a registered grammar, so read_symbol must take the navigator
	// (fake) path and emit a positional-window caveat.
	files := map[string]string{
		"app.rb": "def greet\n  puts 'hi'\n  puts 'bye'\nend\n",
	}
	c, dir := newTestCodeNav(t, files, nil)
	fake := &fakeNavigator{locs: []lsp.Location{{
		URI:   lsp.URIFromPath(filepath.Join(dir, "app.rb")),
		Range: lsp.Range{Start: lsp.Position{Line: 0}},
	}}}
	_ = c.nav.Close()
	c.nav = fake

	tool := toolByName(t, c, "read_symbol")
	out, err := runTool(t, tool, codeNavArgs{File: "app.rb", Line: 1, Symbol: "greet"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "positional window") {
		t.Errorf("missing positional-window caveat:\n%s", out)
	}
	if !strings.Contains(out, "app.rb:1") {
		t.Errorf("definition location not reported:\n%s", out)
	}
	if !strings.Contains(out, "1\tdef greet") {
		t.Errorf("window body not rendered:\n%s", out)
	}
}

// TestReadSymbolOutOfRootSkipped proves a definition location resolved outside
// the repository (dependency / symlink escape) is never excerpted; the tool
// reports not-found rather than leaking external content.
func TestReadSymbolOutOfRootSkipped(t *testing.T) {
	files := map[string]string{"app.rb": "def greet\n  puts 'hi'\nend\n"}
	c, _ := newTestCodeNav(t, files, nil)

	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.rb")
	if err := os.WriteFile(secret, []byte("def greet # CANARY-DO-NOT-LEAK\nend\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := &fakeNavigator{locs: []lsp.Location{{
		URI:   lsp.URIFromPath(secret),
		Range: lsp.Range{Start: lsp.Position{Line: 0}},
	}}}
	_ = c.nav.Close()
	c.nav = fake

	out, err := runTool(t, toolByName(t, c, "read_symbol"), codeNavArgs{File: "app.rb", Line: 1, Symbol: "greet"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(out, "CANARY-DO-NOT-LEAK") {
		t.Errorf("out-of-root definition was excerpted:\n%s", out)
	}
	if !strings.HasPrefix(out, "ERROR:") {
		t.Errorf("out-of-root-only result should be a not-found ERROR:\n%s", out)
	}
}

// TestReadSymbolArgumentErrors mirrors the find_definition arg validation: the
// shared schema must reject the same malformed inputs.
func TestReadSymbolArgumentErrors(t *testing.T) {
	c, _ := newTestCodeNav(t, map[string]string{"main.go": "package main\n\nfunc Hello() {}\n"}, &fakeNavigator{})
	tool := toolByName(t, c, "read_symbol")

	tests := []struct {
		name string
		args codeNavArgs
		want string
	}{
		{"missing file", codeNavArgs{Line: 1, Symbol: "x"}, "file is required"},
		{"bad line", codeNavArgs{File: "main.go", Line: 0, Symbol: "x"}, "1-based"},
		{"missing symbol", codeNavArgs{File: "main.go", Line: 1}, "symbol is required"},
		{"escape", codeNavArgs{File: "../etc/passwd", Line: 1, Symbol: "x"}, "escapes"},
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

// lastLines returns the final n lines of s for compact failure messages.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
