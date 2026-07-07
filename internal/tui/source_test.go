package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// newTestModel returns a Model with repoRoot set to root.
func newTestModel(root string) Model {
	m := NewModel(context.Background(), &fakeFeed{}, nil)
	if root != "" {
		m = m.WithRepoRoot(root)
	}
	return m
}

// sendMsg injects a tea.Msg directly into Update, runs any returned cmd, and
// returns the new Model. This is the pattern used by model_test.go's runCmd.
func sendMsg(m Model, msg tea.Msg) Model {
	next, cmd := m.Update(msg)
	return runCmd(next.(Model), cmd)
}

// ── Path resolution tests ─────────────────────────────────────────────────────

func TestResolveSourcePath_Valid(t *testing.T) {
	root := t.TempDir()
	abs, err := resolveSourcePath(root, "src/main.go")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(abs, root) {
		t.Fatalf("resolved path %q not under root %q", abs, root)
	}
}

func TestResolveSourcePath_AbsoluteRejected(t *testing.T) {
	root := t.TempDir()
	_, err := resolveSourcePath(root, "/etc/passwd")
	if err == nil {
		t.Fatal("expected error for absolute path, got nil")
	}
}

func TestResolveSourcePath_DotDotEscape(t *testing.T) {
	root := t.TempDir()
	_, err := resolveSourcePath(root, "../../etc/passwd")
	if err == nil {
		t.Fatal("expected error for path escaping root, got nil")
	}
}

func TestResolveSourcePath_Exact(t *testing.T) {
	root := t.TempDir()
	// resolving empty string returns root itself — should succeed.
	abs, err := resolveSourcePath(root, "")
	if err != nil {
		t.Fatalf("empty path should resolve to root: %v", err)
	}
	cleanRoot := filepath.Clean(root)
	if abs != cleanRoot {
		t.Fatalf("got %q, want %q", abs, cleanRoot)
	}
}

// ── Binary detection ──────────────────────────────────────────────────────────

func TestIsBinary_WithNUL(t *testing.T) {
	data := []byte("hello\x00world")
	if !isBinary(data) {
		t.Fatal("expected binary=true for data with NUL")
	}
}

func TestIsBinary_PlainText(t *testing.T) {
	data := []byte("package main\n\nfunc main() {}\n")
	if isBinary(data) {
		t.Fatal("expected binary=false for plain Go source")
	}
}

// ── Highlight fallback ────────────────────────────────────────────────────────

func TestHighlightSource_UnknownExtFallback(t *testing.T) {
	src := []byte("hello world\nfoo bar\n")
	lines := highlightSource(src, "something.zzz_unknown_ext")
	// Must not crash and must return lines.
	if len(lines) == 0 {
		t.Fatal("expected non-empty line slice for unknown extension")
	}
	// Lines must contain the original text (either verbatim or ANSI-wrapped).
	combined := strings.Join(lines, "\n")
	if !strings.Contains(stripANSI(combined), "hello world") {
		t.Fatalf("expected original text in output, got: %q", combined)
	}
}

func TestHighlightSource_GoFile(t *testing.T) {
	src := []byte("package main\n\nfunc main() {\n}\n")
	lines := highlightSource(src, "main.go")
	if len(lines) == 0 {
		t.Fatal("expected non-empty line slice for .go file")
	}
}

// ── Oversized truncation ──────────────────────────────────────────────────────

func TestLoadSourceCmd_OversizedTruncation(t *testing.T) {
	root := t.TempDir()
	// Write a file larger than sourceMaxBytes (1 MB).
	large := make([]byte, sourceMaxBytes+1024)
	for i := range large {
		large[i] = 'a'
	}
	large[512] = '\n' // at least one newline so it parses as lines
	path := filepath.Join(root, "large.txt")
	if err := os.WriteFile(path, large, 0o644); err != nil {
		t.Fatal(err)
	}
	fn := loadSourceCmd(1, root, openSourceMsg{File: "large.txt"})
	raw := fn()
	msg, ok := raw.(sourceLoadedMsg)
	if !ok {
		t.Fatalf("expected sourceLoadedMsg, got %T", raw)
	}
	if msg.gen != 1 {
		t.Fatalf("gen mismatch: got %d, want 1", msg.gen)
	}
	// Truncation note should be present somewhere.
	allText := strings.Join(msg.lines, "\n")
	if !strings.Contains(stripANSI(allText), "truncated") {
		t.Fatalf("expected truncation note in output, got lines:\n%s", allText)
	}
}

