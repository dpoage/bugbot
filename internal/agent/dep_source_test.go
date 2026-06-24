package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestDepSourceRoots_EmptyIsNoOp pins that a nil / empty DepSourceRoots set
// rejects every read: a host with no ecosystems installed must produce the
// "path escapes all dep-source roots" error, never a panic, and never a
// false accept that would let a read escape into the host filesystem.
func TestDepSourceRoots_EmptyIsNoOp(t *testing.T) {
	d := &DepSourceRoots{}
	if d.Len() != 0 {
		t.Errorf("empty DepSourceRoots Len = %d, want 0", d.Len())
	}
	if roots := d.Roots(); roots != nil {
		t.Errorf("empty DepSourceRoots Roots = %v, want nil", roots)
	}
	_, err := d.resolve("any/path")
	if !errors.Is(err, errDepPathEscape) {
		t.Errorf("resolve on empty set err = %v, want errDepPathEscape", err)
	}

	// nil receiver must be safe (defensive: callers may pass nil opt-in).
	var nilD *DepSourceRoots
	if nilD.Len() != 0 {
		t.Errorf("nil DepSourceRoots Len = %d, want 0", nilD.Len())
	}
	if _, err := nilD.resolve("any/path"); !errors.Is(err, errDepPathEscape) {
		t.Errorf("nil receiver resolve err = %v, want errDepPathEscape", err)
	}
}

// TestDepSourceRoots_ResolvesInRoot verifies a path under a configured root
// resolves correctly. The test constructs a temp dir as a single root, then
// resolves known files under it. This is the unit test for the "GOROOT /
// module cache path resolves" half of mi5.18 AC3.
func TestDepSourceRoots_ResolvesInRoot(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "lib.go"), "package lib\nfunc F() {}\n")
	mustMkdir(t, filepath.Join(root, "sub"))
	mustWrite(t, filepath.Join(root, "sub", "inner.go"), "package sub\n")

	d := &DepSourceRoots{roots: []string{root}}
	if d.Len() != 1 {
		t.Fatalf("Len = %d, want 1", d.Len())
	}
	got, err := d.resolve("lib.go")
	if err != nil {
		t.Fatalf("resolve lib.go: %v", err)
	}
	if got != filepath.Join(root, "lib.go") {
		t.Errorf("resolve lib.go = %q, want %q", got, filepath.Join(root, "lib.go"))
	}

	got, err = d.resolve("sub/inner.go")
	if err != nil {
		t.Fatalf("resolve sub/inner.go: %v", err)
	}
	if got != filepath.Join(root, "sub", "inner.go") {
		t.Errorf("resolve sub/inner.go = %q, want %q", got, filepath.Join(root, "sub", "inner.go"))
	}
}

// TestDepSourceRoots_RejectsEscape verifies that a `..` traversal from any
// root is rejected. This is the unit test for the "escape from any root is
// rejected" half of mi5.18 AC3.
func TestDepSourceRoots_RejectsEscape(t *testing.T) {
	root := t.TempDir()
	// Build a file outside the root that an attacker would try to reach.
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "secret.txt"), "sensitive\n")

	d := &DepSourceRoots{roots: []string{root}}
	for _, p := range []string{
		"../" + filepath.Base(outside) + "/secret.txt",
		"sub/../../" + filepath.Base(outside) + "/secret.txt",
		"/" + filepath.Base(outside) + "/secret.txt",
	} {
		_, err := d.resolve(p)
		if !errors.Is(err, errDepPathEscape) {
			t.Errorf("resolve %q err = %v, want errDepPathEscape", p, err)
		}
	}
}

// TestDepSourceRoots_SymlinkEscapeRejected verifies that a symlink under a
// root that points OUTSIDE the root is also rejected. Mirrors the in-repo
// fsRoot.symlink containment test, applied to the dep-source variant.
func TestDepSourceRoots_SymlinkEscapeRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks are unreliable on Windows; in-repo tests already gate on this")
	}
	root := t.TempDir()
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "secret.txt"), "sensitive\n")
	// Symlink: root/escape -> outside
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	d := &DepSourceRoots{roots: []string{root}}
	_, err := d.resolve("escape/secret.txt")
	if !errors.Is(err, errDepPathEscape) {
		t.Errorf("symlink escape err = %v, want errDepPathEscape", err)
	}
}

