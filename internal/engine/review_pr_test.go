package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initReviewTestRepo creates a minimal git repo with a single commit and
// returns its directory. ReviewPR's precondition check only needs a resolvable
// HEAD, so this is deliberately smaller than internal/ingest's testRepo
// harness (which is unexported and cannot be reused from internal/engine).
func initReviewTestRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(cmd.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q")
	run("config", "user.name", "test")
	run("config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "init")
	return dir
}

// TestReviewPR_HeadMismatchIsPlainError verifies that a local checkout whose
// HEAD does not equal the PR's head SHA surfaces as a plain returned error
// (never a panic) carrying both SHAs and a recovery hint — the precondition a
// caller like the TUI dispatch palette must be able to show on its status
// line without crashing the cockpit.
func TestReviewPR_HeadMismatchIsPlainError(t *testing.T) {
	dir := initReviewTestRepo(t)
	ctx := context.Background()
	cfg := testConfig(t)

	d, err := Open(ctx, cfg, nil)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = d.Close() }()

	gh := newFakeGH().on("pulls/9", prPayload("baseSHA", "some-other-head-sha", "feature", 9))

	res, err := d.ReviewPR(ctx, ReviewPROpts{
		Target:    dir,
		PRNumber:  9,
		Suspected: "summary",
		GH:        gh.run,
	})
	if err == nil {
		t.Fatal("expected an error for a HEAD/PR-head mismatch, got nil")
	}
	if res != nil {
		t.Errorf("expected a nil result on error, got %+v", res)
	}
	if !strings.Contains(err.Error(), "does not match PR #9 head") {
		t.Errorf("error should name the mismatch clearly: %v", err)
	}
	if !strings.Contains(err.Error(), "some-other") {
		t.Errorf("error should reference the PR head SHA: %v", err)
	}
	// The funnel/gh-write path must never have been reached: only the single
	// resolvePR call should have gone out.
	if len(gh.calls) != 1 {
		t.Errorf("expected exactly one gh call (resolvePR), got %d: %v", len(gh.calls), gh.calls)
	}
}
