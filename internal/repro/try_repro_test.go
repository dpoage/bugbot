package repro

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/sandbox"
)

// fakeMaterializingSandbox wraps *sandbox.Mock (for scripted Exec responses)
// and adds MaterializeWorkspace so unit tests can exercise try_repro's
// iteration-workspace lifecycle without a real container runtime.
// MaterializeWorkspace creates a REAL temp directory (so tests can assert it
// exists, and later that it was removed); Exec itself stays fully scripted
// via the embedded Mock.
type fakeMaterializingSandbox struct {
	*sandbox.Mock

	mu   sync.Mutex
	made []string
}

func newFakeMaterializingSandbox(def sandbox.MockResponse) *fakeMaterializingSandbox {
	return &fakeMaterializingSandbox{Mock: sandbox.NewMock(def)}
}

func (f *fakeMaterializingSandbox) MaterializeWorkspace(repoDir string) (string, error) {
	dir, err := os.MkdirTemp("", "bugbot-test-iterws-")
	if err != nil {
		return "", err
	}
	f.mu.Lock()
	f.made = append(f.made, dir)
	f.mu.Unlock()
	return dir, nil
}

// materialized returns every directory MaterializeWorkspace has returned, in
// call order.
func (f *fakeMaterializingSandbox) materialized() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.made))
	copy(out, f.made)
	return out
}

var (
	_ sandbox.Sandbox       = (*fakeMaterializingSandbox)(nil)
	_ workspaceMaterializer = (*fakeMaterializingSandbox)(nil)
)

// newTryReproTool builds a TryReproTool wired to sb for tests, with no
// dependency-strategy extras and the given per-attempt budget.
func newTryReproTool(sb *fakeMaterializingSandbox, repoDir string, maxExec int, ws *iterationWorkspace) *TryReproTool {
	return NewTryReproTool(sb, repoDir, "", 30*time.Second, nil, nil, nil, sb.MaterializeWorkspace, ws, maxExec)
}

func mustArgs(t *testing.T, files map[string]string, cmd []string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(tryReproArgs{Files: files, Cmd: cmd})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return b
}

// TestTryReproTool_BudgetExhausted verifies that a call beyond maxExec
// returns a recoverable tool error naming the limit, without reaching the
// sandbox (CallCount stays at maxExec).
func TestTryReproTool_BudgetExhausted(t *testing.T) {
	sb := newFakeMaterializingSandbox(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 1, Stdout: "--- FAIL: TestX\nFAIL"}})
	repoDir := newRepoDir(t)
	tool := newTryReproTool(sb, repoDir, 2, &iterationWorkspace{})

	args := mustArgs(t, map[string]string{"x_test.go": "package bug\n"}, []string{"cat", "x_test.go"})

	for i := 0; i < 2; i++ {
		if _, err := tool.Run(context.Background(), args); err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
	}
	_, err := tool.Run(context.Background(), args)
	if err == nil {
		t.Fatal("3rd call should exceed the budget of 2")
	}
	if !strings.Contains(err.Error(), "try_repro budget exhausted") || !strings.Contains(err.Error(), "2/2") {
		t.Errorf("budget error = %q, want it to name the limit (2/2)", err.Error())
	}
	if sb.CallCount() != 2 {
		t.Errorf("sandbox CallCount = %d, want 2 (budget-exceeded call must not reach the sandbox)", sb.CallCount())
	}
}

