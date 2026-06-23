package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/sandbox"
)

// --- helpers -----------------------------------------------------------------

func newRunTestsTool(results []sandbox.Result, baseCmd []string, maxExec int) (*RunTestsTool, *fakeSandbox) {
	fs := &fakeSandbox{results: results}
	tool := NewRunTestsTool(fs, "/repo", baseCmd, maxExec, nil, nil, nil, nil)
	return tool, fs
}

func runTestsTool(t *testing.T, tool *RunTestsTool, args interface{}) (string, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return tool.Run(context.Background(), raw)
}

// --- Def ---------------------------------------------------------------------

func TestRunTestsTool_Def(t *testing.T) {
	tool, _ := newRunTestsTool(nil, []string{"go", "test", "./..."}, 3)
	def := tool.Def()
	if def.Name != "run_tests" {
		t.Errorf("Name = %q, want run_tests", def.Name)
	}
	if def.Description == "" {
		t.Error("Description must be non-empty")
	}
	if len(def.Parameters) == 0 {
		t.Error("Parameters schema must be non-empty")
	}
}

// --- Normal run --------------------------------------------------------------

func TestRunTestsTool_NormalRun_ZeroExit(t *testing.T) {
	result := sandbox.Result{
		ExitCode: 0,
		Stdout:   "ok\tgithub.com/x\n",
		Duration: 50 * time.Millisecond,
	}
	tool, fs := newRunTestsTool([]sandbox.Result{result}, []string{"go", "test", "./..."}, 3)

	out, err := runTestsTool(t, tool, map[string]interface{}{})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if !strings.Contains(out, "exit_code=0") {
		t.Errorf("output missing exit_code=0: %q", out)
	}
	if !strings.Contains(out, "ok") {
		t.Errorf("output missing stdout content: %q", out)
	}
	if len(fs.calls) != 1 {
		t.Errorf("sandbox called %d times, want 1", len(fs.calls))
	}
	if fs.calls[0].Network != "none" {
		t.Errorf("network = %q, want none", fs.calls[0].Network)
	}
	if fs.calls[0].RepoDir != "/repo" {
		t.Errorf("repoDir = %q, want /repo", fs.calls[0].RepoDir)
	}
}

func TestRunTestsTool_NonZeroExit_IsNormalResult(t *testing.T) {
	result := sandbox.Result{
		ExitCode: 1,
		Stdout:   "--- FAIL: TestFoo\n",
		Duration: 20 * time.Millisecond,
	}
	tool, _ := newRunTestsTool([]sandbox.Result{result}, []string{"go", "test", "./..."}, 3)

	out, err := runTestsTool(t, tool, map[string]interface{}{})
	if err != nil {
		t.Fatalf("non-zero exit must not be a tool error; got: %v", err)
	}
	if !strings.Contains(out, "exit_code=1") {
		t.Errorf("output missing exit_code=1: %q", out)
	}
}

// --- Budget ------------------------------------------------------------------

func TestRunTestsTool_BudgetEnforced(t *testing.T) {
	fs := &fakeSandbox{}
	tool := NewRunTestsTool(fs, "/repo", []string{"go", "test", "./..."}, 2, nil, nil, nil, nil)

	args := map[string]interface{}{}
	for i := 0; i < 2; i++ {
		if _, err := runTestsTool(t, tool, args); err != nil {
			t.Fatalf("call %d should succeed: %v", i+1, err)
		}
	}
	// Third call must be rejected.
	_, err := runTestsTool(t, tool, args)
	if err == nil {
		t.Fatal("third call should return a budget-exhausted error")
	}
	if !strings.Contains(err.Error(), "budget exhausted") {
		t.Errorf("error should mention budget exhausted: %v", err)
	}
	// Sandbox must have been called exactly twice (budget check fires before 3rd exec).
	if len(fs.calls) != 2 {
		t.Errorf("sandbox called %d times, want 2", len(fs.calls))
	}
}

func TestRunTestsTool_ExecCount(t *testing.T) {
	tool, _ := newRunTestsTool(nil, []string{"go", "test", "./..."}, 5)
	if tool.ExecCount() != 0 {
		t.Errorf("initial count = %d, want 0", tool.ExecCount())
	}
	_, _ = runTestsTool(t, tool, map[string]interface{}{})
	if tool.ExecCount() != 1 {
		t.Errorf("after 1 run count = %d, want 1", tool.ExecCount())
	}
}

// --- Output cap --------------------------------------------------------------

