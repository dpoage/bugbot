package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/treesitter"
)

// newOutlineTool returns an outlineTool with a fake outline backend for unit
// testing. The fakeOutlineBackend is defined in tools_outline.go.
func newOutlineTool(t *testing.T, files map[string]string, backend tsOutlineBackend) *outlineTool {
	t.Helper()
	c, _ := newTestCodeNav(t, files, &fakeNavigator{})
	if backend != nil {
		c.outline = backend
	}
	return &outlineTool{nav: c}
}

func TestOutlineGoFile(t *testing.T) {
	// Multi-line functions so the outline signature line does not include body
	// content. Inner body lines (marked with BODY_MARKER) must not appear in
	// the output — the outline renders only the opening declaration line.
	src := "package main\n" + // 1
		"\n" + // 2
		"type Foo struct {\n" + // 3
		"\tX int\n" + // 4
		"}\n" + // 5
		"\n" + // 6
		"func NewFoo() *Foo {\n" + // 7
		"\treturn &Foo{} // BODY_MARKER\n" + // 8
		"}\n" + // 9
		"\n" + // 10
		"func (f *Foo) Bar() int {\n" + // 11
		"\treturn f.X // BODY2_MARKER\n" + // 12
		"}\n" // 13

	c, _ := newTestCodeNav(t, map[string]string{"main.go": src}, nil)
	tool := &outlineTool{nav: c}

	out, err := runTool(t, tool, outlineArgs{File: "main.go"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.HasPrefix(out, "ERROR:") {
		t.Fatalf("unexpected error: %s", out)
	}
	if !strings.Contains(out, "outline: main.go") {
		t.Errorf("missing outline header:\n%s", out)
	}
	for _, want := range []string{"Foo", "NewFoo", "Bar"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing symbol %q:\n%s", want, out)
		}
	}
	// Body-only lines must not appear.
	for _, bodyText := range []string{"BODY_MARKER", "BODY2_MARKER"} {
		if strings.Contains(out, bodyText) {
			t.Errorf("outline rendered body text %q (signatures only expected):\n%s", bodyText, out)
		}
	}
	// Line number for NewFoo (line 7) must be present.
	if !strings.Contains(out, "7") {
		t.Errorf("line numbers missing:\n%s", out)
	}
}

func TestOutlineRankedByPosition(t *testing.T) {
	// Symbols should be ordered top-to-bottom (ascending start line).
	entries := []treesitter.OutlineEntry{
		{Name: "Last", Kind: "definition.function", StartLine: 20, EndLine: 25},
		{Name: "First", Kind: "definition.function", StartLine: 3, EndLine: 8},
		{Name: "Middle", Kind: "definition.type", StartLine: 10, EndLine: 15},
	}
	tool := newOutlineTool(t, map[string]string{"x.go": "package x\n"}, &fakeOutlineBackend{
		entries:  entries,
		supports: true,
	})

	out, err := runTool(t, tool, outlineArgs{File: "x.go"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	firstIdx := strings.Index(out, "First")
	middleIdx := strings.Index(out, "Middle")
	lastIdx := strings.Index(out, "Last")
	if firstIdx < 0 || middleIdx < 0 || lastIdx < 0 {
		t.Fatalf("missing symbol in output:\n%s", out)
	}
	if !(firstIdx < middleIdx && middleIdx < lastIdx) {
		t.Errorf("symbols not in position order (First=%d Middle=%d Last=%d):\n%s",
			firstIdx, middleIdx, lastIdx, out)
	}
}

func TestOutlineNoBodies(t *testing.T) {
	// The outline must show signature lines only, not render body content.
	src := "package main\n" +
		"func Alpha() {\n" +
		"\tpanic(\"body content must not appear\")\n" +
		"}\n"
	c, _ := newTestCodeNav(t, map[string]string{"main.go": src}, nil)
	tool := &outlineTool{nav: c}

	out, err := runTool(t, tool, outlineArgs{File: "main.go"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(out, "body content must not appear") {
		t.Errorf("outline rendered function body text:\n%s", out)
	}
}

func TestOutlineUnsupportedFileType(t *testing.T) {
	// A .rb file has no grammar; the tool must return toolError, not a hard error.
	c, _ := newTestCodeNav(t, map[string]string{"script.rb": "def foo; end\n"}, nil)
	tool := &outlineTool{nav: c}

	out, err := runTool(t, tool, outlineArgs{File: "script.rb"})
	if err != nil {
		t.Fatalf("Run returned hard error (should use toolError): %v", err)
	}
	if !strings.HasPrefix(out, "ERROR:") {
		t.Errorf("expected ERROR: prefix for unsupported type, got: %s", out)
	}
}

func TestOutlineEmptyFile(t *testing.T) {
	tool := newOutlineTool(t, map[string]string{"empty.go": "package main\n"}, &fakeOutlineBackend{
		entries:  nil,
		supports: true,
	})
	out, err := runTool(t, tool, outlineArgs{File: "empty.go"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "no top-level symbols") {
		t.Errorf("expected no-symbols message:\n%s", out)
	}
}

func TestOutlineBadArgs(t *testing.T) {
	c, _ := newTestCodeNav(t, map[string]string{"main.go": "package main\n"}, nil)
	tool := &outlineTool{nav: c}

	for _, tc := range []struct {
		name string
		args outlineArgs
		want string
	}{
		{"missing file", outlineArgs{File: ""}, "file is required"},
		{"blank file", outlineArgs{File: "   "}, "file is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(tc.args)
			out, err := tool.Run(context.Background(), raw)
			if err != nil {
				t.Fatalf("Run returned hard error (should use toolError): %v", err)
			}
			if !strings.HasPrefix(out, "ERROR:") {
				t.Errorf("expected ERROR: prefix, got: %s", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("expected %q in error output, got: %s", tc.want, out)
			}
		})
	}
}

func TestOutlineInvalidJSON(t *testing.T) {
	c, _ := newTestCodeNav(t, map[string]string{"main.go": "package main\n"}, nil)
	tool := &outlineTool{nav: c}
	out, err := tool.Run(context.Background(), json.RawMessage(`{bad`))
	if err != nil {
		t.Fatalf("Run returned hard error: %v", err)
	}
	if !strings.HasPrefix(out, "ERROR:") {
		t.Errorf("expected ERROR: prefix for invalid JSON, got: %s", out)
	}
}

func TestOutlineKindLabels(t *testing.T) {
	cases := []struct {
		kind  string
		label string
	}{
		{"definition.function", "func"},
		{"definition.method", "method"},
		{"definition.type", "type"},
		{"definition.class", "class"},
		{"definition.interface", "iface"},
		{"definition.var", "var"},
		{"definition.const", "const"},
		{"definition.module", "module"},
		{"definition.unknown_kind", "unknown_kind"},
		{"something_else", "something_else"},
	}
	for _, tc := range cases {
		got := kindLabel(tc.kind)
		if got != tc.label {
			t.Errorf("kindLabel(%q) = %q, want %q", tc.kind, got, tc.label)
		}
	}
}

func TestOutlineTruncation(t *testing.T) {
	entries := make([]treesitter.OutlineEntry, outlineMaxEntries+10)
	for i := range entries {
		entries[i] = treesitter.OutlineEntry{
			Name:      "sym",
			Kind:      "definition.function",
			StartLine: i + 1,
			EndLine:   i + 1,
		}
	}
	tool := newOutlineTool(t, map[string]string{"big.go": "package main\n"}, &fakeOutlineBackend{
		entries:  entries,
		supports: true,
	})
	out, err := runTool(t, tool, outlineArgs{File: "big.go"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "truncated at") {
		t.Errorf("expected truncation notice for %d entries:\n%s", len(entries), out)
	}
}