func TestLoadSourceCmd_LineCap(t *testing.T) {
	root := t.TempDir()
	// Write a file with more lines than sourceMaxLines.
	var buf strings.Builder
	for i := 0; i < sourceMaxLines+100; i++ {
		buf.WriteString("line\n")
	}
	path := filepath.Join(root, "many.txt")
	if err := os.WriteFile(path, []byte(buf.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	fn := loadSourceCmd(1, root, openSourceMsg{File: "many.txt"})
	raw := fn()
	msg := raw.(sourceLoadedMsg)
	// Lines including any truncation note should be at most sourceMaxLines+1.
	if len(msg.lines) > sourceMaxLines+1 {
		t.Fatalf("expected at most %d lines, got %d", sourceMaxLines+1, len(msg.lines))
	}
}

// ── Binary file degrades to note ──────────────────────────────────────────────

func TestLoadSourceCmd_BinaryFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "binary.bin")
	if err := os.WriteFile(path, []byte("hello\x00world"), 0o644); err != nil {
		t.Fatal(err)
	}
	fn := loadSourceCmd(1, root, openSourceMsg{File: "binary.bin"})
	raw := fn()
	msg := raw.(sourceLoadedMsg)
	if msg.note == "" {
		t.Fatal("expected non-empty note for binary file")
	}
	if len(msg.lines) != 0 {
		t.Fatalf("expected no lines for binary file, got %d", len(msg.lines))
	}
}

// ── Missing file degrades to note ────────────────────────────────────────────

func TestLoadSourceCmd_MissingFile(t *testing.T) {
	root := t.TempDir()
	fn := loadSourceCmd(1, root, openSourceMsg{File: "nonexistent.go"})
	raw := fn()
	msg := raw.(sourceLoadedMsg)
	if msg.note == "" {
		t.Fatal("expected non-empty note for missing file")
	}
}

// ── Async load via injected openSourceMsg ────────────────────────────────────

func TestOpenSourceMsg_AsyncLoad(t *testing.T) {
	root := t.TempDir()
	// Write a small source file.
	src := "package main\n\nfunc main() {\n\t// hello\n}\n"
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	m := newTestModel(root)

	// Send openSourceMsg — Update returns a cmd.
	next, cmd := m.Update(openSourceMsg{File: "main.go", Line: 3, EndLine: 4})
	m = next.(Model)
	if m.contextMode != contextModeSource {
		t.Fatalf("expected contextModeSource, got %v", m.contextMode)
	}
	if cmd == nil {
		t.Fatal("expected a non-nil cmd from openSourceMsg")
	}

	// Execute the cmd (runs loadSourceCmd synchronously in test).
	raw := cmd()
	m2, _ := m.Update(raw)
	m = m2.(Model)

	if m.sourceFile != "main.go" {
		t.Fatalf("sourceFile: got %q, want %q", m.sourceFile, "main.go")
	}
	if len(m.sourceLines) == 0 {
		t.Fatal("expected non-empty sourceLines after load")
	}
	// Target line should be set.
	if m.sourceLine != 3 {
		t.Fatalf("sourceLine: got %d, want 3", m.sourceLine)
	}
}

// ── Stale-load discard ────────────────────────────────────────────────────────

func TestOpenSourceMsg_StaleLoadDiscard(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.go"), []byte("package b\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := newTestModel(root)

	// Open a.go.
	_, cmdA := m.Update(openSourceMsg{File: "a.go"})

	// Immediately open b.go — bumps gen.
	next, cmdB := m.Update(openSourceMsg{File: "b.go"})
	m = next.(Model)
	_ = cmdA // stale

	// Run b.go's cmd.
	if cmdB != nil {
		m = runCmd(m, cmdB)
	}
	// Inject stale result for a.go (old gen).
	staleMsg := sourceLoadedMsg{
		gen:   m.sourceLoadGen - 1, // stale
		lines: []string{"stale content"},
		file:  "a.go",
	}
	m2, _ := m.Update(staleMsg)
	m = m2.(Model)

	// File should still be b.go, not overwritten by stale a.go.
	if m.sourceFile != "b.go" {
		t.Fatalf("stale load clobbered sourceFile: got %q, want b.go", m.sourceFile)
	}
}

// ── Grep hit list ─────────────────────────────────────────────────────────────

