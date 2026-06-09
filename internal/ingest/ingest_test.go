package ingest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// testRepo is a git fixture rooted in a t.TempDir(). All git invocations pin
// user identity inline (git -c ...) so the tests do not depend on the host's
// global git config.
type testRepo struct {
	t   *testing.T
	dir string
}

func newTestRepo(t *testing.T) *testRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	r := &testRepo{t: t, dir: dir}
	// -b main avoids a dependency on the host's init.defaultBranch.
	r.git("init", "-b", "main")
	return r
}

// git runs a git command in the repo with a fixed identity and deterministic
// environment, failing the test on error.
func (r *testRepo) git(args ...string) string {
	r.t.Helper()
	full := append([]string{
		"-C", r.dir,
		"-c", "user.name=Test",
		"-c", "user.email=test@example.com",
		"-c", "commit.gpgsign=false",
		"-c", "init.defaultBranch=main",
	}, args...)
	cmd := exec.Command("git", full...)
	// Isolate from any ambient git env that could change behavior.
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"HOME="+r.dir,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// write creates/overwrites a file (creating parent dirs) relative to the repo.
func (r *testRepo) write(rel, content string) {
	r.t.Helper()
	abs := filepath.Join(r.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// writeBytes writes raw bytes (used for binary fixtures).
func (r *testRepo) writeBytes(rel string, b []byte) {
	r.t.Helper()
	abs := filepath.Join(r.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(abs, b, 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// commit stages everything and commits with the given message, returning the
// new HEAD SHA.
func (r *testRepo) commit(msg string) string {
	r.t.Helper()
	r.git("add", "-A")
	r.git("commit", "-m", msg)
	return strings.TrimSpace(r.git("rev-parse", "HEAD"))
}

func (r *testRepo) open() *Repo {
	r.t.Helper()
	repo, err := Open(context.Background(), r.dir)
	if err != nil {
		r.t.Fatalf("Open: %v", err)
	}
	return repo
}

// ---------------------------------------------------------------------------

func TestOpenRejectsNonRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	if _, err := Open(context.Background(), dir); err == nil {
		t.Fatal("expected error opening non-git directory")
	}
}

func TestSnapshotIncludeExcludeAndBinarySkip(t *testing.T) {
	r := newTestRepo(t)
	r.write("main.go", "package main\n")
	r.write("pkg/util.go", "package pkg\n")
	r.write("docs/readme.md", "# docs\n")
	r.write("script.py", "print('hi')\n")
	r.write("vendor/dep/dep.go", "package dep\n")
	// Binary by extension.
	r.writeBytes("logo.png", []byte("\x89PNG\r\n\x1a\n fake"))
	// Binary by content (null byte), .dat extension not in table.
	r.writeBytes("blob.dat", []byte("text\x00more"))
	// gitignored file must never appear (ls-files is tracked-only anyway).
	r.write(".gitignore", "ignored.txt\n")
	r.write("ignored.txt", "secret\n")
	r.commit("init")

	repo := r.open()

	filter := ScanFilter{
		Include: []string{"**/*.go", "**/*.py"},
		Exclude: []string{"vendor/**"},
	}
	snap, err := repo.Snapshot(context.Background(), filter)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	got := map[string]Language{}
	for _, f := range snap.Files {
		got[f.Path] = f.Language
	}

	wantPresent := map[string]Language{
		"main.go":     LangGo,
		"pkg/util.go": LangGo,
		"script.py":   LangPython,
	}
	for p, lang := range wantPresent {
		if got[p] != lang {
			t.Errorf("expected %q present as %s, got %q", p, lang, got[p])
		}
	}
	for _, p := range []string{
		"docs/readme.md", // excluded by include filter (.md not listed)
		"vendor/dep/dep.go",
		"logo.png",
		"blob.dat",
		"ignored.txt",
		".gitignore",
	} {
		if _, ok := got[p]; ok {
			t.Errorf("did not expect %q in snapshot", p)
		}
	}

	// Sizes populated and sorted order maintained.
	for i := 1; i < len(snap.Files); i++ {
		if snap.Files[i-1].Path > snap.Files[i].Path {
			t.Fatalf("snapshot not sorted: %q before %q", snap.Files[i-1].Path, snap.Files[i].Path)
		}
	}
	if snap.Commit == "" {
		t.Error("snapshot commit empty")
	}
}

func TestSnapshotEmptyIncludeMatchesAll(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.go", "package a\n")
	r.write("b.md", "hi\n")
	r.commit("init")
	repo := r.open()

	snap, err := repo.Snapshot(context.Background(), ScanFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Files) != 2 {
		t.Fatalf("want 2 files with empty include, got %d (%v)", len(snap.Files), snap.Files)
	}
}

func TestFingerprintsAndDiffBetweenCommits(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.go", "package a\nconst A = 1\n")
	r.write("b.go", "package b\nconst B = 1\n")
	r.write("c.go", "package c\nconst C = 1\n")
	c1 := r.commit("c1")

	repo := r.open()
	snap1, err := repo.Snapshot(context.Background(), ScanFilter{})
	if err != nil {
		t.Fatal(err)
	}
	fp1, err := repo.Fingerprints(context.Background(), snap1)
	if err != nil {
		t.Fatal(err)
	}
	if len(fp1) != 3 {
		t.Fatalf("want 3 fingerprints, got %d", len(fp1))
	}

	// Modify b, delete c, add d.
	r.write("b.go", "package b\nconst B = 2\n")
	if err := os.Remove(filepath.Join(r.dir, "c.go")); err != nil {
		t.Fatal(err)
	}
	r.write("d.go", "package d\n")
	c2 := r.commit("c2")
	_ = c1
	_ = c2

	snap2, err := repo.Snapshot(context.Background(), ScanFilter{})
	if err != nil {
		t.Fatal(err)
	}
	fp2, err := repo.Fingerprints(context.Background(), snap2)
	if err != nil {
		t.Fatal(err)
	}

	if fp1["a.go"] != fp2["a.go"] {
		t.Error("a.go fingerprint should be unchanged")
	}
	if fp1["b.go"] == fp2["b.go"] {
		t.Error("b.go fingerprint should differ after edit")
	}

	diff := DiffFingerprints(fp1, fp2)
	assertEqualSet(t, "added", diff.Added, []string{"d.go"})
	assertEqualSet(t, "modified", diff.Modified, []string{"b.go"})
	assertEqualSet(t, "deleted", diff.Deleted, []string{"c.go"})

	// HashBytes consistency with on-disk fingerprint.
	if got := HashBytes([]byte("package a\nconst A = 1\n")); got != fp2["a.go"] {
		t.Errorf("HashBytes mismatch: %s vs %s", got, fp2["a.go"])
	}
}

func TestChangedFilesAddModifyDeleteRename(t *testing.T) {
	r := newTestRepo(t)
	r.write("keep.go", "package keep\n")
	r.write("old/name.go", "package old\nconst X = 1\n")
	r.write("gone.go", "package gone\n")
	from := r.commit("base")

	repo := r.open()

	// Add new, modify keep, delete gone, rename old/name.go -> new/name.go.
	r.write("added.go", "package added\n")
	r.write("keep.go", "package keep\nvar V = 1\n")
	if err := os.Remove(filepath.Join(r.dir, "gone.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(r.dir, "new"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Move with identical content so git detects a rename.
	r.git("mv", "old/name.go", "new/name.go")
	to := r.commit("changes")

	changes, err := repo.ChangedFiles(context.Background(), from, to)
	if err != nil {
		t.Fatalf("ChangedFiles: %v", err)
	}

	byPath := map[string]Change{}
	for _, c := range changes {
		byPath[c.Path] = c
	}

	if c, ok := byPath["added.go"]; !ok || c.Kind != ChangeAdded {
		t.Errorf("added.go: got %+v ok=%v", c, ok)
	}
	if c, ok := byPath["keep.go"]; !ok || c.Kind != ChangeModified {
		t.Errorf("keep.go: got %+v ok=%v", c, ok)
	}
	if c, ok := byPath["gone.go"]; !ok || c.Kind != ChangeDeleted {
		t.Errorf("gone.go: got %+v ok=%v", c, ok)
	}
	if c, ok := byPath["new/name.go"]; !ok || c.Kind != ChangeRenamed || c.OldPath != "old/name.go" {
		t.Errorf("rename: got %+v ok=%v", c, ok)
	}

	// ChangedPaths flattens both rename endpoints.
	paths := ChangedPaths(changes)
	for _, want := range []string{"added.go", "keep.go", "gone.go", "new/name.go", "old/name.go"} {
		if !contains(paths, want) {
			t.Errorf("ChangedPaths missing %q (got %v)", want, paths)
		}
	}
}

func TestPollLocalOnly(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.go", "package a\n")
	c1 := r.commit("first")
	repo := r.open()
	poller := NewPoller(repo, "")

	// First poll with no watermark establishes a baseline, no new commits.
	res, err := poller.Poll(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if res.HeadSHA != c1 {
		t.Errorf("baseline head = %s, want %s", res.HeadSHA, c1)
	}
	if res.HasNew() {
		t.Errorf("baseline poll should report no new commits, got %v", res.NewCommits)
	}

	// Second poll, same head -> no new commits.
	res, err = poller.Poll(context.Background(), c1)
	if err != nil {
		t.Fatal(err)
	}
	if res.HasNew() {
		t.Errorf("unchanged poll should report nothing, got %v", res.NewCommits)
	}

	// Make two new commits, then poll from c1.
	r.write("b.go", "package b\n")
	c2 := r.commit("second commit")
	r.write("c.go", "package c\n")
	c3 := r.commit("third commit")

	res, err = poller.Poll(context.Background(), c1)
	if err != nil {
		t.Fatal(err)
	}
	if res.HeadSHA != c3 {
		t.Errorf("head = %s, want %s", res.HeadSHA, c3)
	}
	if len(res.NewCommits) != 2 {
		t.Fatalf("want 2 new commits, got %d (%v)", len(res.NewCommits), res.NewCommits)
	}
	// Oldest-first ordering.
	if res.NewCommits[0].SHA != c2 || res.NewCommits[1].SHA != c3 {
		t.Errorf("commit order wrong: %v", res.NewCommits)
	}
	if res.NewCommits[0].Summary != "second commit" {
		t.Errorf("summary = %q, want 'second commit'", res.NewCommits[0].Summary)
	}
}

func TestPollRemoteFetch(t *testing.T) {
	// "remote" is a normal repo with commits; "local" clones it. A new commit
	// pushed to the remote should be visible to local's Poll after fetch.
	remote := newTestRepo(t)
	remote.write("a.go", "package a\n")
	remote.commit("r1")

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	localDir := t.TempDir()
	cloneCmd := exec.Command("git",
		"-c", "user.name=Test", "-c", "user.email=test@example.com",
		"clone", "--quiet", remote.dir, localDir)
	cloneCmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "HOME="+localDir)
	if out, err := cloneCmd.CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}

	local := &testRepo{t: t, dir: localDir}
	repo := local.open()
	poller := NewPoller(repo, "origin")

	// Baseline at current upstream tip.
	base, err := poller.Poll(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if base.HeadSHA == "" {
		t.Fatal("empty baseline head")
	}

	// New commit on the remote.
	remote.write("b.go", "package b\n")
	rc2 := remote.commit("r2 remote commit")

	res, err := poller.Poll(context.Background(), base.HeadSHA)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if res.HeadSHA != rc2 {
		t.Errorf("after fetch head = %s, want remote %s", res.HeadSHA, rc2)
	}
	if len(res.NewCommits) != 1 || res.NewCommits[0].SHA != rc2 {
		t.Fatalf("want 1 new commit %s, got %v", rc2, res.NewCommits)
	}
}

func TestBlastRadiusGoImportGraph(t *testing.T) {
	r := newTestRepo(t)
	// Package a (changed), package b imports a, package c does not.
	r.write("a/a.go", "package a\n\nfunc A() int { return 1 }\n")
	r.write("b/b.go", "package b\n\nimport \"github.com/dpoage/bugbot/a\"\n\nfunc B() int { return a.A() }\n")
	r.write("b/more.go", "package b\n\nfunc More() {}\n")
	r.write("c/c.go", "package c\n\nfunc C() {}\n")
	r.commit("init")

	repo := r.open()
	snap, err := repo.Snapshot(context.Background(), ScanFilter{})
	if err != nil {
		t.Fatal(err)
	}

	radius, err := repo.BlastRadius(context.Background(), snap, []string{"a/a.go"})
	if err != nil {
		t.Fatalf("BlastRadius: %v", err)
	}

	// Changed file itself.
	if !contains(radius, "a/a.go") {
		t.Errorf("radius missing changed file a/a.go: %v", radius)
	}
	// Direct dependent: b/b.go imports package a.
	if !contains(radius, "b/b.go") {
		t.Errorf("radius missing dependent b/b.go: %v", radius)
	}
	// c/c.go does not import a; must not appear via the Go graph.
	if contains(radius, "c/c.go") {
		t.Errorf("radius should not include non-dependent c/c.go: %v", radius)
	}
}

func TestBlastRadiusTextualFallback(t *testing.T) {
	r := newTestRepo(t)
	// Non-Go change: helper.py. Another file references the basename "helper".
	r.write("lib/helper.py", "def helper():\n    return 1\n")
	r.write("app/main.py", "from lib import helper\n\nhelper.helper()\n")
	r.write("app/unrelated.py", "x = 1\n")
	r.commit("init")

	repo := r.open()
	snap, err := repo.Snapshot(context.Background(), ScanFilter{})
	if err != nil {
		t.Fatal(err)
	}

	radius, err := repo.BlastRadius(context.Background(), snap, []string{"lib/helper.py"})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(radius, "lib/helper.py") {
		t.Errorf("radius missing changed file: %v", radius)
	}
	if !contains(radius, "app/main.py") {
		t.Errorf("textual fallback missed reference in app/main.py: %v", radius)
	}
	if contains(radius, "app/unrelated.py") {
		t.Errorf("unrelated file should not be in radius: %v", radius)
	}
}

func TestBlastRadiusEmptyChanged(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.go", "package a\n")
	r.commit("init")
	repo := r.open()
	snap, err := repo.Snapshot(context.Background(), ScanFilter{})
	if err != nil {
		t.Fatal(err)
	}
	radius, err := repo.BlastRadius(context.Background(), snap, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(radius) != 0 {
		t.Errorf("empty changed set should yield empty radius, got %v", radius)
	}
}

// ---------------------------------------------------------------------------

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func assertEqualSet(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: got %v, want %v", label, got, want)
		return
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s: got %v, want %v", label, got, want)
			return
		}
	}
}
