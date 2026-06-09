package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareWorkspaceCopiesTreeAndAppliesWriteFiles(t *testing.T) {
	src := t.TempDir()

	// Lay out a small repo: a nested file and an executable.
	mustWrite(t, filepath.Join(src, "go.mod"), "module example\n", 0o644)
	mustMkdir(t, filepath.Join(src, "pkg"))
	mustWrite(t, filepath.Join(src, "pkg", "main.go"), "package pkg\n", 0o644)
	mustWrite(t, filepath.Join(src, "run.sh"), "#!/bin/sh\necho hi\n", 0o755)

	ws, err := prepareWorkspace(src, map[string][]byte{
		"repro/bug_test.go": []byte("package repro\n"),
		"go.mod":            []byte("module overridden\n"), // overwrites an existing file
	})
	if err != nil {
		t.Fatalf("prepareWorkspace: %v", err)
	}
	defer os.RemoveAll(ws)

	if ws == src {
		t.Fatal("workspace must be a separate directory from the source repo")
	}

	// Copied files present.
	assertFileContent(t, filepath.Join(ws, "pkg", "main.go"), "package pkg\n")

	// Executable bit preserved.
	if info, err := os.Stat(filepath.Join(ws, "run.sh")); err != nil {
		t.Fatalf("stat run.sh: %v", err)
	} else if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("run.sh lost its executable bit: %v", info.Mode())
	}

	// WriteFiles injected, including nested dir creation.
	assertFileContent(t, filepath.Join(ws, "repro", "bug_test.go"), "package repro\n")

	// WriteFiles overwrote the copied file.
	assertFileContent(t, filepath.Join(ws, "go.mod"), "module overridden\n")

	// Original repo was not mutated.
	assertFileContent(t, filepath.Join(src, "go.mod"), "module example\n")
	if _, err := os.Stat(filepath.Join(src, "repro")); !os.IsNotExist(err) {
		t.Errorf("original repo was mutated: repro/ should not exist, err=%v", err)
	}
}

func TestPrepareWorkspaceRejectsNonDir(t *testing.T) {
	f := filepath.Join(t.TempDir(), "afile")
	mustWrite(t, f, "x", 0o644)
	if _, err := prepareWorkspace(f, nil); err == nil {
		t.Fatal("expected error for non-directory repo dir")
	}
}

func TestSanitizeRelPath(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
		want    string
	}{
		{"simple", "a.txt", false, "a.txt"},
		{"nested", "a/b/c.txt", false, filepath.Join("a", "b", "c.txt")},
		{"dotslash", "./a.txt", false, "a.txt"},
		{"absolute", "/etc/passwd", true, ""},
		{"escape", "../outside", true, ""},
		{"escape-nested", "a/../../outside", true, ""},
		{"empty", "", true, ""},
		{"root", ".", true, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := sanitizeRelPath(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got %q", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("sanitizeRelPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestApplyWriteFilesRejectsEscape(t *testing.T) {
	ws := t.TempDir()
	err := applyWriteFiles(ws, map[string][]byte{"../escape.txt": []byte("nope")})
	if err == nil {
		t.Fatal("expected error for path escaping workspace")
	}
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(ws), "escape.txt")); !os.IsNotExist(statErr) {
		t.Errorf("escape file should not have been written: %v", statErr)
	}
}

// --- helpers ---

func mustWrite(t *testing.T, path, content string, perm os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != want {
		t.Errorf("%s = %q, want %q", path, got, want)
	}
}
