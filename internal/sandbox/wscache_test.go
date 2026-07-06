package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

// TestWorkspaceCacheKeyChangesOnHEAD proves the cache key is sensitive to the
// repo's HEAD commit: committing a new change must produce a different key,
// or a stale pristine would be served for post-commit content.
func TestWorkspaceCacheKeyChangesOnHEAD(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := t.TempDir()
	mustWrite(t, filepath.Join(repo, "a.txt"), "v1\n", 0o644)
	gitInit(t, repo)

	key1, isRepo, err := workspaceCacheKey(repo)
	if err != nil {
		t.Fatalf("workspaceCacheKey: %v", err)
	}
	if !isRepo {
		t.Fatalf("isRepo = false, want true")
	}

	mustWrite(t, filepath.Join(repo, "a.txt"), "v2\n", 0o644)
	gitCommitAll(t, repo, "second")

	key2, _, err := workspaceCacheKey(repo)
	if err != nil {
		t.Fatalf("workspaceCacheKey after commit: %v", err)
	}
	if key1 == key2 {
		t.Errorf("cache key unchanged after HEAD moved: %q", key1)
	}
}

// TestWorkspaceCacheKeyChangesOnDirtyStatus proves the cache key also reacts
// to uncommitted drift (dirty/untracked files), not just HEAD: a commit hash
// alone would serve a stale pristine to a repo with local edits.
func TestWorkspaceCacheKeyChangesOnDirtyStatus(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := t.TempDir()
	mustWrite(t, filepath.Join(repo, "a.txt"), "v1\n", 0o644)
	gitInit(t, repo)

	clean, _, err := workspaceCacheKey(repo)
	if err != nil {
		t.Fatalf("workspaceCacheKey (clean): %v", err)
	}

	// Dirty the tracked file without committing.
	mustWrite(t, filepath.Join(repo, "a.txt"), "v1-dirty\n", 0o644)
	dirty, _, err := workspaceCacheKey(repo)
	if err != nil {
		t.Fatalf("workspaceCacheKey (dirty): %v", err)
	}
	if clean == dirty {
		t.Errorf("cache key unchanged after dirtying tracked file")
	}

	// Revert, then add a new untracked file instead.
	mustWrite(t, filepath.Join(repo, "a.txt"), "v1\n", 0o644)
	mustWrite(t, filepath.Join(repo, "untracked.txt"), "new\n", 0o644)
	untracked, _, err := workspaceCacheKey(repo)
	if err != nil {
		t.Fatalf("workspaceCacheKey (untracked): %v", err)
	}
	if clean == untracked {
		t.Errorf("cache key unchanged after adding an untracked file")
	}
}

// TestWorkspaceCacheKeyBypassedForNonGitDir proves a non-git repoDir reports
// isRepo=false with no error, matching gitWorktreeFiles' fallback contract —
// the caller (CLI.prepareWorkspace) must bypass the cache entirely there.
func TestWorkspaceCacheKeyBypassedForNonGitDir(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a.txt"), "hi\n", 0o644)

	key, isRepo, err := workspaceCacheKey(dir)
	if err != nil {
		t.Fatalf("workspaceCacheKey: %v", err)
	}
	if isRepo {
		t.Errorf("isRepo = true for non-git dir")
	}
	if key != "" {
		t.Errorf("key = %q, want empty for non-git dir", key)
	}
}

// TestCLIPrepareWorkspaceBypassesCacheForNonGitDir proves the CLI-level
// method takes the uncached path (hit always false, and no cache dir is
// created) when RepoDir is not a git work tree.
func TestCLIPrepareWorkspaceBypassesCacheForNonGitDir(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a.txt"), "hi\n", 0o644)

	s := &CLI{}
	ws, hit, err := s.prepareWorkspace(dir, nil)
	if err != nil {
		t.Fatalf("prepareWorkspace: %v", err)
	}
	defer func() { _ = os.RemoveAll(ws) }()
	if hit {
		t.Errorf("hit = true for non-git dir, want false")
	}
	if s.wsCache.dir != "" {
		t.Errorf("wsCache.dir = %q, want empty (cache bypassed)", s.wsCache.dir)
	}
	assertFileContent(t, filepath.Join(ws, "a.txt"), "hi\n")
}

