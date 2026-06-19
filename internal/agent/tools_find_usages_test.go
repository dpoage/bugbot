package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/lsp"
)

// newFindUsagesToolWithLocs creates a findUsagesTool over a temp repo; the
// scripted navigator returns locs for References queries. absPath is the
// resolved temp-dir root so test code can build absolute URIs.
func newFindUsagesToolWithLocs(t *testing.T, files map[string]string, locs []lsp.Location) (*findUsagesTool, string) {
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
	nav, err := NewCodeNav(dir)
	if err != nil {
		t.Fatalf("NewCodeNav: %v", err)
	}
	_ = nav.nav.Close()
	nav.nav = &fakeNavigator{locs: locs}
	t.Cleanup(func() { _ = nav.Close() })
	return &findUsagesTool{nav: nav}, dir
}

func TestFindUsagesHappyPath(t *testing.T) {
	src := "package main\n" + // 1
		"\n" + // 2
		"func Doer() {}\n" + // 3
		"\n" + // 4
		"func callerA() { Doer() }\n" + // 5
		"func callerB() { Doer() }\n" + // 6
		"func callerC() { Doer() }\n" // 7

	files := map[string]string{"main.go": src}
	// Pre-flight: get the real dir so we can build correct file URIs.
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
	absMain := filepath.Join(dir, "main.go")
	realLocs := []lsp.Location{
		{URI: lsp.URIFromPath(absMain), Range: lsp.Range{Start: lsp.Position{Line: 4}}}, // line 5
		{URI: lsp.URIFromPath(absMain), Range: lsp.Range{Start: lsp.Position{Line: 5}}}, // line 6
		{URI: lsp.URIFromPath(absMain), Range: lsp.Range{Start: lsp.Position{Line: 6}}}, // line 7
	}
	nav, err := NewCodeNav(dir)
	if err != nil {
		t.Fatalf("NewCodeNav: %v", err)
	}
	_ = nav.nav.Close()
	nav.nav = &fakeNavigator{locs: realLocs}
	t.Cleanup(func() { _ = nav.Close() })
	tool := &findUsagesTool{nav: nav}

	out, err := runTool(t, tool, findUsagesArgs{File: "main.go", Line: 3, Symbol: "Doer"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.HasPrefix(out, "ERROR:") {
		t.Fatalf("unexpected error result: %s", out)
	}
	// Each call site must appear as a section header.
	for _, want := range []string{"main.go:5", "main.go:6", "main.go:7"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing call site %q:\n%s", want, out)
		}
	}
	// Context lines: callerA is at line 5, context window includes lines 2-8.
	if !strings.Contains(out, "callerA") {
		t.Errorf("callerA context missing:\n%s", out)
	}
	if !strings.Contains(out, "callerB") {
		t.Errorf("callerB context missing:\n%s", out)
	}
}

func TestFindUsagesCapHonored(t *testing.T) {
	// 15 lines of filler functions.
	src := "package main\n"
	for i := 0; i < 20; i++ {
		src += "func doer() {}\n"
	}
	files := map[string]string{"main.go": src}

	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	absMain := filepath.Join(dir, "main.go")
	var realLocs []lsp.Location
	for i := 2; i <= 16; i++ {
		realLocs = append(realLocs, lsp.Location{
			URI:   lsp.URIFromPath(absMain),
			Range: lsp.Range{Start: lsp.Position{Line: i - 1}},
		})
	}
	nav, err := NewCodeNav(dir)
	if err != nil {
		t.Fatalf("NewCodeNav: %v", err)
	}
	_ = nav.nav.Close()
	nav.nav = &fakeNavigator{locs: realLocs}
	t.Cleanup(func() { _ = nav.Close() })
	tool := &findUsagesTool{nav: nav}

	out, err := runTool(t, tool, findUsagesArgs{
		File: "main.go", Line: 2, Symbol: "doer", TopN: 5,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Exactly 5 call-site windows.
	count := strings.Count(out, "--- main.go:")
	if count != 5 {
		t.Errorf("expected 5 call-site windows, got %d:\n%s", count, out)
	}
	// Truncation notice.
	if !strings.Contains(out, "capped at 5 usages") {
		t.Errorf("missing truncation notice:\n%s", out)
	}
}

func TestFindUsagesNoResults(t *testing.T) {
	src := "package main\nfunc Unused() {}\n"
	c, _ := newTestCodeNav(t, map[string]string{"main.go": src}, &fakeNavigator{locs: nil})
	tool := &findUsagesTool{nav: c}

	out, err := runTool(t, tool, findUsagesArgs{File: "main.go", Line: 2, Symbol: "Unused"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "no usages found") {
		t.Errorf("expected no-usages message:\n%s", out)
	}
}

func TestFindUsagesBadArgs(t *testing.T) {
	c, _ := newTestCodeNav(t, map[string]string{"main.go": "package main\n"}, &fakeNavigator{})
	tool := &findUsagesTool{nav: c}

	for _, tc := range []struct {
		name string
		args findUsagesArgs
		want string
	}{
		{"missing file", findUsagesArgs{Line: 1, Symbol: "X"}, "file is required"},
		{"zero line", findUsagesArgs{File: "main.go", Symbol: "X"}, "line must be"},
		{"empty symbol", findUsagesArgs{File: "main.go", Line: 1, Symbol: "   "}, "symbol is required"},
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

func TestFindUsagesInvalidJSON(t *testing.T) {
	c, _ := newTestCodeNav(t, map[string]string{"main.go": "package main\n"}, &fakeNavigator{})
	tool := &findUsagesTool{nav: c}
	out, err := tool.Run(context.Background(), json.RawMessage(`{bad json`))
	if err != nil {
		t.Fatalf("Run returned hard error (should use toolError): %v", err)
	}
	if !strings.HasPrefix(out, "ERROR:") {
		t.Errorf("expected ERROR: prefix for invalid JSON, got: %s", out)
	}
}

func TestFindUsagesDefaultTopN(t *testing.T) {
	// top_n omitted (zero) should default to findUsagesDefaultN (10).
	src := "package main\n"
	for i := 0; i < 20; i++ {
		src += "func doer() {}\n"
	}
	files := map[string]string{"main.go": src}

	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	absMain := filepath.Join(dir, "main.go")
	var realLocs []lsp.Location
	for i := 2; i <= 21; i++ {
		realLocs = append(realLocs, lsp.Location{
			URI:   lsp.URIFromPath(absMain),
			Range: lsp.Range{Start: lsp.Position{Line: i - 1}},
		})
	}
	nav, err := NewCodeNav(dir)
	if err != nil {
		t.Fatalf("NewCodeNav: %v", err)
	}
	_ = nav.nav.Close()
	nav.nav = &fakeNavigator{locs: realLocs}
	t.Cleanup(func() { _ = nav.Close() })
	tool := &findUsagesTool{nav: nav}

	out, err := runTool(t, tool, findUsagesArgs{File: "main.go", Line: 2, Symbol: "doer"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	count := strings.Count(out, "--- main.go:")
	if count != findUsagesDefaultN {
		t.Errorf("expected %d (default) call-site windows, got %d:\n%s", findUsagesDefaultN, count, out)
	}
}
