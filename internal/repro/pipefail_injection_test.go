package repro

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/sandbox"
)

// TestInjectPipefail covers bugbot-2zoo's shape recognition directly: a
// bash -c (and absolute-path / flag-cluster variant) script gains a
// `set -o pipefail; ` prefix unless it already mentions pipefail; sh -c and
// plain argv are left completely alone, mirroring ecosystem.UnwrapShell's
// shell-wrapper recognition (bash/sh, absolute paths, flag clusters ending
// in "c") minus sh (dash has no pipefail builtin).
func TestInjectPipefail(t *testing.T) {
	cases := []struct {
		name string
		cmd  []string
		want []string
	}{
		{
			name: "bash -c script gains the prefix",
			cmd:  []string{"bash", "-c", "node --test x.test.js 2>&1 | tail -60"},
			want: []string{"bash", "-c", "set -o pipefail; node --test x.test.js 2>&1 | tail -60"},
		},
		{
			name: "absolute /bin/bash -c is handled the same as bare bash",
			cmd:  []string{"/bin/bash", "-c", "go test ./... | grep FAIL"},
			want: []string{"/bin/bash", "-c", "set -o pipefail; go test ./... | grep FAIL"},
		},
		{
			name: "combined flag cluster bash -lc is handled",
			cmd:  []string{"bash", "-lc", "npm test | tail -60"},
			want: []string{"bash", "-lc", "set -o pipefail; npm test | tail -60"},
		},
		{
			name: "sh -c is NEVER touched (dash has no pipefail)",
			cmd:  []string{"sh", "-c", "node --test x.test.js 2>&1 | tail -60"},
			want: []string{"sh", "-c", "node --test x.test.js 2>&1 | tail -60"},
		},
		{
			name: "absolute /bin/sh -c is NEVER touched either",
			cmd:  []string{"/bin/sh", "-c", "pytest | tail -60"},
			want: []string{"/bin/sh", "-c", "pytest | tail -60"},
		},
		{
			name: "plain argv (no shell wrapper) is untouched",
			cmd:  []string{"go", "test", "-run", "TestBug", "./..."},
			want: []string{"go", "test", "-run", "TestBug", "./..."},
		},
		{
			name: "script already mentioning pipefail is not double-wrapped",
			cmd:  []string{"bash", "-c", "set -eo pipefail; node --test x.test.js | tail -60"},
			want: []string{"bash", "-c", "set -eo pipefail; node --test x.test.js | tail -60"},
		},
		{
			name: "bash without -c flag is untouched",
			cmd:  []string{"bash", "x.sh"},
			want: []string{"bash", "x.sh"},
		},
		{
			name: "empty cmd is untouched",
			cmd:  nil,
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := injectPipefail(tc.cmd)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("injectPipefail(%v) = %v, want %v", tc.cmd, got, tc.want)
			}
		})
	}
}

// TestApplyStructuredOutputSpec_InjectsPipefail asserts the pipefail
// injection rides on the SAME shared seam (applyStructuredOutputSpec,
// bugbot-0zay/bugbot-2zoo) both buildSpec (execute()'s official run) and
// runExec (workspace-tool preview) call, and that it composes correctly
// with the pre-existing structured-output rewrite: a bash -c wrapped
// command (which normalizeCmdForStructuredOutput deliberately leaves
// untouched, see runnerevents.go) still gets the pipefail prefix, while a
// direct `go test`/`pytest` invocation (never a shell wrapper) is
// unaffected by the new injection.
func TestApplyStructuredOutputSpec_InjectsPipefail(t *testing.T) {
	cases := []struct {
		name    string
		cmd     []string
		wantCmd []string
	}{
		{
			name:    "bash -c test-runner-through-tail gains pipefail",
			cmd:     []string{"bash", "-c", "node --test x.test.js 2>&1 | tail -60"},
			wantCmd: []string{"bash", "-c", "set -o pipefail; node --test x.test.js 2>&1 | tail -60"},
		},
		{
			name:    "direct go test is untouched by pipefail injection, still gains -json",
			cmd:     []string{"go", "test", "-run", "TestBug", "./..."},
			wantCmd: []string{"go", "test", "-json", "-run", "TestBug", "./..."},
		},
		{
			name:    "sh -c wrapped pytest is untouched",
			cmd:     []string{"sh", "-c", "pytest -k test_bug | tail -60"},
			wantCmd: []string{"sh", "-c", "pytest -k test_bug | tail -60"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := applyStructuredOutputSpec(sandbox.Spec{}, tc.cmd)
			if !reflect.DeepEqual(spec.Cmd, tc.wantCmd) {
				t.Errorf("spec.Cmd = %v, want %v", spec.Cmd, tc.wantCmd)
			}
		})
	}
}

