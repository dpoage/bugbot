package sandbox

import (
	"io/fs"
	"os"
	"os/exec"
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
	defer func() { _ = os.RemoveAll(ws) }()

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

// TestValidateWorkspacePath locks the exported guard to sanitizeRelPath's rule:
// it must reject the exact keys applyWriteFiles would reject (absolute,
// escaping, empty, root) and accept legal relative ones, so the reproducer's
// pre-flight plan check can never diverge from the actual write.
func TestValidateWorkspacePath(t *testing.T) {
	reject := []string{"/tmp/repro_test.cpp", "/etc/passwd", "../outside", "a/../../outside", "", "."}
	for _, in := range reject {
		if err := ValidateWorkspacePath(in); err == nil {
			t.Errorf("ValidateWorkspacePath(%q) = nil, want error", in)
		}
	}
	accept := []string{"repro_test.cpp", "test/repro_test.cpp", "./a.txt"}
	for _, in := range accept {
		if err := ValidateWorkspacePath(in); err != nil {
			t.Errorf("ValidateWorkspacePath(%q) = %v, want nil", in, err)
		}
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

// TestPrepareWorkspaceSkipsGitignoredAndGit verifies the git-aware copy path:
// for a git work tree, gitignored content (a stale build tree) and the .git
// directory are NOT copied into the sandbox workspace, while tracked and
// untracked-but-not-ignored files are. This is the guard against a host
// CMakeCache.txt poisoning an in-sandbox rebuild.
func TestPrepareWorkspaceSkipsGitignoredAndGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, ".gitignore"), "/build\n", 0o644)
	mustWrite(t, filepath.Join(src, "main.go"), "package main\n", 0o644)
	mustMkdir(t, filepath.Join(src, "build"))
	mustWrite(t, filepath.Join(src, "build", "CMakeCache.txt"), "stale-host-path\n", 0o644)
	gitInit(t, src)
	// Written AFTER the commit so it is untracked-but-not-ignored: it must still
	// be copied (working-tree fidelity for uncommitted source).
	mustWrite(t, filepath.Join(src, "untracked.go"), "package main\n", 0o644)

	ws, err := prepareWorkspace(src, nil)
	if err != nil {
		t.Fatalf("prepareWorkspace: %v", err)
	}
	defer func() { _ = os.RemoveAll(ws) }()

	// Tracked + untracked-non-ignored are copied.
	assertFileContent(t, filepath.Join(ws, "main.go"), "package main\n")
	assertFileContent(t, filepath.Join(ws, "untracked.go"), "package main\n")
	// The gitignored build tree must NOT be copied (it would poison a rebuild).
	if _, err := os.Stat(filepath.Join(ws, "build")); !os.IsNotExist(err) {
		t.Errorf("gitignored build/ was copied into the workspace (err=%v)", err)
	}
	// .git metadata must NOT be copied.
	if _, err := os.Stat(filepath.Join(ws, ".git")); !os.IsNotExist(err) {
		t.Errorf(".git was copied into the workspace (err=%v)", err)
	}
}

// gitInit makes dir a git repo and commits its current tracked contents.
func gitInit(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
		{"add", "-A"},
		{"commit", "-q", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		// Neutralize the user's/CI's global+system git config so a global
		// commit.gpgsign, hooksPath hook, or credential prompt cannot make
		// `git commit` fail or hang. The repo-local identity below suffices.
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_TERMINAL_PROMPT=0",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// TestCopyWorkspaceRecreatesSymlink covers the git-aware copy's symlink branch:
// a tracked symlink must be recreated as a symlink (target not followed).
func TestCopyWorkspaceRecreatesSymlink(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "real.txt"), "hi\n", 0o644)
	if err := os.Symlink("real.txt", filepath.Join(src, "link.txt")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	gitInit(t, src)
	dst := t.TempDir()

	if err := copyWorkspace(src, dst); err != nil {
		t.Fatalf("copyWorkspace: %v", err)
	}
	fi, err := os.Lstat(filepath.Join(dst, "link.txt"))
	if err != nil {
		t.Fatalf("symlink not copied: %v", err)
	}
	if fi.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("link.txt copied as non-symlink: %v", fi.Mode())
	}
	if tgt, _ := os.Readlink(filepath.Join(dst, "link.txt")); tgt != "real.txt" {
		t.Errorf("symlink target = %q, want real.txt", tgt)
	}
}

// TestCopyFileList_RecursesSubmoduleDir covers the submodule path: git ls-files
// emits a submodule by its gitlink path, which resolves on disk to a directory.
// copyFileList must recurse-copy that working tree (so vendored-as-submodule
// deps reach the sandbox) while skipping the submodule's own .git gitfile.
func TestCopyFileList_RecursesSubmoduleDir(t *testing.T) {
	src := t.TempDir()
	mustMkdir(t, filepath.Join(src, "vendor", "gtest"))
	mustWrite(t, filepath.Join(src, "vendor", "gtest", "gtest.h"), "#pragma once\n", 0o644)
	// Submodules carry a .git FILE (a gitfile); copyTree must skip it.
	mustWrite(t, filepath.Join(src, "vendor", "gtest", ".git"), "gitdir: ../../.git/modules/vendor/gtest\n", 0o644)
	dst := t.TempDir()

	if err := copyFileList(src, dst, []string{filepath.Join("vendor", "gtest")}); err != nil {
		t.Fatalf("copyFileList: %v", err)
	}
	assertFileContent(t, filepath.Join(dst, "vendor", "gtest", "gtest.h"), "#pragma once\n")
	if _, err := os.Stat(filepath.Join(dst, "vendor", "gtest", ".git")); !os.IsNotExist(err) {
		t.Errorf("submodule .git gitfile was copied (err=%v)", err)
	}
}