func TestLoadGrepCmd_BasicSearch(t *testing.T) {
	root := t.TempDir()
	// Write a couple of files with matching content.
	if err := os.WriteFile(filepath.Join(root, "foo.go"), []byte("package main\n\nfunc hello() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bar.go"), []byte("package main\n\nvar x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	fn := loadGrepCmd(1, root, "hello")
	raw := fn()
	msg := raw.(grepLoadedMsg)
	if msg.note != "" {
		t.Fatalf("unexpected note: %q", msg.note)
	}
	if len(msg.hits) == 0 {
		t.Fatal("expected at least one grep hit")
	}
	found := false
	for _, h := range msg.hits {
		if h.file == "foo.go" && strings.Contains(h.content, "hello") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected hit in foo.go, got hits: %+v", msg.hits)
	}
}

func TestLoadGrepCmd_BoundedHits(t *testing.T) {
	root := t.TempDir()
	// Write one file with many matches (more than grepMaxHits).
	var buf strings.Builder
	for i := 0; i < grepMaxHits+50; i++ {
		buf.WriteString("match_me\n")
	}
	if err := os.WriteFile(filepath.Join(root, "big.txt"), []byte(buf.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	fn := loadGrepCmd(1, root, "match_me")
	raw := fn()
	msg := raw.(grepLoadedMsg)
	if len(msg.hits) > grepMaxHits {
		t.Fatalf("expected at most %d hits, got %d", grepMaxHits, len(msg.hits))
	}
}

func TestLoadGrepCmd_InvalidRegexLiteralFallback(t *testing.T) {
	root := t.TempDir()
	// Write a file containing the literal pattern (which is invalid regex).
	if err := os.WriteFile(filepath.Join(root, "test.go"), []byte("x := [broken\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// "[broken" is invalid regex; should fall back to literal search.
	fn := loadGrepCmd(1, root, "[broken")
	raw := fn()
	msg := raw.(grepLoadedMsg)
	if len(msg.hits) == 0 {
		t.Fatal("expected literal-fallback hit for invalid regex pattern")
	}
}

func TestLoadGrepCmd_SkipsGitDir(t *testing.T) {
	root := t.TempDir()
	// Create a .git directory with a matching file — should be skipped.
	gitDir := filepath.Join(root, ".git")
	if err := os.Mkdir(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte("find_me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Write a real file without the pattern.
	if err := os.WriteFile(filepath.Join(root, "real.go"), []byte("nothing here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fn := loadGrepCmd(1, root, "find_me")
	raw := fn()
	msg := raw.(grepLoadedMsg)
	// No hits — .git was skipped.
	if len(msg.hits) != 0 {
		t.Fatalf("expected 0 hits (.git skipped), got %d: %+v", len(msg.hits), msg.hits)
	}
}

// ── Grep + enter jumps to file ────────────────────────────────────────────────

func TestGrepEnter_JumpsToFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "target.go"), []byte("package main\n\nfunc foo() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := newTestModel(root)

	// Open grep view.
	next, cmd := m.Update(openSourceMsg{Pattern: "foo"})
	m = next.(Model)
	if m.contextMode != contextModeGrep {
		t.Fatalf("expected contextModeGrep, got %v", m.contextMode)
	}
	// Run grep cmd to populate hits.
	if cmd != nil {
		m = runCmd(m, cmd)
	}
	if len(m.grepHits) == 0 {
		t.Fatal("expected grep hits after running cmd")
	}
	// Press enter — should trigger a source load.
	m.grepCursor = 0
	next2, cmd2 := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next2.(Model)
	if cmd2 != nil {
		m = runCmd(m, cmd2)
	}
	if m.contextMode != contextModeSource {
		t.Fatalf("expected contextModeSource after enter on grep hit, got %v", m.contextMode)
	}
	if m.sourceFile == "" {
		t.Fatal("sourceFile should be set after enter on grep hit")
	}
}

// ── Esc returns from source/grep view ────────────────────────────────────────

func TestSourceEsc_ReturnsToPrevMode(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "x.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := newTestModel(root)
	m.focus = paneContext
	m.contextMode = contextModeFindings // set a non-default prior mode

	// Open source view.
	next, cmd := m.Update(openSourceMsg{File: "x.go"})
	m = next.(Model)
	if cmd != nil {
		m = runCmd(m, cmd)
	}
	if m.contextMode != contextModeSource {
		t.Fatalf("expected contextModeSource, got %v", m.contextMode)
	}

	// Press esc — should return to the prior mode (contextModeFindings).
	m = sendKey(m, "esc")
	if m.contextMode != contextModeFindings {
		t.Fatalf("esc should restore prior mode (findings), got %v", m.contextMode)
	}
}

// ── Path escape via openSourceMsg model rejected ──────────────────────────────

func TestOpenSourceMsg_PathEscape_Note(t *testing.T) {
	root := t.TempDir()
	m := newTestModel(root)

	// Send openSourceMsg with escaping path; the async cmd should return an error note.
	next, cmd := m.Update(openSourceMsg{File: "../../etc/passwd"})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("expected cmd for file open attempt")
	}
	raw := cmd()
	m2, _ := m.Update(raw)
	m = m2.(Model)
	// Should have a note and no lines.
	if m.sourceNote == "" {
		t.Fatal("expected sourceNote for escaping path")
	}
	if len(m.sourceLines) != 0 {
		t.Fatal("expected no lines for escaping path")
	}
}

// ── N3: additional path-safety cases ─────────────────────────────────────────

// TestResolveSourcePath_EscapeAfterClean checks that "a/../../etc/passwd"
// (which Clean reduces to "../etc/passwd") is rejected.
func TestResolveSourcePath_EscapeAfterClean(t *testing.T) {
	root := t.TempDir()
	_, err := resolveSourcePath(root, "a/../../etc/passwd")
	if err == nil {
		t.Fatal("expected error for path that escapes root after Clean, got nil")
	}
}

// TestResolveSourcePath_SymlinkEscape checks that a symlink inside the repo
// that points outside the root is rejected.
func TestResolveSourcePath_SymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	// Create a symlink inside root pointing to outside.
	link := filepath.Join(root, "escape_link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skip("symlinks not supported on this filesystem:", err)
	}
	_, err := resolveSourcePath(root, "escape_link")
	if err == nil {
		t.Fatal("expected error for symlink escaping root, got nil")
	}
}

// ── B2: renderSourceView marks only the target line when endLine==0 ───────────

// TestRenderSourceView_SingleLineMarked forces an ANSI256 color profile so
// lipgloss emits real SGR sequences, then asserts on raw (ANSI-bearing) output:
//
//   - raw(end=0) == raw(end=5)  clamping: end<targetLine → end=targetLine
//   - raw(end=0) != raw(end=6)  end=6 highlights line 6 additionally
//   - exactly 1 rendered line differs between end=0 and end=6 (line 6's row only)
//
// The gutter style (Faint) emits SGR on every line, so counting total SGR lines
// is meaningless; we compare end=0 vs end=6 row-by-row instead.
func TestRenderSourceView_SingleLineMarked(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI256)
	t.Cleanup(func() { lipgloss.SetColorProfile(termenv.Ascii) })

	lines := []string{"line one", "line two", "line three", "line four", "line five target", "line six"}

	outEnd0 := renderSourceView(lines, "test.go", 5, 0, 0, 120, 20)
	outEnd5 := renderSourceView(lines, "test.go", 5, 5, 0, 120, 20)
	outEnd6 := renderSourceView(lines, "test.go", 5, 6, 0, 120, 20)

	// Clamping: end=0 and end=5 must be byte-identical.
	if outEnd0 != outEnd5 {
		t.Fatalf("end=0 and end=5 must produce identical raw output (end clamped to targetLine);\nend=0:\n%s\nend=5:\n%s", outEnd0, outEnd5)
	}

	// end=6 highlights line 6 too, so the outputs must differ.
	if outEnd0 == outEnd6 {
		t.Fatal("end=0 and end=6 must differ in raw output (end=6 marks line 6 additionally)")
	}

	// Exactly 1 row differs between end=0 and end=6: line 6's rendered row.
	rows0 := strings.Split(outEnd0, "\n")
	rows6 := strings.Split(outEnd6, "\n")
	if len(rows0) != len(rows6) {
		t.Fatalf("end=0 and end=6 must have equal row count; got %d vs %d", len(rows0), len(rows6))
	}
	diffCount := 0
	for i := range rows0 {
		if rows0[i] != rows6[i] {
			diffCount++
		}
	}
	if diffCount != 1 {
		t.Fatalf("expected exactly 1 differing row between end=0 and end=6 (line 6's row), got %d", diffCount)
	}
}