// TestCLIPrepareWorkspaceCachesAcrossCalls proves the cache-aware path: two
// calls against an unchanged git repo report hit=false then hit=true, and a
// commit in between invalidates the cache (hit=false again).
func TestCLIPrepareWorkspaceCachesAcrossCalls(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := t.TempDir()
	mustWrite(t, filepath.Join(repo, "a.txt"), "v1\n", 0o644)
	gitInit(t, repo)

	s := &CLI{}
	defer func() { _ = s.Close() }()

	ws1, hit1, err := s.prepareWorkspace(repo, nil)
	if err != nil {
		t.Fatalf("prepareWorkspace #1: %v", err)
	}
	_ = os.RemoveAll(ws1)
	if hit1 {
		t.Errorf("hit1 = true, want false (first materialization)")
	}

	ws2, hit2, err := s.prepareWorkspace(repo, nil)
	if err != nil {
		t.Fatalf("prepareWorkspace #2: %v", err)
	}
	_ = os.RemoveAll(ws2)
	if !hit2 {
		t.Errorf("hit2 = false, want true (repo unchanged)")
	}

	mustWrite(t, filepath.Join(repo, "a.txt"), "v2\n", 0o644)
	gitCommitAll(t, repo, "second")

	ws3, hit3, err := s.prepareWorkspace(repo, nil)
	if err != nil {
		t.Fatalf("prepareWorkspace #3: %v", err)
	}
	defer func() { _ = os.RemoveAll(ws3) }()
	if hit3 {
		t.Errorf("hit3 = true, want false (repo changed since #1/#2)")
	}
	assertFileContent(t, filepath.Join(ws3, "a.txt"), "v2\n")
}

// TestCloneTreePreservesContentPermsAndSymlinks proves cloneTree (the
// reflink-first fast path) produces output identical to copyTree for content,
// permission bits, and symlink targets — the fast path must be behaviorally
// transparent, not just faster.
func TestCloneTreePreservesContentPermsAndSymlinks(t *testing.T) {
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "exec.sh"), "#!/bin/sh\necho hi\n", 0o755)
	mustWrite(t, filepath.Join(src, "plain.txt"), "hello\n", 0o644)
	mustMkdir(t, filepath.Join(src, "sub"))
	mustWrite(t, filepath.Join(src, "sub", "nested.txt"), "nested\n", 0o600)
	if err := os.Symlink("plain.txt", filepath.Join(src, "link.txt")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	copyDst := t.TempDir()
	if err := copyTree(src, copyDst); err != nil {
		t.Fatalf("copyTree: %v", err)
	}
	cloneDst := t.TempDir()
	if err := cloneTree(src, cloneDst); err != nil {
		t.Fatalf("cloneTree: %v", err)
	}

	for _, rel := range []string{"exec.sh", "plain.txt", "sub/nested.txt"} {
		copyInfo, err := os.Stat(filepath.Join(copyDst, rel))
		if err != nil {
			t.Fatalf("stat copyTree output %s: %v", rel, err)
		}
		cloneInfo, err := os.Stat(filepath.Join(cloneDst, rel))
		if err != nil {
			t.Fatalf("stat cloneTree output %s: %v", rel, err)
		}
		if copyInfo.Mode().Perm() != cloneInfo.Mode().Perm() {
			t.Errorf("%s: perm = %v, want %v (copyTree's)", rel, cloneInfo.Mode().Perm(), copyInfo.Mode().Perm())
		}
		copyContent, err := os.ReadFile(filepath.Join(copyDst, rel))
		if err != nil {
			t.Fatalf("read copyTree output %s: %v", rel, err)
		}
		cloneContent, err := os.ReadFile(filepath.Join(cloneDst, rel))
		if err != nil {
			t.Fatalf("read cloneTree output %s: %v", rel, err)
		}
		if string(copyContent) != string(cloneContent) {
			t.Errorf("%s: content = %q, want %q", rel, cloneContent, copyContent)
		}
	}

	linkFi, err := os.Lstat(filepath.Join(cloneDst, "link.txt"))
	if err != nil {
		t.Fatalf("lstat cloned symlink: %v", err)
	}
	if linkFi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("link.txt cloned as non-symlink: %v", linkFi.Mode())
	}
	if tgt, _ := os.Readlink(filepath.Join(cloneDst, "link.txt")); tgt != "plain.txt" {
		t.Errorf("cloned symlink target = %q, want plain.txt", tgt)
	}
}

// TestCLIPrepareWorkspaceConcurrentRaceFree exercises overlapping
// prepareWorkspace calls against the same cached repo under `go test -race`:
// the wsCache mutex must guard check-and-materialize correctly with no data
// race, regardless of hit/miss interleaving.
func TestCLIPrepareWorkspaceConcurrentRaceFree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := t.TempDir()
	mustWrite(t, filepath.Join(repo, "a.txt"), "v1\n", 0o644)
	gitInit(t, repo)

	s := &CLI{}
	defer func() { _ = s.Close() }()

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	wss := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ws, _, err := s.prepareWorkspace(repo, nil)
			wss[i] = ws
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: prepareWorkspace: %v", i, err)
			continue
		}
		assertFileContent(t, filepath.Join(wss[i], "a.txt"), "v1\n")
		_ = os.RemoveAll(wss[i])
	}
}

// gitCommitAll stages and commits every change in dir. Companion to gitInit
// for tests that need a second commit to move HEAD.
func gitCommitAll(t *testing.T, dir, msg string) {
	t.Helper()
	for _, args := range [][]string{
		{"add", "-A"},
		{"commit", "-q", "-m", msg},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
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