// TestWorkspaceTool_ExecInjectsPipefail is the preview-side end-to-end
// mirror of the live transcript (20260716T153203, the_cloud): an agent
// bash -c's a failing `node --test` through `tail -60`. The mock's
// ResponseFunc is keyed on whether the spec it actually received carries
// the pipefail prefix — exactly the transcript's failure mode, where the
// UNPATCHED command would report tail's exit 0 (falsely "clean") and only
// the pipefail-prefixed command reports the real non-zero failure. The
// call is then asserted directly to have carried the injected script, and
// the rendered preview is asserted to classify the run as demonstrated.
func TestWorkspaceTool_ExecInjectsPipefail(t *testing.T) {
	repoDir := newRepoDir(t)
	sb := newFakeMaterializingSandbox(sandbox.MockResponse{})
	sb.ResponseFunc = func(_ int, spec sandbox.Spec) (sandbox.Result, error) {
		script := spec.Cmd[len(spec.Cmd)-1]
		if strings.Contains(script, "set -o pipefail") {
			// Honest: the pipeline now reports node's failing exit code.
			return sandbox.Result{ExitCode: 1, Stdout: "not ok 1 - x.test.js\n# fail 1\nFAIL"}, nil
		}
		// Unpatched: tail's own exit code (0) masks the upstream failure.
		return sandbox.Result{ExitCode: 0, Stdout: "not ok 1 - x.test.js\n# fail 1\nFAIL"}, nil
	}
	tool := newWorkspaceTool(sb, repoDir, 1, &iterationWorkspace{})

	cmd := []string{"bash", "-c", "node --test x.test.js 2>&1 | tail -60"}
	out, err := tool.Run(context.Background(), mustCmdArgs(t, cmd))
	if err != nil {
		t.Fatalf("workspace exec: %v", err)
	}

	calls := sb.Calls()
	if len(calls) != 1 {
		t.Fatalf("sandbox call count = %d, want 1", len(calls))
	}
	gotScript := calls[0].Spec.Cmd[2]
	wantScript := "set -o pipefail; node --test x.test.js 2>&1 | tail -60"
	if gotScript != wantScript {
		t.Errorf("spec.Cmd[2] = %q, want %q", gotScript, wantScript)
	}
	if !strings.Contains(out, "exit_code=1") {
		t.Errorf("preview output = %q, want exit_code=1 (pipefail-honest failure)", out)
	}
}

// TestExecute_InjectsPipefail is the official-run (buildSpec/execute)
// mirror: the same bash -c pipeline reaches the sandbox via
// Reproducer.execute with the pipefail prefix injected, proving the two
// call sites (workspace-tool preview, execute()'s clean-room run) get
// IDENTICAL treatment through the shared applyStructuredOutputSpec seam.
func TestExecute_InjectsPipefail(t *testing.T) {
	repoDir := newRepoDir(t)
	sb := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 1, Stdout: "not ok 1 - x.test.js\nFAIL"}})
	r := &Reproducer{
		sb:      sb,
		repoDir: repoDir,
		opts:    Options{Timeout: 30 * time.Second}.resolve(),
	}

	plan := &Plan{
		Files: map[string]string{"x.test.js": "// repro\n"},
		Cmd:   []string{"bash", "-c", "node --test x.test.js 2>&1 | tail -60"},
	}
	if _, err := r.execute(context.Background(), plan); err != nil {
		t.Fatalf("execute: %v", err)
	}

	calls := sb.Calls()
	if len(calls) != 1 {
		t.Fatalf("sandbox call count = %d, want 1", len(calls))
	}
	want := []string{"bash", "-c", "set -o pipefail; node --test x.test.js 2>&1 | tail -60"}
	if !reflect.DeepEqual(calls[0].Spec.Cmd, want) {
		t.Errorf("spec.Cmd = %v, want %v", calls[0].Spec.Cmd, want)
	}
}

