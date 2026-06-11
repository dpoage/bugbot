package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadCaps_Resolve(t *testing.T) {
	// Zero/negative fields fall back to the package defaults; positive ones pass
	// through.
	if got := (ReadCaps{}).resolve(); got.MaxLines != maxReadLines || got.MaxBytes != maxReadBytes {
		t.Errorf("zero ReadCaps resolved to %+v, want defaults {%d %d}", got, maxReadLines, maxReadBytes)
	}
	if got := (ReadCaps{MaxLines: -5, MaxBytes: -5}).resolve(); got.MaxLines != maxReadLines || got.MaxBytes != maxReadBytes {
		t.Errorf("negative ReadCaps resolved to %+v, want defaults", got)
	}
	if got := (ReadCaps{MaxLines: 50, MaxBytes: 1024}).resolve(); got.MaxLines != 50 || got.MaxBytes != 1024 {
		t.Errorf("explicit ReadCaps resolved to %+v, want {50 1024}", got)
	}
}

func TestNewReadFileWithCaps_LineCapTruncates(t *testing.T) {
	root := t.TempDir()
	// 100-line file.
	var sb strings.Builder
	for i := 1; i <= 100; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	mustWrite(t, filepath.Join(root, "big.txt"), sb.String())

	tool, err := NewReadFileWithCaps(root, ReadCaps{MaxLines: 10})
	if err != nil {
		t.Fatalf("NewReadFileWithCaps: %v", err)
	}
	out, err := tool.Run(context.Background(), []byte(`{"path":"big.txt"}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "1\tline 1") || !strings.Contains(out, "10\tline 10") {
		t.Errorf("expected lines 1-10:\n%s", out)
	}
	if strings.Contains(out, "line 11") {
		t.Errorf("line cap of 10 should have truncated before line 11:\n%s", out)
	}
	if !strings.Contains(out, "truncated: showing lines 1-10 of 100 — call read_file again with offset=11 to continue") {
		t.Errorf("expected truncation note:\n%s", out)
	}
}

func TestNewReadFileWithCaps_ByteCapTruncates(t *testing.T) {
	root := t.TempDir()
	content := strings.Repeat("abcdefghij\n", 100) // 1100 bytes
	mustWrite(t, filepath.Join(root, "big.txt"), content)

	tool, err := NewReadFileWithCaps(root, ReadCaps{MaxBytes: 50})
	if err != nil {
		t.Fatalf("NewReadFileWithCaps: %v", err)
	}
	out, err := tool.Run(context.Background(), []byte(`{"path":"big.txt"}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "truncated at 50 bytes: this is a window, not the whole file") {
		t.Errorf("expected byte-cap truncation note:\n%s", out)
	}
}

func TestNewReadFile_UsesDefaultCaps(t *testing.T) {
	// The plain constructor must behave exactly as the looser default caps so
	// repro/verify callers are unaffected by the finder tightening.
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "f.txt"), "only line\n")
	tool, err := NewReadFile(root)
	if err != nil {
		t.Fatalf("NewReadFile: %v", err)
	}
	rf, ok := tool.(*readFileTool)
	if !ok {
		t.Fatalf("expected *readFileTool, got %T", tool)
	}
	if rf.caps.MaxLines != maxReadLines || rf.caps.MaxBytes != maxReadBytes {
		t.Errorf("NewReadFile caps = %+v, want package defaults {%d %d}", rf.caps, maxReadLines, maxReadBytes)
	}
}

// TestReadFileTool_DefDescription_ReflectsActualCaps verifies that a tool built
// with custom caps reports THOSE caps in its schema Description, not the
// hardcoded package defaults. A finder told the wrong cap would believe a
// truncated read was the whole file and never page for more (bugbot-3nf review).
func TestReadFileTool_DefDescription_ReflectsActualCaps(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "f.txt"), "only line\n")

	// Build with tighter caps (800 lines / 96 KB — the funnel finder defaults).
	tool, err := NewReadFileWithCaps(root, ReadCaps{MaxLines: 800, MaxBytes: 96 * 1024})
	if err != nil {
		t.Fatalf("NewReadFileWithCaps: %v", err)
	}
	desc := tool.Def().Description
	if !strings.Contains(desc, "800") {
		t.Errorf("Def().Description does not mention MaxLines=800:\n%s", desc)
	}
	if !strings.Contains(desc, "96") {
		t.Errorf("Def().Description does not mention MaxBytes/1024=96:\n%s", desc)
	}
	// Sanity: must NOT claim the looser package defaults.
	if strings.Contains(desc, "2000") {
		t.Errorf("Def().Description still mentions hardcoded 2000 lines for a custom-cap tool:\n%s", desc)
	}

	// Also check that the default constructor reports package defaults.
	defaultTool, err := NewReadFile(root)
	if err != nil {
		t.Fatalf("NewReadFile: %v", err)
	}
	defaultDesc := defaultTool.Def().Description
	if !strings.Contains(defaultDesc, "2000") {
		t.Errorf("default Def().Description does not mention 2000:\n%s", defaultDesc)
	}
	if !strings.Contains(defaultDesc, "256") {
		t.Errorf("default Def().Description does not mention 256KB:\n%s", defaultDesc)
	}
}

// TestReadFileTool_LineTruncNote_OffsetIsCorrect verifies the truncation note
// emits the correct next-offset — a finder must be able to paste it verbatim
// into its next call (bugbot-3nf review).
func TestReadFileTool_LineTruncNote_OffsetIsCorrect(t *testing.T) {
	root := t.TempDir()
	var sb strings.Builder
	for i := 1; i <= 50; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	mustWrite(t, filepath.Join(root, "f.txt"), sb.String())

	// Cap at 20 lines so lines 1-20 are shown and offset=21 is the next page.
	tool, err := NewReadFileWithCaps(root, ReadCaps{MaxLines: 20})
	if err != nil {
		t.Fatalf("NewReadFileWithCaps: %v", err)
	}
	out, err := tool.Run(context.Background(), []byte(`{"path":"f.txt"}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := "truncated: showing lines 1-20 of 50 — call read_file again with offset=21 to continue"
	if !strings.Contains(out, want) {
		t.Errorf("line-trunc note wrong or missing correct next offset.\nwant substring: %q\ngot:\n%s", want, out)
	}
}

// TestReadFileTool_ByteTruncNote_OffsetIsCorrect verifies the byte-trunc note
// carries the "window not whole file" signal and a usable next-offset
// (bugbot-3nf review).
func TestReadFileTool_ByteTruncNote_OffsetIsCorrect(t *testing.T) {
	root := t.TempDir()
	// Write 100 lines of "abcdefghij\n" (11 bytes each = 1100 bytes total).
	// With MaxBytes=55 we get exactly 5 complete lines (55 bytes), so the next
	// page starts at offset=6.
	content := strings.Repeat("abcdefghij\n", 100)
	mustWrite(t, filepath.Join(root, "f.txt"), content)

	tool, err := NewReadFileWithCaps(root, ReadCaps{MaxBytes: 55})
	if err != nil {
		t.Fatalf("NewReadFileWithCaps: %v", err)
	}
	out, err := tool.Run(context.Background(), []byte(`{"path":"f.txt"}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "truncated at 55 bytes: this is a window, not the whole file") {
		t.Errorf("byte-trunc note missing window-not-whole-file signal:\n%s", out)
	}
	if !strings.Contains(out, "offset=6") {
		t.Errorf("byte-trunc note missing offset=6 (next page after 5 lines):\n%s", out)
	}
}