func TestRunTestsTool_StdoutTruncation(t *testing.T) {
	bigOut := strings.Repeat("x", sandboxOutputCap+100)
	result := sandbox.Result{ExitCode: 0, Stdout: bigOut, Duration: 1 * time.Millisecond}
	tool, _ := newRunTestsTool([]sandbox.Result{result}, []string{"go", "test", "./..."}, 3)

	out, err := runTestsTool(t, tool, map[string]interface{}{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "[stdout truncated") {
		t.Errorf("large stdout must be truncated: %q", out[:200])
	}
}

// --- Argv building -----------------------------------------------------------

func TestRunTestsTool_GoArgv_Defaults(t *testing.T) {
	fs := &fakeSandbox{}
	tool := NewRunTestsTool(fs, "/repo", []string{"go", "test", "./..."}, 3, nil, nil, nil, nil)
	_, _ = runTestsTool(t, tool, map[string]interface{}{})

	want := []string{"go", "test", "./..."}
	got := fs.calls[0].Cmd
	if !equalSlice(got, want) {
		t.Errorf("argv = %v, want %v", got, want)
	}
}

func TestRunTestsTool_GoArgv_WithPkg(t *testing.T) {
	fs := &fakeSandbox{}
	tool := NewRunTestsTool(fs, "/repo", []string{"go", "test", "./..."}, 3, nil, nil, nil, nil)
	_, _ = runTestsTool(t, tool, map[string]interface{}{"pkg": "./internal/store/..."})

	want := []string{"go", "test", "./internal/store/..."}
	if !equalSlice(fs.calls[0].Cmd, want) {
		t.Errorf("argv = %v, want %v", fs.calls[0].Cmd, want)
	}
}

func TestRunTestsTool_GoArgv_WithRun(t *testing.T) {
	fs := &fakeSandbox{}
	tool := NewRunTestsTool(fs, "/repo", []string{"go", "test", "./..."}, 3, nil, nil, nil, nil)
	_, _ = runTestsTool(t, tool, map[string]interface{}{"run": "TestFoo"})

	want := []string{"go", "test", "./...", "-run", "TestFoo"}
	if !equalSlice(fs.calls[0].Cmd, want) {
		t.Errorf("argv = %v, want %v", fs.calls[0].Cmd, want)
	}
}

func TestRunTestsTool_GoArgv_WithPkgAndRun(t *testing.T) {
	fs := &fakeSandbox{}
	tool := NewRunTestsTool(fs, "/repo", []string{"go", "test", "./..."}, 3, nil, nil, nil, nil)
	_, _ = runTestsTool(t, tool, map[string]interface{}{"pkg": "./internal/store/...", "run": "TestFoo"})

	want := []string{"go", "test", "./internal/store/...", "-run", "TestFoo"}
	if !equalSlice(fs.calls[0].Cmd, want) {
		t.Errorf("argv = %v, want %v", fs.calls[0].Cmd, want)
	}
}

func TestRunTestsTool_CargoArgv_Defaults(t *testing.T) {
	fs := &fakeSandbox{}
	tool := NewRunTestsTool(fs, "/repo", []string{"cargo", "test"}, 3, nil, nil, nil, nil)
	_, _ = runTestsTool(t, tool, map[string]interface{}{})

	want := []string{"cargo", "test"}
	if !equalSlice(fs.calls[0].Cmd, want) {
		t.Errorf("argv = %v, want %v", fs.calls[0].Cmd, want)
	}
}

func TestRunTestsTool_CargoArgv_WithRun(t *testing.T) {
	fs := &fakeSandbox{}
	tool := NewRunTestsTool(fs, "/repo", []string{"cargo", "test"}, 3, nil, nil, nil, nil)
	_, _ = runTestsTool(t, tool, map[string]interface{}{"run": "test_divide"})

	want := []string{"cargo", "test", "test_divide"}
	if !equalSlice(fs.calls[0].Cmd, want) {
		t.Errorf("argv = %v, want %v", fs.calls[0].Cmd, want)
	}
}

func TestRunTestsTool_PytestArgv_WithPkgAndRun(t *testing.T) {
	fs := &fakeSandbox{}
	tool := NewRunTestsTool(fs, "/repo", []string{"python", "-m", "pytest"}, 3, nil, nil, nil, nil)
	_, _ = runTestsTool(t, tool, map[string]interface{}{"pkg": "tests/", "run": "test_add"})

	want := []string{"python", "-m", "pytest", "tests/", "-k", "test_add"}
	if !equalSlice(fs.calls[0].Cmd, want) {
		t.Errorf("argv = %v, want %v", fs.calls[0].Cmd, want)
	}
}

func TestRunTestsTool_NpmArgv_Unchanged(t *testing.T) {
	fs := &fakeSandbox{}
	tool := NewRunTestsTool(fs, "/repo", []string{"npm", "test"}, 3, nil, nil, nil, nil)
	_, _ = runTestsTool(t, tool, map[string]interface{}{"pkg": "foo", "run": "bar"})

	// npm test doesn't support inline filters — base cmd returned verbatim.
	want := []string{"npm", "test"}
	if !equalSlice(fs.calls[0].Cmd, want) {
		t.Errorf("argv = %v, want %v", fs.calls[0].Cmd, want)
	}
}

func TestRunTestsTool_BazelArgv_WithPkg(t *testing.T) {
	fs := &fakeSandbox{}
	base := []string{"bazel", "test", "--build_tests_only", "--test_output=errors", "//..."}
	tool := NewRunTestsTool(fs, "/repo", base, 3, nil, nil, nil, nil)
	_, _ = runTestsTool(t, tool, map[string]interface{}{"pkg": "//internal/store/..."})

	// Narrowing preserves every base-command flag and only swaps the final
	// target token for the supplied package pattern.
	want := []string{"bazel", "test", "--build_tests_only", "--test_output=errors", "//internal/store/..."}
	if !equalSlice(fs.calls[0].Cmd, want) {
		t.Errorf("argv = %v, want %v", fs.calls[0].Cmd, want)
	}
}

func TestRunTestsTool_BazelArgv_NoPkg_ReturnsBaseVerbatim(t *testing.T) {
	fs := &fakeSandbox{}
	base := []string{"bazel", "test", "--build_tests_only", "--test_output=errors", "//..."}
	tool := NewRunTestsTool(fs, "/repo", base, 3, nil, nil, nil, nil)
	_, _ = runTestsTool(t, tool, map[string]interface{}{})

	// No pkg filter: the canonical base argv is returned verbatim.
	want := []string{"bazel", "test", "--build_tests_only", "--test_output=errors", "//..."}
	if !equalSlice(fs.calls[0].Cmd, want) {
		t.Errorf("argv = %v, want %v", fs.calls[0].Cmd, want)
	}
}

// --- No base command ---------------------------------------------------------

func TestRunTestsTool_NilBaseCmd_ReturnsError(t *testing.T) {
	fs := &fakeSandbox{}
	tool := NewRunTestsTool(fs, "/repo", nil, 3, nil, nil, nil, nil)
	_, err := runTestsTool(t, tool, map[string]interface{}{})
	if err == nil {
		t.Fatal("nil base command must return an error")
	}
	if !strings.Contains(err.Error(), "no test command") {
		t.Errorf("error = %v, want 'no test command' message", err)
	}
}

// --- onExec hook -------------------------------------------------------------

func TestRunTestsTool_OnExecCalled(t *testing.T) {
	var called int
	var got time.Duration
	onExec := func(d time.Duration) {
		called++
		got = d
	}
	result := sandbox.Result{ExitCode: 0, Duration: 77 * time.Millisecond}
	fs := &fakeSandbox{results: []sandbox.Result{result}}
	tool := NewRunTestsTool(fs, "/repo", []string{"go", "test", "./..."}, 3, nil, nil, nil, onExec)

	if _, err := runTestsTool(t, tool, map[string]interface{}{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called != 1 {
		t.Errorf("onExec called %d times, want 1", called)
	}
	if got != 77*time.Millisecond {
		t.Errorf("onExec duration = %v, want 77ms", got)
	}
}

func TestRunTestsTool_OnExecNotCalledOnBudgetExhausted(t *testing.T) {
	var called int
	onExec := func(time.Duration) { called++ }
	fs := &fakeSandbox{}
	tool := NewRunTestsTool(fs, "/repo", []string{"go", "test", "./..."}, 0, nil, nil, nil, onExec)

	_, _ = runTestsTool(t, tool, map[string]interface{}{})
	if called != 0 {
		t.Errorf("onExec must not be called on budget-exhausted path, called %d times", called)
	}
}

// --- SetupCmds propagation ---------------------------------------------------

func TestRunTestsTool_SetupCmdsPropagate(t *testing.T) {
	fs := &fakeSandbox{}
	setup := [][]string{{"go", "mod", "download"}}
	tool := NewRunTestsTool(fs, "/repo", []string{"go", "test", "./..."}, 3, nil, nil, setup, nil)
	_, _ = runTestsTool(t, tool, map[string]interface{}{})

	if len(fs.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(fs.calls))
	}
	if len(fs.calls[0].SetupCmds) != 1 || fs.calls[0].SetupCmds[0][0] != "go" {
		t.Errorf("SetupCmds not propagated: %v", fs.calls[0].SetupCmds)
	}
}

// --- helpers -----------------------------------------------------------------

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
