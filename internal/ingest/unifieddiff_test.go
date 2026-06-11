package ingest

import (
	"context"
	"strings"
	"testing"
)

// TestUnifiedDiff_ProducesHunks confirms UnifiedDiff returns a parseable
// three-dot diff between two commits, with the post-image path in the +++
// header and an added line present.
func TestUnifiedDiff_ProducesHunks(t *testing.T) {
	r := newTestRepo(t)
	r.write("main.go", "package main\n\nfunc main() {}\n")
	base := r.commit("base")

	r.write("main.go", "package main\n\nfunc main() { println(\"x\") }\n")
	head := r.commit("head")

	repo := r.open()
	diff, err := repo.UnifiedDiff(context.Background(), base, head)
	if err != nil {
		t.Fatalf("UnifiedDiff: %v", err)
	}
	s := string(diff)
	if !strings.Contains(s, "+++ b/main.go") {
		t.Errorf("diff should name the post-image path:\n%s", s)
	}
	if !strings.Contains(s, "@@") {
		t.Errorf("diff should contain a hunk header:\n%s", s)
	}
	if !strings.Contains(s, "println") {
		t.Errorf("diff should contain the added content:\n%s", s)
	}
}

// TestUnifiedDiff_RequiresBothCommits guards the input contract.
func TestUnifiedDiff_RequiresBothCommits(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.go", "package a\n")
	r.commit("c")
	repo := r.open()
	if _, err := repo.UnifiedDiff(context.Background(), "", "HEAD"); err == nil {
		t.Error("expected error when from is empty")
	}
}

// TestCommitMessage_ReturnsFullMessage confirms CommitMessage round-trips the
// full commit message body (subject and body if any) for a given commit ref.
func TestCommitMessage_ReturnsFullMessage(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.go", "package a\n")
	sha := r.commit("validate input before use\n\nThis is the body.")

	repo := r.open()
	msg, err := repo.CommitMessage(context.Background(), sha)
	if err != nil {
		t.Fatalf("CommitMessage: %v", err)
	}
	if !strings.Contains(msg, "validate input before use") {
		t.Errorf("message missing subject: %q", msg)
	}
	if !strings.Contains(msg, "This is the body.") {
		t.Errorf("message missing body: %q", msg)
	}
}

// TestCommitMessage_RequiresCommit guards the input contract.
func TestCommitMessage_RequiresCommit(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.go", "package a\n")
	r.commit("c")
	repo := r.open()
	if _, err := repo.CommitMessage(context.Background(), ""); err == nil {
		t.Error("expected error when commit ref is empty")
	}
}

// TestCommitMessage_HEAD works with symbolic ref HEAD.
func TestCommitMessage_HEAD(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.go", "package a\n")
	r.commit("the HEAD commit message")

	repo := r.open()
	msg, err := repo.CommitMessage(context.Background(), "HEAD")
	if err != nil {
		t.Fatalf("CommitMessage HEAD: %v", err)
	}
	if !strings.Contains(msg, "the HEAD commit message") {
		t.Errorf("HEAD message mismatch: %q", msg)
	}
}