// TestCopyTreeSkipsNestedGitInFallback covers the full-copy fallback (non-git
// src): a nested .git directory must be skipped so stale metadata never reaches
// the workspace even when the git-aware path is not taken.
func TestCopyTreeSkipsNestedGitInFallback(t *testing.T) {
	src := t.TempDir() // no top-level .git -> copyWorkspace falls back to copyTree
	mustWrite(t, filepath.Join(src, "main.go"), "package main\n", 0o644)
	mustMkdir(t, filepath.Join(src, "sub", ".git"))
	mustWrite(t, filepath.Join(src, "sub", ".git", "config"), "x\n", 0o644)
	mustWrite(t, filepath.Join(src, "sub", "real.txt"), "keep\n", 0o644)
	dst := t.TempDir()

	if err := copyWorkspace(src, dst); err != nil {
		t.Fatalf("copyWorkspace: %v", err)
	}
	assertFileContent(t, filepath.Join(dst, "main.go"), "package main\n")
	assertFileContent(t, filepath.Join(dst, "sub", "real.txt"), "keep\n")
	if _, err := os.Stat(filepath.Join(dst, "sub", ".git")); !os.IsNotExist(err) {
		t.Errorf("nested .git dir was copied in fallback (err=%v)", err)
	}
}

// TestCopyWorkspaceSurfacesGitError covers the RC2 safety invariant: when src is
// a git work tree but `git ls-files` fails, copyWorkspace returns the error
// rather than silently full-copying gitignored artifacts (which would reintroduce
// the stale build-tree poisoning).
func TestCopyWorkspaceSurfacesGitError(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "main.go"), "package main\n", 0o644)
	// A .git GITFILE pointing at a nonexistent gitdir: os.Stat(.git) succeeds so
	// the git-repo branch is taken, but `git ls-files` exits non-zero, driving
	// the (nil, true, err) return that copyWorkspace must propagate.
	mustWrite(t, filepath.Join(src, ".git"), "gitdir: /nonexistent-bugbot-gitdir\n", 0o644)
	dst := t.TempDir()

	if err := copyWorkspace(src, dst); err == nil {
		t.Fatal("copyWorkspace must return an error when git ls-files fails on a work tree")
	}
}

// TestSanitizeCapturePaths mirrors the sanitizeRelPath contract that
// applyWriteFiles already relies on: escaping paths are rejected up front
// (before a sandbox run is spent), well-formed relative paths are cleaned and
// returned in order.
func TestSanitizeCapturePaths(t *testing.T) {
	t.Run("empty input", func(t *testing.T) {
		got, err := sanitizeCapturePaths(nil)
		if err != nil || got != nil {
			t.Fatalf("sanitizeCapturePaths(nil) = (%v, %v), want (nil, nil)", got, err)
		}
	})

	t.Run("valid relative paths are cleaned and ordered", func(t *testing.T) {
		got, err := sanitizeCapturePaths([]string{".bugbot-repro-junit.xml", "./sub/../report.json"})
		if err != nil {
			t.Fatalf("sanitizeCapturePaths: %v", err)
		}
		want := []string{".bugbot-repro-junit.xml", "report.json"}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("sanitizeCapturePaths = %v, want %v", got, want)
		}
	})

	t.Run("absolute path rejected", func(t *testing.T) {
		if _, err := sanitizeCapturePaths([]string{"/etc/passwd"}); err == nil {
			t.Fatal("expected error for absolute capture path")
		}
	})

	t.Run("escaping .. path rejected", func(t *testing.T) {
		if _, err := sanitizeCapturePaths([]string{"../outside.txt"}); err == nil {
			t.Fatal("expected error for escaping capture path")
		}
	})
}

// TestCaptureWorkspaceFiles covers the post-run read-back: present files are
// returned (capped at maxBytes), missing files are silently absent, and an
// empty paths list yields a nil map.
func TestCaptureWorkspaceFiles(t *testing.T) {
	ws := t.TempDir()
	mustWrite(t, filepath.Join(ws, "report.xml"), "<testsuites></testsuites>", 0o644)
	mustWrite(t, filepath.Join(ws, "big.txt"), "0123456789", 0o644)

	t.Run("nil paths yields nil", func(t *testing.T) {
		if got := captureWorkspaceFiles(ws, nil, 0); got != nil {
			t.Fatalf("captureWorkspaceFiles(nil paths) = %v, want nil", got)
		}
	})

	t.Run("present file captured, missing file silently absent", func(t *testing.T) {
		got := captureWorkspaceFiles(ws, []string{"report.xml", "does-not-exist.xml"}, 0)
		if string(got["report.xml"]) != "<testsuites></testsuites>" {
			t.Fatalf("got[report.xml] = %q", got["report.xml"])
		}
		if _, present := got["does-not-exist.xml"]; present {
			t.Fatal("missing file must be absent from the map, not present with empty/zero value")
		}
	})

	t.Run("capped at maxBytes", func(t *testing.T) {
		got := captureWorkspaceFiles(ws, []string{"big.txt"}, 4)
		if string(got["big.txt"]) != "0123" {
			t.Fatalf("got[big.txt] = %q, want capped to 4 bytes", got["big.txt"])
		}
	})

	t.Run("no captures found yields nil map", func(t *testing.T) {
		got := captureWorkspaceFiles(ws, []string{"nope.txt"}, 0)
		if got != nil {
			t.Fatalf("captureWorkspaceFiles with no hits = %v, want nil", got)
		}
	})
}
