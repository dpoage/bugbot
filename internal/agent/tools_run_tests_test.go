package agent

import (
	"context"
	"encoding/json"
	"os/exec"
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

// --- Bash-wrapped ctest/meson (compound `bash -c` baseCmd) ------------------

func TestRunTestsTool_BashCtestArgv_WithRun(t *testing.T) {
	fs := &fakeSandbox{}
	base := []string{"bash", "-c", "cmake --build build && ctest --test-dir build"}
	tool := NewRunTestsTool(fs, "/repo", base, 3, nil, nil, nil, nil)
	_, _ = runTestsTool(t, tool, map[string]interface{}{"run": "TestFoo"})

	// The -R filter is injected immediately after the ctest token; the rest
	// of the script (cmake invocation, --test-dir flag) is preserved.
	want := []string{"bash", "-c", "cmake --build build && ctest -R 'TestFoo' --test-dir build"}
	if !equalSlice(fs.calls[0].Cmd, want) {
		t.Errorf("argv = %v, want %v", fs.calls[0].Cmd, want)
	}
}

func TestRunTestsTool_BashMesonArgv_WithRun(t *testing.T) {
	fs := &fakeSandbox{}
	base := []string{"bash", "-c", "meson test -C build"}
	tool := NewRunTestsTool(fs, "/repo", base, 3, nil, nil, nil, nil)
	_, _ = runTestsTool(t, tool, map[string]interface{}{"run": "suite_a"})

	// meson test takes the test name as a positional arg, not a flag.
	want := []string{"bash", "-c", "meson test 'suite_a' -C build"}
	if !equalSlice(fs.calls[0].Cmd, want) {
		t.Errorf("argv = %v, want %v", fs.calls[0].Cmd, want)
	}
}

func TestRunTestsTool_BashArgv_NoRun_ReturnsBaseVerbatim(t *testing.T) {
	fs := &fakeSandbox{}
	base := []string{"bash", "-c", "cmake --build build && ctest"}
	tool := NewRunTestsTool(fs, "/repo", base, 3, nil, nil, nil, nil)
	_, _ = runTestsTool(t, tool, map[string]interface{}{})

	// No run filter supplied: leave the compound command alone.
	want := base
	if !equalSlice(fs.calls[0].Cmd, want) {
		t.Errorf("argv = %v, want %v", fs.calls[0].Cmd, want)
	}
}

func TestRunTestsTool_BashArgv_UnexpectedShape_ReturnsBaseVerbatim(t *testing.T) {
	fs := &fakeSandbox{}
	// baseCmd is not the ["bash", "-c", script] shape we recognize.
	base := []string{"bash", "echo", "hello"}
	tool := NewRunTestsTool(fs, "/repo", base, 3, nil, nil, nil, nil)
	_, _ = runTestsTool(t, tool, map[string]interface{}{"run": "TestFoo"})

	// Shape mismatch: graceful degradation, base cmd returned verbatim.
	if !equalSlice(fs.calls[0].Cmd, base) {
		t.Errorf("argv = %v, want %v", fs.calls[0].Cmd, base)
	}
}

func TestRunTestsTool_BashArgv_UnrecognizedScript_ReturnsBaseVerbatim(t *testing.T) {
	fs := &fakeSandbox{}
	// Valid ["bash", "-c", script] shape but the script invokes neither
	// ctest nor `meson test`. Narrowing would risk producing a broken argv.
	base := []string{"bash", "-c", "cmake --build build && ./run_tests.sh"}
	tool := NewRunTestsTool(fs, "/repo", base, 3, nil, nil, nil, nil)
	_, _ = runTestsTool(t, tool, map[string]interface{}{"run": "TestFoo"})

	if !equalSlice(fs.calls[0].Cmd, base) {
		t.Errorf("argv = %v, want %v", fs.calls[0].Cmd, base)
	}
}

func TestRunTestsTool_BashMesonArgv_NotMatchedInWord_ReturnsBaseVerbatim(t *testing.T) {
	fs := &fakeSandbox{}
	// "meson testing" is not a "meson test" invocation: the right-hand word
	// boundary check rejects it, so the script is returned verbatim rather
	// than spliced mid-word into "meson test 'suite_a'ing".
	base := []string{"bash", "-c", "meson testing --foo"}
	tool := NewRunTestsTool(fs, "/repo", base, 3, nil, nil, nil, nil)
	_, _ = runTestsTool(t, tool, map[string]interface{}{"run": "suite_a"})
	if !equalSlice(fs.calls[0].Cmd, base) {
		t.Errorf("argv = %v, want verbatim %v", fs.calls[0].Cmd, base)
	}
}

func TestRunTestsTool_BashArgv_CtestInIdentifier_NotInjected(t *testing.T) {
	fs := &fakeSandbox{}
	// `ctest_helper` is an identifier that contains "ctest" as a substring.
	// We must NOT inject `-R` into it.
	base := []string{"bash", "-c", "./ctest_helper build && echo done"}
	tool := NewRunTestsTool(fs, "/repo", base, 3, nil, nil, nil, nil)
	_, _ = runTestsTool(t, tool, map[string]interface{}{"run": "TestFoo"})

	// No recognizable ctest invocation -> base cmd verbatim.
	if !equalSlice(fs.calls[0].Cmd, base) {
		t.Errorf("argv = %v, want %v", fs.calls[0].Cmd, base)
	}
}

func TestRunTestsTool_BashArgv_MaliciousRun_QuotedSafely(t *testing.T) {
	fs := &fakeSandbox{}
	// baseCmd has a recognizable bare `ctest` invocation followed by a
	// fake flag. The point of the test is the single-quoting: if we ever
	// splice the unquoted run value into the script, bash would see the
	// `;` as a command terminator and run `echo PWNED`.
	base := []string{"bash", "-c", "ctest_does_not_exist_xyz ; ctest --no-such-flag-xyz"}
	tool := NewRunTestsTool(fs, "/repo", base, 3, nil, nil, nil, nil)

	// Classic single-quote-escape payload. If we accidentally splat the
	// unquoted value into the script, bash will see the `;` as a command
	// separator and execute `echo PWNED`.
	malicious := "INJECTED; echo PWNED; #"
	_, _ = runTestsTool(t, tool, map[string]interface{}{"run": malicious})

	if len(fs.calls) != 1 {
		t.Fatalf("expected 1 sandbox call, got %d", len(fs.calls))
	}
	script := fs.calls[0].Cmd[2]

	// Structural check: the produced script is exactly the expected
	// single-quoted form. If shellSingleQuote is bypassed, the literal
	// payload would appear unquoted and this string equality fails.
	wantScript := "ctest_does_not_exist_xyz ; ctest -R 'INJECTED; echo PWNED; #' --no-such-flag-xyz"
	if script != wantScript {
		t.Errorf("script = %q, want %q", script, wantScript)
	}

	// Functional check: when bash executes the produced script, the
	// injected `echo PWNED` must not run. The ctest binary may or may
	// not be installed; in either case, the literal string "PWNED" must
	// not appear in the script's stdout or stderr. (ctest, even if
	// present, only sees `INJECTED; echo PWNED; #` as a single argument
	// passed via `-R` — it cannot execute it as a shell command.)
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available on this host")
	}
	out, _ := exec.Command("bash", "-c", script).CombinedOutput()
	if strings.Contains(string(out), "PWNED") {
		t.Errorf("malicious run value escaped single-quoting; bash output:\n%s", out)
	}
}

