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

// TestTSXJSXResolves is the regression test for the .tsx-parsed-as-typescript
// bug: a .tsx file contains JSX (`<Panel/>`), which fails to parse under the
// plain TypeScript grammar, silently dropping every symbol. With a distinct tsx
// grammar entry, the function definition and its call must both resolve.
func TestTSXJSXResolves(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"App.tsx": "function Panel(): JSX.Element {\n" + // 1: definition
			"  return <div className=\"p\">{title()}</div>;\n" + // 2: ref to title
			"}\n" + // 3
			"function title(): string { return \"hi\" }\n" + // 4: definition
			"function App(): JSX.Element {\n" + // 5
			"  return <Panel />;\n" + // 6: JSX use of Panel (not a call ref)
			"}\n", // 7
	})
	b := New(root)
	abs := filepath.Join(root, "App.tsx")

	// The definition of Panel must resolve — proving the JSX file parsed at all.
	defRes, err := b.Definition(abs, "Panel")
	if err != nil {
		t.Fatalf("tsx Definition(Panel): %v", err)
	}
	if got := locLines(t, root, defRes); !contains(got, "App.tsx:1") {
		t.Fatalf("tsx Panel def = %v, want App.tsx:1 (JSX file failed to parse?)", got)
	}

	// The call site title() inside JSX-bearing source resolves as a reference.
	refRes, err := b.References(abs, "title")
	if err != nil {
		t.Fatalf("tsx References(title): %v", err)
	}
	if got := locLines(t, root, refRes); !contains(got, "App.tsx:2") {
		t.Errorf("tsx title ref = %v, want App.tsx:2", got)
	}
}

// bodyLineSpan returns the 1-based [startLine, endLine] of the single body
// location for a symbol, failing if there is not exactly one match. It is the
// assertion helper for DefinitionBodies: a body range must cover the
// declaration from its first to its last source line.
func bodyLineSpan(t *testing.T, b *Backend, abs, symbol string) (int, int) {
	t.Helper()
	res, err := b.DefinitionBodies(abs, symbol)
	if err != nil {
		t.Fatalf("DefinitionBodies(%s): %v", symbol, err)
	}
	if len(res.Locations) != 1 {
		t.Fatalf("DefinitionBodies(%s): got %d locations, want 1", symbol, len(res.Locations))
	}
	l := res.Locations[0]
	return l.Range.Start.Line + 1, l.Range.End.Line + 1
}

// TestDefinitionBodiesSpansWholeDeclaration is the load-bearing test for the
// read_symbol tool: a body lookup must return a range covering the declaration
// from its first to its last line (so the tool can render the full function /
// method / type / def body), across every supported language. It also confirms
// DefinitionBodies does NOT collapse to the single name line the way Definition
// does.
func TestDefinitionBodiesSpansWholeDeclaration(t *testing.T) {
	root := writeRepo(t, map[string]string{
		// Go: a multi-line func, a multi-line method, a multi-line type.
		"greeter.go": "package main\n" + // 1
			"\n" + // 2
			"type Greeter struct {\n" + // 3  type start
			"\tName string\n" + // 4
			"}\n" + // 5  type end
			"\n" + // 6
			"func (g Greeter) Speak() string {\n" + // 7  method start
			"\treturn g.Name\n" + // 8
			"}\n" + // 9  method end
			"\n" + // 10
			"func Hello() string {\n" + // 11 func start
			"\tx := \"hi\"\n" + // 12
			"\treturn x\n" + // 13
			"}\n", // 14 func end
		// Python: a multi-line def.
		"app.py": "def top():\n" + // 1 def start
			"    x = 1\n" + // 2
			"    return x\n", // 3 def end
		// TypeScript: a multi-line function.
		"app.ts": "function f() {\n" + // 1 func start
			"  const c = 1\n" + // 2
			"  return c\n" + // 3
			"}\n", // 4 func end
	})
	b := New(root)

	cases := []struct {
		file, symbol       string
		wantStart, wantEnd int
	}{
		{"greeter.go", "Hello", 11, 14},
		{"greeter.go", "Speak", 7, 9},
		{"greeter.go", "Greeter", 3, 5},
		{"app.py", "top", 1, 3},
		{"app.ts", "f", 1, 4},
	}
	for _, c := range cases {
		gotStart, gotEnd := bodyLineSpan(t, b, filepath.Join(root, c.file), c.symbol)
		if gotStart != c.wantStart || gotEnd != c.wantEnd {
			t.Errorf("DefinitionBodies(%s, %s) span = %d-%d, want %d-%d",
				c.file, c.symbol, gotStart, gotEnd, c.wantStart, c.wantEnd)
		}
	}
}

