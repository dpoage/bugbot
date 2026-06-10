//go:build integration

// LSP integration tests exercise the code-navigation tools against real
// language servers. Run with:
//
//	go test -tags integration ./internal/agent/...
//
// They skip automatically when the server binary (gopls, clangd) is not
// installed. Install gopls with: go install golang.org/x/tools/gopls@latest
// (it lands in ~/go/bin, which these tests add to PATH when present).
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// requireServer skips the test unless the named language server binary is
// available, extending PATH with ~/go/bin first so a fresh `go install` of
// gopls is found without shell profile changes.
func requireServer(t *testing.T, binary string) {
	t.Helper()
	if home, err := os.UserHomeDir(); err == nil {
		gobin := filepath.Join(home, "go", "bin")
		path := os.Getenv("PATH")
		if !strings.Contains(path, gobin) {
			t.Setenv("PATH", gobin+string(os.PathListSeparator)+path)
		}
	}
	if _, err := exec.LookPath(binary); err != nil {
		t.Skipf("%s not installed; skipping LSP integration test", binary)
	}
}

// writeFixture materializes files into a temp dir and returns its
// symlink-resolved path (so rendered repo-relative paths are stable).
func writeFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
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
	return dir
}

// lineOf returns the 1-based line number of the first line containing needle.
func lineOf(t *testing.T, content, needle string) int {
	t.Helper()
	for i, line := range strings.Split(content, "\n") {
		if strings.Contains(line, needle) {
			return i + 1
		}
	}
	t.Fatalf("fixture does not contain %q", needle)
	return 0
}

func runNav(t *testing.T, tool Tool, file string, line int, symbol string) string {
	t.Helper()
	raw, err := json.Marshal(codeNavArgs{File: file, Line: line, Symbol: symbol})
	if err != nil {
		t.Fatal(err)
	}
	out, err := tool.Run(context.Background(), raw)
	if err != nil {
		t.Fatalf("%s(%s:%d %q): %v", tool.Def().Name, file, line, symbol, err)
	}
	return out
}

const goGreeter = `package navfix

// Greeter is implemented by anything that can greet.
type Greeter interface {
	Greet() string
}

// Hello returns a greeting.
func Hello() string {
	return "hello"
}
`

const goUse = `package navfix

type Dog struct{}

func (Dog) Greet() string { return "woof" }

func UseHello() string {
	return Hello()
}

func UseHelloUnicode() string {
	x := "héllo🎉中" + Hello()
	return x
}

func Announce(g Greeter) string {
	return g.Greet() + "!"
}
`

func TestIntegrationGopls(t *testing.T) {
	requireServer(t, "gopls")

	dir := writeFixture(t, map[string]string{
		"go.mod":     "module example.com/navfix\n\ngo 1.22\n",
		"greeter.go": goGreeter,
		"use.go":     goUse,
	})
	nav, err := NewCodeNav(dir)
	if err != nil {
		t.Fatalf("NewCodeNav: %v", err)
	}
	t.Cleanup(func() { _ = nav.Close() })

	defTool := toolByName(t, nav, "find_definition")
	refTool := toolByName(t, nav, "find_references")
	implTool := toolByName(t, nav, "find_implementations")

	helloDefLine := lineOf(t, goGreeter, "func Hello()")
	helloCallLine := lineOf(t, goUse, "return Hello()")
	unicodeCallLine := lineOf(t, goUse, `"héllo🎉中" + Hello()`)
	greeterLine := lineOf(t, goGreeter, "type Greeter interface")
	dogLine := lineOf(t, goUse, "type Dog struct")

	t.Run("definition of function from call site", func(t *testing.T) {
		out := runNav(t, defTool, "use.go", helloCallLine, "Hello")
		want := fmt.Sprintf("greeter.go:%d:", helloDefLine)
		if !strings.Contains(out, want) {
			t.Errorf("definition output missing %q:\n%s", want, out)
		}
	})

	t.Run("references finds callers excluding declaration", func(t *testing.T) {
		out := runNav(t, refTool, "greeter.go", helloDefLine, "Hello")
		for _, want := range []string{
			fmt.Sprintf("use.go:%d:", helloCallLine),
			fmt.Sprintf("use.go:%d:", unicodeCallLine),
		} {
			if !strings.Contains(out, want) {
				t.Errorf("references output missing caller %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, fmt.Sprintf("greeter.go:%d:", helloDefLine)) {
			t.Errorf("references must exclude the declaration:\n%s", out)
		}
	})

	t.Run("implementations of interface", func(t *testing.T) {
		out := runNav(t, implTool, "greeter.go", greeterLine, "Greeter")
		if !strings.Contains(out, fmt.Sprintf("use.go:%d:", dogLine)) {
			t.Errorf("implementations output missing Dog at use.go:%d:\n%s", dogLine, out)
		}
	})

	t.Run("definition with non-ASCII before symbol (UTF-16 positions)", func(t *testing.T) {
		// Emoji + CJK before the symbol: byte, rune, and UTF-16 offsets all
		// differ on this line, so a unit mix-up would point gopls at the
		// wrong token.
		out := runNav(t, defTool, "use.go", unicodeCallLine, "Hello")
		want := fmt.Sprintf("greeter.go:%d:", helloDefLine)
		if !strings.Contains(out, want) {
			t.Errorf("unicode-line definition missing %q:\n%s", want, out)
		}
	})

	t.Run("method callers via references", func(t *testing.T) {
		// References of the interface method must surface the call made
		// through the interface — exactly the "who actually calls this?"
		// question a refuter needs answered.
		greetDeclLine := lineOf(t, goGreeter, "Greet() string")
		greetCallLine := lineOf(t, goUse, "g.Greet()")
		out := runNav(t, refTool, "greeter.go", greetDeclLine, "Greet")
		if !strings.Contains(out, fmt.Sprintf("use.go:%d:", greetCallLine)) {
			t.Errorf("expected interface-typed call at use.go:%d in references:\n%s", greetCallLine, out)
		}
	})
}

const cFixture = `int add(int a, int b) { return a + b; }

int twice(int x) { return add(x, x); }

int thrice(int x) { return add(add(x, x), x); }
`

func TestIntegrationClangd(t *testing.T) {
	requireServer(t, "clangd")

	dir := writeFixture(t, map[string]string{"math.c": cFixture})
	nav, err := NewCodeNav(dir)
	if err != nil {
		t.Fatalf("NewCodeNav: %v", err)
	}
	t.Cleanup(func() { _ = nav.Close() })

	addDefLine := lineOf(t, cFixture, "int add(")
	twiceLine := lineOf(t, cFixture, "int twice(")
	thriceLine := lineOf(t, cFixture, "int thrice(")

	t.Run("definition", func(t *testing.T) {
		out := runNav(t, toolByName(t, nav, "find_definition"), "math.c", twiceLine, "add")
		if !strings.Contains(out, fmt.Sprintf("math.c:%d:", addDefLine)) {
			t.Errorf("clangd definition output:\n%s", out)
		}
	})

	t.Run("references", func(t *testing.T) {
		out := runNav(t, toolByName(t, nav, "find_references"), "math.c", addDefLine, "add")
		for _, want := range []string{
			fmt.Sprintf("math.c:%d:", twiceLine),
			fmt.Sprintf("math.c:%d:", thriceLine),
		} {
			if !strings.Contains(out, want) {
				t.Errorf("clangd references missing %q:\n%s", want, out)
			}
		}
	})
}