// --- shellSingleQuote --------------------------------------------------------

func TestShellSingleQuote_PosixSafe(t *testing.T) {
	// Direct unit test of the helper. The contract: for any input, the
	// result, when embedded in a single-quoted shell context, parses to
	// exactly the input. We verify by having bash print the result back
	// via `printf %s`. If the quoting is sound, what bash prints equals
	// the input. If not, the printed string diverges.
	cases := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"plain", "TestFoo"},
		{"spaces", "a b c"},
		{"single_quote", "it's"},
		{"two_single_quotes", "a'b'c"},
		{"double_quote", `say "hi"`},
		{"dollar", "$HOME"},
		{"backtick", "`uname`"},
		{"backslash", `a\b`},
		{"semicolon_breakout_attempt", "INJECTED; echo PWNED; #"},
		{"embedded_newline", "line1\nline2"},
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available on this host")
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			quoted := shellSingleQuote(tc.input)
			// Run bash and have it echo the single-quoted value back to us.
			// If the quoting is sound, the bytes printed equal tc.input.
			cmd := exec.Command("bash", "-c", "printf %s "+quoted)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("bash failed: %v (output: %q)", err, out)
			}
			if string(out) != tc.input {
				t.Errorf("roundtrip: got %q, want %q", out, tc.input)
			}
		})
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
