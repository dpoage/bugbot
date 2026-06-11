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
	if !strings.Contains(out, "truncated: showing lines 1-10 of 100") {
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
	if !strings.Contains(out, "file exceeds 50 bytes") {
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
