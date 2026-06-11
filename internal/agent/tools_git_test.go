package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Fixture git repo helpers (mirroring ingest.testRepo but scoped to agent pkg)
// ---------------------------------------------------------------------------

// gitFixtureRepo builds a minimal real git repo in a t.TempDir() with two
// commits. It returns the repo root path, or skips the test if git is
// unavailable.
func gitFixtureRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()

	runGit := func(args ...string) {
		t.Helper()
		full := append([]string{"-C", dir,
			"-c", "user.name=Test",
			"-c", "user.email=test@example.com",
			"-c", "commit.gpgsign=false",
		}, args...)
		cmd := exec.Command("git", full...)
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_TERMINAL_PROMPT=0",
			"HOME="+dir,
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	write := func(name, content string) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	runGit("init", "-b", "main")
	write("main.go", "package main\n\nfunc main() {\n\tprintln(\"hello\")\n}\n")
	runGit("add", ".")
	runGit("commit", "-m", "initial: add main.go")

	write("main.go", "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"hello\")\n}\n")
	runGit("add", ".")
	runGit("commit", "-m", "fix: use fmt.Println")

	return dir
}

// ---------------------------------------------------------------------------
// git_blame tests
// ---------------------------------------------------------------------------

func TestGitBlame_HappyPath(t *testing.T) {
	dir := gitFixtureRepo(t)
	tool, err := NewGitBlame(dir, nil)
	if err != nil {
		t.Fatalf("NewGitBlame: %v", err)
	}
	ctx := context.Background()

	out, err := tool.Run(ctx, json.RawMessage(`{"path":"main.go","line_start":1,"line_end":3}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Expect blame output: each line should have a commit hash and the line content.
	if !strings.Contains(out, "main.go") && !strings.Contains(out, "main") {
		t.Errorf("expected blame output containing 'main'; got:\n%s", out)
	}
	// The output should not be empty.
	if strings.TrimSpace(out) == "" {
		t.Error("expected non-empty blame output")
	}
}

func TestGitBlame_RangeClamped(t *testing.T) {
	dir := gitFixtureRepo(t)
	tool, err := NewGitBlame(dir, nil)
	if err != nil {
		t.Fatalf("NewGitBlame: %v", err)
	}
	ctx := context.Background()

	// Request more than gitBlameMaxLines.
	raw := json.RawMessage(fmt.Sprintf(`{"path":"main.go","line_start":1,"line_end":%d}`,
		gitBlameMaxLines+50))
	out, err := tool.Run(ctx, raw)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "clamped to") {
		t.Errorf("expected clamped note in output; got:\n%s", out)
	}
}

func TestGitBlame_BadArgs(t *testing.T) {
	dir := gitFixtureRepo(t)
	tool, err := NewGitBlame(dir, nil)
	if err != nil {
		t.Fatalf("NewGitBlame: %v", err)
	}
	ctx := context.Background()

	cases := []struct {
		name string
		raw  string
	}{
		{"bad json", `{`},
		{"empty path", `{"path":"","line_start":1,"line_end":3}`},
		{"start_zero", `{"path":"main.go","line_start":0,"line_end":3}`},
		{"end_before_start", `{"path":"main.go","line_start":5,"line_end":2}`},
		{"traversal", `{"path":"../escape","line_start":1,"line_end":3}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tool.Run(ctx, json.RawMessage(tc.raw)); err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}

func TestGitBlame_UntrackedFile(t *testing.T) {
	dir := gitFixtureRepo(t)
	// Write a file that is not committed.
	if err := os.WriteFile(filepath.Join(dir, "untracked.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool, err := NewGitBlame(dir, nil)
	if err != nil {
		t.Fatalf("NewGitBlame: %v", err)
	}
	ctx := context.Background()

	_, err = tool.Run(ctx, json.RawMessage(`{"path":"untracked.go","line_start":1,"line_end":1}`))
	if err == nil {
		t.Error("expected error for untracked file, got nil")
	}
	if !strings.Contains(err.Error(), "not a git repository or file not tracked") {
		t.Errorf("expected hint in error; got: %v", err)
	}
}

func TestGitBlame_InjectedRunner(t *testing.T) {
	// Verify the injectable runner plumbing with a no-git environment.
	dir := t.TempDir()
	fakeOut := []byte("abc1234 (Test 2024-01-01 1) package main\n")
	runner := func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return fakeOut, nil
	}
	// We need an fsRoot, which requires at least the dir to exist. That's fine.
	tool, err := NewGitBlame(dir, runner)
	if err != nil {
		t.Fatalf("NewGitBlame: %v", err)
	}
	// Write a dummy file so fsRoot.resolve succeeds.
	if err := os.WriteFile(filepath.Join(dir, "foo.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := tool.Run(context.Background(), json.RawMessage(`{"path":"foo.go","line_start":1,"line_end":1}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "abc1234") {
		t.Errorf("expected fake output; got: %s", out)
	}
}

// ---------------------------------------------------------------------------
// git_log tests
// ---------------------------------------------------------------------------

func TestGitLog_HappyPath(t *testing.T) {
	dir := gitFixtureRepo(t)
	tool, err := NewGitLog(dir, nil)
	if err != nil {
		t.Fatalf("NewGitLog: %v", err)
	}
	ctx := context.Background()

	out, err := tool.Run(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Should contain both commit subjects.
	if !strings.Contains(out, "initial: add main.go") {
		t.Errorf("expected first commit subject; got:\n%s", out)
	}
	if !strings.Contains(out, "fix: use fmt.Println") {
		t.Errorf("expected second commit subject; got:\n%s", out)
	}
}

func TestGitLog_PathScoped(t *testing.T) {
	dir := gitFixtureRepo(t)
	tool, err := NewGitLog(dir, nil)
	if err != nil {
		t.Fatalf("NewGitLog: %v", err)
	}
	ctx := context.Background()

	out, err := tool.Run(ctx, json.RawMessage(`{"path":"main.go","max_count":10}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "main.go") || (strings.TrimSpace(out) == "") {
		// The output may not contain "main.go" (git log path scope does not echo
		// the path) but should have commit entries.
		if strings.TrimSpace(out) == "" {
			t.Error("expected non-empty output for main.go path scope")
		}
	}
}

func TestGitLog_MaxCountCapped(t *testing.T) {
	dir := gitFixtureRepo(t)
	// Use an injected runner that captures the git args.
	var captured []string
	runner := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		captured = args
		return []byte("abc1234 2024-01-01 Test fix: something\n"), nil
	}
	tool, err := NewGitLog(dir, runner)
	if err != nil {
		t.Fatalf("NewGitLog: %v", err)
	}

	_, err = tool.Run(context.Background(), json.RawMessage(fmt.Sprintf(`{"max_count":%d}`, gitLogMaxCount+100)))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The -n arg should be capped at gitLogMaxCount.
	found := false
	for _, a := range captured {
		if a == fmt.Sprintf("-n%d", gitLogMaxCount) {
			found = true
		}
	}
	if !found {
		t.Errorf("expected -n%d in git args; got %v", gitLogMaxCount, captured)
	}
}

func TestGitLog_DefaultCount(t *testing.T) {
	dir := gitFixtureRepo(t)
	var captured []string
	runner := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		captured = args
		return []byte("abc1234 2024-01-01 Test fix\n"), nil
	}
	tool, err := NewGitLog(dir, runner)
	if err != nil {
		t.Fatalf("NewGitLog: %v", err)
	}

	_, _ = tool.Run(context.Background(), json.RawMessage(`{}`))
	expected := fmt.Sprintf("-n%d", gitLogDefaultCount)
	found := false
	for _, a := range captured {
		if a == expected {
			found = true
		}
	}
	if !found {
		t.Errorf("expected %s in git args; got %v", expected, captured)
	}
}

func TestGitLog_BadArgs(t *testing.T) {
	dir := gitFixtureRepo(t)
	tool, err := NewGitLog(dir, nil)
	if err != nil {
		t.Fatalf("NewGitLog: %v", err)
	}
	ctx := context.Background()

	if _, err := tool.Run(ctx, json.RawMessage(`{`)); err == nil {
		t.Error("expected error for bad JSON")
	}
}

func TestGitLog_EmptyHistory(t *testing.T) {
	// Injected runner returning empty output → graceful "no commits" message.
	dir := t.TempDir()
	runner := func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return []byte(""), nil
	}
	tool, err := NewGitLog(dir, runner)
	if err != nil {
		t.Fatalf("NewGitLog: %v", err)
	}
	out, err := tool.Run(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "no commits") {
		t.Errorf("expected no-commits message; got: %s", out)
	}
}

func TestGitLog_GitError(t *testing.T) {
	dir := t.TempDir()
	runner := func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return nil, fmt.Errorf("not a git repository")
	}
	tool, err := NewGitLog(dir, runner)
	if err != nil {
		t.Fatalf("NewGitLog: %v", err)
	}
	_, err = tool.Run(context.Background(), json.RawMessage(`{}`))
	if err == nil {
		t.Error("expected error when git fails")
	}
	if !strings.Contains(err.Error(), "not a git repository or no history") {
		t.Errorf("expected hint in error; got: %v", err)
	}
}
