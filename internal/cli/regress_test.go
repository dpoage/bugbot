package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/ingest"
)

// regressTestRepo is a minimal git fixture for the regress tests. It mirrors
// the testRepo helper in internal/ingest (ingest_test.go) but is duplicated
// here as a small, self-contained helper because the cli package has no
// existing git-fixture helper and importing internal/ingest_test.go's
// unexported helpers would leak implementation across the package boundary.
//
// All git invocations pin user identity inline (git -c ...) so the tests do
// not depend on the host's global git config, and the repo is created with
// an explicit `-b main` to avoid the host's init.defaultBranch preference.
type regressTestRepo struct {
	t   *testing.T
	dir string
}

func newRegressTestRepo(t *testing.T) *regressTestRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	r := &regressTestRepo{t: t, dir: dir}
	r.git("init", "-b", "main")
	return r
}

func (r *regressTestRepo) git(args ...string) string {
	r.t.Helper()
	full := append([]string{
		"-C", r.dir,
		"-c", "user.name=Test",
		"-c", "user.email=test@example.com",
		"-c", "commit.gpgsign=false",
		"-c", "init.defaultBranch=main",
	}, args...)
	cmd := exec.Command("git", full...)
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

func (r *regressTestRepo) write(rel, content string) {
	r.t.Helper()
	abs := filepath.Join(r.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *regressTestRepo) commit(msg string) string {
	r.t.Helper()
	r.git("add", "-A")
	r.git("commit", "-m", msg)
	return strings.TrimSpace(r.git("rev-parse", "HEAD"))
}

func (r *regressTestRepo) open() *ingest.Repo {
	r.t.Helper()
	repo, err := ingest.Open(context.Background(), r.dir)
	if err != nil {
		r.t.Fatalf("ingest.Open: %v", err)
	}
	return repo
}

// TestRegressAttribution exercises printRegressAttribution end-to-end on a
// realistic two-commit fixture. The summary line and section header must
// reflect the per-finding labels, INTRODUCED must appear before
// PRE-EXISTING (the order an operator reads), and the count line must
// match the labeled counts.
func TestRegressAttribution(t *testing.T) {
	r := newRegressTestRepo(t)
	r.write("old.go", "line one\nline two\nline three\n")
	a := r.commit("A: introduce old.go (3 lines)")
	r.write("new.go", "alpha\nbeta\ngamma\n")
	r.commit("B: add new.go")
	repo := r.open()

	findings := []domain.Finding{
		// Anchored to a line beyond old.go's EOF at A (old.go has 3 lines at
		// both A and B). The file existed at A, but the anchored line did not.
		// This is the INTRODUCED-at-finer-granularity case.
		{File: "old.go", Line: 4, Title: "appended-line bug", Tier: 2, Severity: "high"},
		// Anchored to a line that existed at A (line 1 of old.go). PRE-EXISTING.
		{File: "old.go", Line: 1, Title: "preexisting bug", Tier: 2, Severity: "low"},
		// Anchored to a file that did not exist at A. INTRODUCED.
		{File: "new.go", Line: 1, Title: "new-file bug", Tier: 2, Severity: "high"},
	}

	var buf bytes.Buffer
	// Attribution labels findings by their absence at the RANGE'S BASE
	// (--from), which is A — not at the head. Passing B as fromRef would
	// mark everything PRE-EXISTING (everything exists at HEAD).
	printRegressAttribution(context.Background(), &buf, repo, findings, a)

	out := buf.String()
	if !strings.Contains(out, "Regress attribution") {
		t.Errorf("missing attribution header in:\n%s", out)
	}
	// INTRODUCED must appear before PRE-EXISTING in the output so a reader
	// who scans top-to-bottom sees the new bugs first.
	introIdx := strings.Index(out, "INTRODUCED")
	preIdx := strings.Index(out, "PRE-EXISTING")
	if introIdx < 0 {
		t.Errorf("INTRODUCED label missing in:\n%s", out)
	}
	if preIdx < 0 {
		t.Errorf("PRE-EXISTING label missing in:\n%s", out)
	}
	if introIdx > preIdx {
		t.Errorf("INTRODUCED must precede PRE-EXISTING; got intro=%d pre=%d in:\n%s", introIdx, preIdx, out)
	}
	// Count line: 2 introduced, 1 pre-existing.
	if !strings.Contains(out, "2 introduced, 1 pre-existing since "+a) {
		t.Errorf("summary count line missing or wrong; got:\n%s", out)
	}
	// Each finding must appear with its anchored file:line.
	for _, f := range findings {
		if !strings.Contains(out, f.File+":") {
			t.Errorf("output missing file %q in:\n%s", f.File, out)
		}
	}
}

// TestRegressAttribution_NoFindingsIsNoOp ensures an empty finding list
// emits no section header / count line — the regress summary stays terse on
// a clean run.
func TestRegressAttribution_NoFindingsIsNoOp(t *testing.T) {
	r := newRegressTestRepo(t)
	r.write("a.go", "package a\n")
	r.commit("init")
	repo := r.open()

	var buf bytes.Buffer
	printRegressAttribution(context.Background(), &buf, repo, nil, "HEAD")
	if buf.Len() != 0 {
		t.Errorf("expected empty output for nil findings; got: %q", buf.String())
	}
}