// TestInterpret_PipefailSIGPIPE_NoFalsePromote is the required
// interpret()-level regression for the oracle-review defect on bugbot-2zoo:
// injectPipefail makes a `bash -c` pipeline report the FAILING stage's exit
// code instead of the last stage's — but when an agent bounds output with
// an EARLY-TERMINATING filter (head -N, grep -m1) instead of a
// read-to-EOF one (tail, plain grep), a genuinely PASSING upstream runner
// can be killed by SIGPIPE (exit 128+13=141) once the filter is satisfied
// and closes its end of the pipe. Rust's "test result:" and JS's
// "test suites:" ran-markers used to match that PASSING summary just as
// readily as a failing one, so a passing run could reach the ran-evidence
// gate (interpret.go step 4) and be misclassified demonstrated=true. Fixed
// by failure-qualifying/removing those two markers (internal/ecosystem/
// interp.go); this test pins that a passing run under SIGPIPE stays
// not_demonstrated while a genuinely failing run (ordinary non-zero exit,
// no SIGPIPE involved) still demonstrates.
func TestInterpret_PipefailSIGPIPE_NoFalsePromote(t *testing.T) {
	t.Run("rust_passing_under_sigpipe_not_demonstrated", func(t *testing.T) {
		// `set -o pipefail; cargo test 2>&1 | head -60` on a PASSING crate:
		// head exits after its 60 lines, cargo (still writing doctest
		// output) is killed by SIGPIPE -> exit 141, but the unit-test
		// summary already read by head shows a clean pass.
		res := sandbox.Result{
			ExitCode: 141,
			Stdout:   "running 2 tests\ntest tests::adds_one ... ok\ntest tests::adds_two ... ok\n\ntest result: ok. 2 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out; finished in 0.01s\n\n",
		}
		v := interpret(res, []string{"bash", "-c", "cargo test 2>&1 | head -60"})
		if v.demonstrated {
			t.Errorf("passing cargo test under SIGPIPE must NOT demonstrate; got demonstrated=true, summary=%q", v.summary)
		}
		if v.reason != VerdictReasonNotDemonstrated {
			t.Errorf("reason = %q, want not_demonstrated", v.reason)
		}
	})

	t.Run("rust_failing_demonstrated", func(t *testing.T) {
		res := sandbox.Result{
			ExitCode: 101,
			Stdout:   "running 2 tests\ntest tests::adds_one ... FAILED\ntest tests::adds_two ... ok\n\ntest result: FAILED. 1 passed; 1 failed; 0 ignored; 0 measured; 0 filtered out; finished in 0.01s\n\n",
		}
		v := interpret(res, []string{"bash", "-c", "cargo test"})
		if !v.demonstrated {
			t.Errorf("genuinely failing cargo test must demonstrate; got reason=%q", v.reason)
		}
	})

	t.Run("js_passing_under_sigpipe_not_demonstrated", func(t *testing.T) {
		// `set -o pipefail; npx jest 2>&1 | grep -m1 -i suites` on a
		// PASSING suite: grep exits after its first match, jest is killed
		// by SIGPIPE -> exit 141, but the summary line grep captured shows
		// a clean pass.
		res := sandbox.Result{
			ExitCode: 141,
			Stdout:   "Test Suites: 1 passed, 1 total\n",
		}
		v := interpret(res, []string{"bash", "-c", "npx jest 2>&1 | grep -m1 -i suites"})
		if v.demonstrated {
			t.Errorf("passing jest run under SIGPIPE must NOT demonstrate; got demonstrated=true, summary=%q", v.summary)
		}
		if v.reason != VerdictReasonNotDemonstrated {
			t.Errorf("reason = %q, want not_demonstrated", v.reason)
		}
	})

	t.Run("js_failing_demonstrated", func(t *testing.T) {
		res := sandbox.Result{
			ExitCode: 1,
			Stdout:   "FAIL src/add.test.js\n  ✕ sums numbers (3 ms)\n  ● sums numbers\n    expect(received).toBe(expected)\n\nTests:       1 failed, 1 passed, 2 total\nTest Suites: 1 failed, 1 total\n",
		}
		v := interpret(res, []string{"bash", "-c", "npx jest"})
		if !v.demonstrated {
			t.Errorf("genuinely failing jest run must demonstrate; got reason=%q", v.reason)
		}
	})
}