// TestTryReproTool_ValidationErrorsAreRecoverable verifies that files/cmd
// violating the SAME rules validatePlan applies (path escape, no files, no
// cmd) are surfaced as ordinary (non-budget) tool errors, and that a
// rejected call still counts against the budget (matching run_tests/
// sandbox_exec's behavior of counting every Run invocation).
func TestTryReproTool_ValidationErrorsAreRecoverable(t *testing.T) {
	sb := newFakeMaterializingSandbox(sandbox.MockResponse{})
	repoDir := newRepoDir(t)
	tool := newTryReproTool(sb, repoDir, 5, &iterationWorkspace{})

	cases := []struct {
		name string
		args json.RawMessage
		want string
	}{
		{"no files", mustArgs(t, nil, []string{"cat", "x"}), "no repro files"},
		{"no cmd", mustArgs(t, map[string]string{"x": "y"}, nil), "no command"},
		{"path escape", mustArgs(t, map[string]string{"../escape.go": "y"}, []string{"cat", "x"}), "workspace-relative"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := tool.Run(context.Background(), c.args)
			if err == nil {
				t.Fatalf("expected a validation error, got none")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), c.want)
			}
		})
	}
	// None of the invalid calls should have reached the sandbox.
	if sb.CallCount() != 0 {
		t.Errorf("sandbox CallCount = %d, want 0 (all calls were rejected before Exec)", sb.CallCount())
	}
}

// TestTryReproTool_WorkspaceMaterializedOnceAndReused verifies that the
// iteration workspace is materialized on the FIRST call and the exact same
// directory is reused (passed as Spec.Workspace) on every later call within
// the same holder, matching the "one persistent workspace per attempt"
// design.
func TestTryReproTool_WorkspaceMaterializedOnceAndReused(t *testing.T) {
	sb := newFakeMaterializingSandbox(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 1, Stdout: "--- FAIL: TestX\nFAIL"}})
	repoDir := newRepoDir(t)
	ws := &iterationWorkspace{}
	tool := newTryReproTool(sb, repoDir, 5, ws)

	args1 := mustArgs(t, map[string]string{"x_test.go": "v1"}, []string{"cat", "x_test.go"})
	args2 := mustArgs(t, map[string]string{"x_test.go": "v2"}, []string{"cat", "x_test.go"})

	if _, err := tool.Run(context.Background(), args1); err != nil {
		t.Fatalf("call 1: %v", err)
	}
	if _, err := tool.Run(context.Background(), args2); err != nil {
		t.Fatalf("call 2: %v", err)
	}

	made := sb.materialized()
	if len(made) != 1 {
		t.Fatalf("MaterializeWorkspace called %d times, want 1 (materialize once, reuse thereafter)", len(made))
	}

	calls := sb.Calls()
	if len(calls) != 2 {
		t.Fatalf("sandbox calls = %d, want 2", len(calls))
	}
	if calls[0].Spec.Workspace == "" || calls[0].Spec.Workspace != calls[1].Spec.Workspace {
		t.Errorf("Workspace differs across calls: call0=%q call1=%q, want identical non-empty path",
			calls[0].Spec.Workspace, calls[1].Spec.Workspace)
	}
	if calls[0].Spec.Workspace != made[0] {
		t.Errorf("Spec.Workspace = %q, want the materialized path %q", calls[0].Spec.Workspace, made[0])
	}
	// RepoDir is still threaded through so a non-git fallback works, but
	// Workspace is what Exec actually uses (see sandbox.Spec.Workspace doc).
	if calls[0].Spec.RepoDir != repoDir {
		t.Errorf("Spec.RepoDir = %q, want %q", calls[0].Spec.RepoDir, repoDir)
	}
}

// TestTryReproTool_RendersClassificationAndTail verifies that Run's textual
// result carries the exit code, the interpret()-style demonstrated/reason
// classification, and a tail excerpt of the combined output.
func TestTryReproTool_RendersClassificationAndTail(t *testing.T) {
	sb := newFakeMaterializingSandbox(sandbox.MockResponse{Result: sandbox.Result{
		ExitCode: 1,
		Stdout:   "--- FAIL: TestBug\n    bug_test.go:1: boom\nFAIL",
	}})
	repoDir := newRepoDir(t)
	tool := newTryReproTool(sb, repoDir, 5, &iterationWorkspace{})

	args := mustArgs(t, map[string]string{"bug_test.go": "package bug\n"},
		[]string{"go", "test", "-timeout", "60s", "-run", "TestBug", "./..."})
	out, err := tool.Run(context.Background(), args)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "exit_code=1") {
		t.Errorf("output missing exit_code: %q", out)
	}
	if !strings.Contains(out, "demonstrated=true") {
		t.Errorf("output missing demonstrated=true classification: %q", out)
	}
	if !strings.Contains(out, "--- FAIL: TestBug") {
		t.Errorf("output missing tail excerpt of combined output: %q", out)
	}
}

