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
