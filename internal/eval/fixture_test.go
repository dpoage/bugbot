package eval

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/dpoage/bugbot/internal/ingest"
)

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

func TestMaterialize_RealGitRepo(t *testing.T) {
	requireGit(t)
	dir, err := materialize(FixtureSpec{Files: map[string]string{
		"a.go":        "package fixture\n",
		"sub/b.go":    "package sub\n",
		"nested/x.go": "package nested\n",
	}})
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	defer cleanup(dir)

	// Files materialized, including nested dirs.
	for _, rel := range []string{"a.go", "sub/b.go", "nested/x.go"} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("missing fixture file %q: %v", rel, err)
		}
	}

	// It is a real git repo with a commit, and ingest can open it.
	repo, err := ingest.Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("ingest.Open on fixture: %v", err)
	}
	snap, err := repo.Snapshot(context.Background(), ingest.ScanFilter{})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.Commit == "" {
		t.Errorf("snapshot has no commit; fixture not committed")
	}
	if len(snap.Files) == 0 {
		t.Errorf("snapshot has no files")
	}
}

func TestMaterialize_EmptyFiles_Errors(t *testing.T) {
	if _, err := materialize(FixtureSpec{}); err == nil {
		t.Errorf("expected error for fixture with no files")
	}
}

func TestMaterialize_PathTraversalRejected(t *testing.T) {
	requireGit(t)
	_, err := materialize(FixtureSpec{Files: map[string]string{
		"../escape.go": "package x\n",
	}})
	if err == nil {
		t.Errorf("expected error for path escaping repo root")
	}
}

func TestMaterialize_Base_CopiedAndOverridden(t *testing.T) {
	requireGit(t)
	base := t.TempDir()
	// A base file that Files will override, and one it won't.
	if err := os.WriteFile(filepath.Join(base, "keep.go"), []byte("package base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "over.go"), []byte("OLD"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A .git dir in the base must NOT be copied.
	if err := os.MkdirAll(filepath.Join(base, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, ".git", "config"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}

	dir, err := materialize(FixtureSpec{
		Base:  base,
		Files: map[string]string{"over.go": "NEW"},
	})
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	defer cleanup(dir)

	if b, _ := os.ReadFile(filepath.Join(dir, "keep.go")); string(b) != "package base\n" {
		t.Errorf("base file not copied: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "over.go")); string(b) != "NEW" {
		t.Errorf("Files did not override base: %q", b)
	}
	// The fixture's own .git (from gitInitCommit) exists, but it must not contain
	// the base's junk config.
	if b, _ := os.ReadFile(filepath.Join(dir, ".git", "config")); string(b) == "junk" {
		t.Errorf("base .git leaked into fixture")
	}
}