// TestDepSourceRoots_DiscoveredGoRootExistsIsUsable exercises the
// NewDepSourceRoots discovery path against a real env probe. It does NOT
// require GOROOT to be set (it is on any Go install); it simply asserts
// that when GOROOT is set and the path is valid, the root appears in the
// set. A host without the Go toolchain returns an empty set without
// error.
func TestDepSourceRoots_DiscoveredGoRootExistsIsUsable(t *testing.T) {
	d := NewDepSourceRoots()
	if d == nil {
		t.Fatal("NewDepSourceRoots returned nil")
	}
	// All roots must exist as directories and be absolute.
	for _, r := range d.Roots() {
		if !filepath.IsAbs(r) {
			t.Errorf("root %q is not absolute", r)
		}
		info, err := os.Stat(r)
		if err != nil {
			t.Errorf("discovered root %q not accessible: %v", r, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("discovered root %q is not a directory", r)
		}
	}
	// When GOROOT is set, NewDepSourceRoots must include GOROOT/src.
	if goroot := strings.TrimSpace(os.Getenv("GOROOT")); goroot != "" {
		expected := filepath.Join(goroot, "src")
		found := false
		for _, r := range d.Roots() {
			if r == expected {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("NewDepSourceRoots missing GOROOT/src (%q); got %v", expected, d.Roots())
		}
	}
}

// TestNewReadFileWithDepRoots_InRepoStillWorks pins the AC4 invariant: a
// tool constructed with dep-source roots behaves EXACTLY like the in-repo
// variant for in-repo paths.
func TestNewReadFileWithDepRoots_InRepoStillWorks(t *testing.T) {
	repo := t.TempDir()
	mustWrite(t, filepath.Join(repo, "main.go"), "package main\nfunc main() {}\n")
	dep := t.TempDir()
	mustWrite(t, filepath.Join(dep, "lib.go"), "package lib\n")

	tool, err := NewReadFileWithDepRoots(repo, ReadCaps{}, &DepSourceRoots{roots: []string{dep}})
	if err != nil {
		t.Fatalf("NewReadFileWithDepRoots: %v", err)
	}
	out, err := tool.Run(context.Background(), []byte(`{"path":"main.go"}`))
	if err != nil {
		t.Fatalf("read_file in-repo: %v", err)
	}
	if !strings.Contains(out, "package main") {
		t.Errorf("read_file in-repo output missing content; got: %q", out)
	}
}

// TestNewReadFileWithDepRoots_ReadsDepSource verifies a path under a
// configured dep-source root reads. Combines the constructor's tool wiring
// with DepSourceRoots.resolve to prove the end-to-end "verifier can read
// outside the repo" claim.
func TestNewReadFileWithDepRoots_ReadsDepSource(t *testing.T) {
	repo := t.TempDir()
	mustWrite(t, filepath.Join(repo, "main.go"), "package main\nfunc main() {}\n")
	dep := t.TempDir()
	mustWrite(t, filepath.Join(dep, "stdlib.go"), "package stdlib\nfunc F() {}\n")

	tool, err := NewReadFileWithDepRoots(repo, ReadCaps{}, &DepSourceRoots{roots: []string{dep}})
	if err != nil {
		t.Fatalf("NewReadFileWithDepRoots: %v", err)
	}
	out, err := tool.Run(context.Background(), []byte(`{"path":"stdlib.go"}`))
	if err != nil {
		t.Fatalf("read_file dep-source: %v", err)
	}
	if !strings.Contains(out, "package stdlib") {
		t.Errorf("read_file dep-source output missing content; got: %q", out)
	}
}

// TestNewReadFileWithDepRoots_RejectsEscapesFromDepRoot verifies the
// constructor's tool wiring still rejects `..` from the dep-source root
// (i.e. a path that escapes the dep-source root is not allowed any more
// than one that escapes the repo root).
func TestNewReadFileWithDepRoots_RejectsEscapesFromDepRoot(t *testing.T) {
	repo := t.TempDir()
	mustWrite(t, filepath.Join(repo, "main.go"), "package main\nfunc main() {}\n")
	dep := t.TempDir()
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "secret.txt"), "sensitive\n")

	tool, err := NewReadFileWithDepRoots(repo, ReadCaps{}, &DepSourceRoots{roots: []string{dep}})
	if err != nil {
		t.Fatalf("NewReadFileWithDepRoots: %v", err)
	}
	_, err = tool.Run(context.Background(), []byte(`{"path":"../`+filepath.Base(outside)+`/secret.txt"}`))
	if err == nil {
		t.Errorf("escape from dep root succeeded; want error")
	}
	if !strings.Contains(err.Error(), "escapes") {
		t.Errorf("escape err = %v, want it to mention escape", err)
	}
}