// TestDefinitionBodiesNameVsBodyRange proves DefinitionBodies and Definition
// agree on WHICH declaration is found but differ in the RANGE they report:
// Definition returns the name line, DefinitionBodies the full body span. This is
// the invariant find_definition rendering relies on (it must keep name ranges).
func TestDefinitionBodiesNameVsBodyRange(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"greeter.go": "package main\n\nfunc Hello() string {\n\treturn \"hi\"\n}\n",
	})
	b := New(root)
	abs := filepath.Join(root, "greeter.go")

	nameRes, err := b.Definition(abs, "Hello")
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	bodyRes, err := b.DefinitionBodies(abs, "Hello")
	if err != nil {
		t.Fatalf("DefinitionBodies: %v", err)
	}
	if len(nameRes.Locations) != 1 || len(bodyRes.Locations) != 1 {
		t.Fatalf("want one location each, got name=%d body=%d", len(nameRes.Locations), len(bodyRes.Locations))
	}
	nl := nameRes.Locations[0]
	bl := bodyRes.Locations[0]
	// Both start on the declaration's first line (line 3).
	if nl.Range.Start.Line != 2 || bl.Range.Start.Line != 2 {
		t.Errorf("both should start on line 3 (0-based 2): name=%d body=%d", nl.Range.Start.Line, bl.Range.Start.Line)
	}
	// The name range ends on the same line; the body range extends past it.
	if nl.Range.End.Line != 2 {
		t.Errorf("name range should be single-line, ended on line %d", nl.Range.End.Line+1)
	}
	if bl.Range.End.Line <= nl.Range.End.Line {
		t.Errorf("body range must extend past the name line: name end=%d body end=%d", nl.Range.End.Line+1, bl.Range.End.Line+1)
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
	// JavaScript family must all be supported via tsxGrammar.
	for _, ext := range []string{".js", ".jsx", ".mjs", ".cjs"} {
		if !b.Supports("/x/y" + ext) {
			t.Errorf("JavaScript extension %s must be supported", ext)
		}
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

// TestDefinitionBodiesDecoratorExtension verifies that DefinitionBodies extends
// the body range upward to include decorator lines that precede the captured
// node. The tree-sitter @definition capture for Python def/class starts at the
// "def"/"class" keyword, excluding any preceding @decorator lines; TypeScript
// method decorators are likewise excluded from the method_definition capture.
// Without the decorator extension, @property/@app.route/@staticmethod etc.
// would silently vanish from the rendered body — a correctness defect.
//
// Cases:
//   - Python function with two decorators: decorator lines must be included
//   - Python class with one decorator: decorator line must be included
//   - TypeScript method with two decorators: decorator lines must be included
//   - Undecorated Go function: range must not change (regression guard)
func TestDefinitionBodiesDecoratorExtension(t *testing.T) {
	root := writeRepo(t, map[string]string{
		// Python: two decorators before a function (lines 1-2), function body
		// on lines 3-4. A decorated class follows (decorator line 6, class 7-8).
		"app.py": "@property\n" + // 1
			"@staticmethod\n" + // 2
			"def foo(self):\n" + // 3
			"    return 1\n" + // 4
			"\n" + // 5
			"@dataclass\n" + // 6
			"class Bar:\n" + // 7
			"    x: int = 0\n", // 8
		// TypeScript: two decorators before a method (lines 2-3), method body
		// on lines 4-6, inside a class wrapper.
		"app.ts": "class MyClass {\n" + // 1
			"  @log\n" + // 2
			"  @validate\n" + // 3
			"  greet() {\n" + // 4
			"    return 1;\n" + // 5
			"  }\n" + // 6
			"}\n", // 7
		// Go: plain undecorated function — range must be unchanged.
		"plain.go": "package main\n" + // 1
			"\n" + // 2
			"func Plain() int {\n" + // 3
			"\treturn 42\n" + // 4
			"}\n", // 5
	})
	b := New(root)

	// Python decorated function: decorators on lines 1-2 must be included.
	gotStart, gotEnd := bodyLineSpan(t, b, filepath.Join(root, "app.py"), "foo")
	if gotStart != 1 || gotEnd != 4 {
		t.Errorf("Python decorated func foo: span = %d-%d, want 1-4 (decorators included)", gotStart, gotEnd)
	}

	// Python decorated class: decorator on line 6 must be included.
	gotStart, gotEnd = bodyLineSpan(t, b, filepath.Join(root, "app.py"), "Bar")
	if gotStart != 6 || gotEnd != 8 {
		t.Errorf("Python decorated class Bar: span = %d-%d, want 6-8 (decorator included)", gotStart, gotEnd)
	}

	// TypeScript decorated method: decorators on lines 2-3 must be included.
	gotStart, gotEnd = bodyLineSpan(t, b, filepath.Join(root, "app.ts"), "greet")
	if gotStart != 2 || gotEnd != 6 {
		t.Errorf("TS decorated method greet: span = %d-%d, want 2-6 (decorators included)", gotStart, gotEnd)
	}

	// Undecorated Go function: body range must be unchanged (regression guard).
	gotStart, gotEnd = bodyLineSpan(t, b, filepath.Join(root, "plain.go"), "Plain")
	if gotStart != 3 || gotEnd != 5 {
		t.Errorf("Go undecorated func Plain: span = %d-%d, want 3-5 (no change)", gotStart, gotEnd)
	}
}

// TestJSXFileDefsAndRefs proves that a .jsx file is parsed by the tsx grammar:
// JSX syntax must not prevent symbol extraction. A function returning JSX must
// produce a definition, and a call site within JSX-bearing source must produce
// a reference. This mirrors the tsx regression test (TestTSXJSXResolves) but
// exercises the .jsx extension path through grammarTable.
func TestJSXFileDefsAndRefs(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"widget.jsx": "function greet() { return \"hi\" }\n" + // 1: definition
			"function Widget() {\n" + // 2
			"  return <div>{greet()}</div>;\n" + // 3: JSX with call ref to greet
			"}\n", // 4
	})
	b := New(root)
	abs := filepath.Join(root, "widget.jsx")

	// The definition of greet must resolve — proving the .jsx file parsed.
	defRes, err := b.Definition(abs, "greet")
	if err != nil {
		t.Fatalf("jsx Definition(greet): %v", err)
	}
	if got := locLines(t, root, defRes); !contains(got, "widget.jsx:1") {
		t.Fatalf("jsx greet def = %v, want widget.jsx:1 (jsx file failed to parse?)", got)
	}

	// The call to greet() inside JSX source must appear as a reference.
	refRes, err := b.References(abs, "greet")
	if err != nil {
		t.Fatalf("jsx References(greet): %v", err)
	}
	if got := locLines(t, root, refRes); !contains(got, "widget.jsx:3") {
		t.Errorf("jsx greet ref = %v, want widget.jsx:3", got)
	}
}

// TestPlainJSFileDefsAndRefs proves that plain .js files (no JSX, no TypeScript)
// are parsed correctly via tsxGrammar. Function declarations must produce
// definitions and call expressions must produce references.
func TestPlainJSFileDefsAndRefs(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"util.js": "function add(a, b) { return a + b; }\n" + // 1: definition
			"function main() {\n" + // 2
			"  return add(1, 2);\n" + // 3: call ref to add
			"}\n", // 4
	})
	b := New(root)
	abs := filepath.Join(root, "util.js")

	// The definition of add must resolve.
	defRes, err := b.Definition(abs, "add")
	if err != nil {
		t.Fatalf("js Definition(add): %v", err)
	}
	if got := locLines(t, root, defRes); !contains(got, "util.js:1") {
		t.Fatalf("js add def = %v, want util.js:1", got)
	}

	// The call site add(1, 2) must appear as a reference.
	refRes, err := b.References(abs, "add")
	if err != nil {
		t.Fatalf("js References(add): %v", err)
	}
	if got := locLines(t, root, refRes); !contains(got, "util.js:3") {
		t.Errorf("js add ref = %v, want util.js:3", got)
	}
}
