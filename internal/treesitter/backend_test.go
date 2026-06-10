package treesitter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/lsp"
)

// writeRepo materializes files in a temp dir and returns the (symlink-resolved)
// root so location paths match what the backend reports.
func writeRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// locLines maps a Result's locations to "relpath:line" strings (1-based line)
// for compact assertions.
func locLines(t *testing.T, root string, res Result) []string {
	t.Helper()
	var out []string
	for _, l := range res.Locations {
		p, ok := lsp.PathFromURI(l.URI)
		if !ok {
			t.Fatalf("non-file URI %q", l.URI)
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, filepath.ToSlash(rel)+":"+itoa(l.Range.Start.Line+1))
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}

func TestDefinitionFuncMethodType(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"greeter.go": "package main\n" +
			"\n" +
			"type Greeter struct{ Name string }\n" + // line 3
			"\n" +
			"func (g Greeter) Speak() string { return g.Name }\n" + // line 5
			"\n" +
			"func Hello() string { return \"hi\" }\n", // line 7
	})
	b := New(root)
	abs := filepath.Join(root, "greeter.go")

	cases := []struct {
		symbol string
		line   int
	}{
		{"Hello", 7},
		{"Greeter", 3},
		{"Speak", 5},
	}
	for _, c := range cases {
		res, err := b.Definition(abs, c.symbol)
		if err != nil {
			t.Fatalf("Definition(%s): %v", c.symbol, err)
		}
		want := "greeter.go:" + itoa(c.line)
		got := locLines(t, root, res)
		if !contains(got, want) {
			t.Errorf("Definition(%s) = %v, want to contain %q", c.symbol, got, want)
		}
	}
}

// TestReferencesExcludeCommentAndString is the load-bearing test: the symbol
// name appears in a comment and in a string literal, neither of which must be
// reported as a reference. Only the two real call sites count. This is the core
// advantage over grep.
func TestReferencesExcludeCommentAndString(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"main.go": "package main\n" + // 1
			"\n" + // 2
			"// Greet is great. This comment mentions Greet and must not count.\n" + // 3
			"func Greet(name string) string {\n" + // 4
			"\tmsg := \"please Greet the user\" // Greet in a string, also not a ref\n" + // 5
			"\t_ = msg\n" + // 6
			"\treturn name\n" + // 7
			"}\n" + // 8
			"\n" + // 9
			"func caller() {\n" + // 10
			"\tGreet(\"a\")\n" + // 11
			"\tGreet(\"b\")\n" + // 12
			"}\n", // 13
	})
	b := New(root)
	abs := filepath.Join(root, "main.go")

	res, err := b.References(abs, "Greet")
	if err != nil {
		t.Fatalf("References: %v", err)
	}
	got := locLines(t, root, res)
	if len(got) != 2 {
		t.Fatalf("References(Greet) = %v, want exactly 2 (the call sites only)", got)
	}
	for _, want := range []string{"main.go:11", "main.go:12"} {
		if !contains(got, want) {
			t.Errorf("missing call site %q in %v", want, got)
		}
	}
	// The declaration (line 4), the comment (line 3), and the string (line 5)
	// must all be absent.
	for _, unwanted := range []string{"main.go:3", "main.go:4", "main.go:5"} {
		if contains(got, unwanted) {
			t.Errorf("reference set %v wrongly includes %q (comment/string/decl)", got, unwanted)
		}
	}
}

// TestDefinitionAmbiguousRanking covers the ambiguous-name case: two functions
// named New in different files. The candidate in the query file must rank
// first, and the result must be flagged ambiguous.
func TestDefinitionAmbiguousRanking(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"a/svc.go":   "package a\n\nfunc New() int { return 1 }\n",           // far
		"b/here.go":  "package b\n\nfunc New() int { return 2 }\n",           // same file
		"b/use.go":   "package b\n\nvar _ = New()\n",                         // query site
		"c/other.go": "package c\n\nfunc New() int { return 3 }\n// padding", // far
	})
	b := New(root)
	// Query from b/here.go where one definition lives, plus the call in b/use.go.
	// Use the definition file itself as the query origin to prove same-file wins.
	abs := filepath.Join(root, "b/here.go")

	res, err := b.Definition(abs, "New")
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if !res.Ambiguous {
		t.Errorf("expected ambiguous result for 3 same-named defs, got %d candidates", res.Candidates)
	}
	if res.Candidates != 3 {
		t.Errorf("Candidates = %d, want 3", res.Candidates)
	}
	got := locLines(t, root, res)
	if len(got) == 0 || !strings.HasPrefix(got[0], "b/here.go:") {
		t.Errorf("same-file candidate must rank first, got order %v", got)
	}
	// b/use.go shares directory b with the query — its proximity should beat a/ and c/.
	// (No def in use.go, but verify path-proximity tie among a and c is deterministic.)
}

// TestDefinitionPathProximity proves path proximity ranks a sibling-directory
// definition above a distant one when neither is in the query file.
func TestDefinitionPathProximity(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"pkg/sub/query.go": "package sub\n\nvar _ = Target()\n",
		"pkg/sub/near.go":  "package sub\n\nfunc Target() int { return 1 }\n",
		"other/far.go":     "package other\n\nfunc Target() int { return 2 }\n",
	})
	b := New(root)
	abs := filepath.Join(root, "pkg/sub/query.go")

	res, err := b.Definition(abs, "Target")
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	got := locLines(t, root, res)
	if len(got) != 2 {
		t.Fatalf("got %v, want 2 candidates", got)
	}
	if !strings.HasPrefix(got[0], "pkg/sub/near.go:") {
		t.Errorf("nearest-by-path candidate must rank first, got order %v", got)
	}
}

func TestSupportsAndUnsupported(t *testing.T) {
	b := New(t.TempDir())
	if !b.Supports("/x/y.go") {
		t.Error("Go must be supported")
	}
	if !b.Supports("/x/y.py") {
		t.Error("Python must be supported")
	}
	if !b.Supports("/x/y.ts") {
		t.Error("TypeScript must be supported")
	}
	if b.Supports("/x/y.rb") {
		t.Error("Ruby is not registered and must be unsupported")
	}
	// An unsupported language yields an empty (non-error) result.
	res, err := b.Definition("/x/y.rb", "Foo")
	if err != nil {
		t.Fatalf("Definition on unsupported ext: %v", err)
	}
	if len(res.Locations) != 0 {
		t.Errorf("unsupported ext returned locations: %v", res.Locations)
	}
}

func TestPythonAndTypeScriptDefinitions(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"app.py": "class Foo:\n    def bar(self):\n        return 1\n\ndef top():\n    return Foo()\n",
		"app.ts": "interface I { m(): void }\nclass C { greet(){ return 1 } }\nfunction f(){ return new C() }\n",
	})
	b := New(root)

	pyRes, err := b.Definition(filepath.Join(root, "app.py"), "bar")
	if err != nil {
		t.Fatalf("py Definition: %v", err)
	}
	if got := locLines(t, root, pyRes); !contains(got, "app.py:2") {
		t.Errorf("python method def = %v, want app.py:2", got)
	}

	tsRes, err := b.Definition(filepath.Join(root, "app.ts"), "greet")
	if err != nil {
		t.Fatalf("ts Definition: %v", err)
	}
	if got := locLines(t, root, tsRes); !contains(got, "app.ts:2") {
		t.Errorf("ts method def = %v, want app.ts:2", got)
	}
}