// TestTryReproTool_NotDemonstratedReasonSurfaced verifies a non-demonstrating
// run (e.g. exit 0) reports demonstrated=false with its reason, so the agent
// gets the same signal validatePlan/interpret would give the final plan.
func TestTryReproTool_NotDemonstratedReasonSurfaced(t *testing.T) {
	sb := newFakeMaterializingSandbox(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 0, Stdout: "PASS"}})
	repoDir := newRepoDir(t)
	tool := newTryReproTool(sb, repoDir, 5, &iterationWorkspace{})

	args := mustArgs(t, map[string]string{"bug_test.go": "package bug\n"},
		[]string{"go", "test", "-timeout", "60s", "-run", "TestBug", "./..."})
	out, err := tool.Run(context.Background(), args)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "demonstrated=false") {
		t.Errorf("output missing demonstrated=false: %q", out)
	}
	if !strings.Contains(out, "reason=exit_zero") {
		t.Errorf("output missing reason=exit_zero: %q", out)
	}
}

// TestAttempt_IterationWorkspaceRemovedAfterReturn verifies that an Attempt
// that calls try_repro leaves the iteration workspace materialized during the
// run but removes it (via the deferred cleanup) before Attempt returns — the
// official clean-room verdict must never be able to observe it afterward.
func TestAttempt_IterationWorkspaceRemovedAfterReturn(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	finding := seedFinding(t, st)
	repoDir := newRepoDir(t)
	artifactDir := t.TempDir()

	// The scripted client calls try_repro once, then submits the final plan.
	client := newToolScriptedClient(
		toolCallStep("c1", "try_repro", string(mustArgs(t, goodPlan().Files, goodPlan().Cmd))),
		textStep(planBody(t, goodPlan())),
	)

	sb := newFakeMaterializingSandbox(sandbox.MockResponse{Result: sandbox.Result{
		ExitCode: 1,
		Stdout:   "--- FAIL: TestBug\nFAIL",
	}})

	r, err := New(client, sb, repoDir, Options{ArtifactDir: artifactDir})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()

	att, err := r.Attempt(ctx, finding)
	if err != nil {
		t.Fatalf("Attempt: %v", err)
	}
	if !att.Promoted {
		t.Fatalf("Attempt not promoted: %+v", att)
	}

	made := sb.materialized()
	if len(made) != 1 {
		t.Fatalf("MaterializeWorkspace called %d times, want exactly 1", len(made))
	}
	if _, statErr := os.Stat(made[0]); !os.IsNotExist(statErr) {
		t.Errorf("iteration workspace %q still exists after Attempt returned (stat err = %v), want removed", made[0], statErr)
	}
}

// TestAttempt_NoTryReproCallLeavesWorkspaceUnmaterialized verifies that an
// Attempt whose agent never calls try_repro never materializes an iteration
// workspace at all (zero extra disk cost for the common single-shot case).
func TestAttempt_NoTryReproCallLeavesWorkspaceUnmaterialized(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	finding := seedFinding(t, st)
	repoDir := newRepoDir(t)
	artifactDir := t.TempDir()

	client := newScriptedClient(planBody(t, goodPlan()))
	sb := newFakeMaterializingSandbox(sandbox.MockResponse{Result: sandbox.Result{
		ExitCode: 1,
		Stdout:   "--- FAIL: TestBug\nFAIL",
	}})

	r, err := New(client, sb, repoDir, Options{ArtifactDir: artifactDir})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()

	att, err := r.Attempt(ctx, finding)
	if err != nil {
		t.Fatalf("Attempt: %v", err)
	}
	if !att.Promoted {
		t.Fatalf("Attempt not promoted: %+v", att)
	}
	if made := sb.materialized(); len(made) != 0 {
		t.Errorf("MaterializeWorkspace called %d times, want 0 (try_repro was never invoked)", len(made))
	}
}
