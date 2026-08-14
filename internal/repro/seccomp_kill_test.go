package repro

import (
	"strings"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/sandbox"
)

// realGoTestJSONSeccompKilled is a real `go test -json` shape (matching
// runnerevents_test.go's realGoTestJSONFailing fixture format) for a
// helper subprocess killed by the bwrap seccomp arch-mismatch guard: the
// PARENT go test process is unaffected (it forked the killed process, saw
// a normal Go runtime error from exec.Cmd.Run, and reported it via
// t.Fatalf — an ordinary per-test "fail" action, structurally
// indistinguishable from a genuine assertion failure to classifyGoEvents).
// This is oracle finding B2's exact manufactured-finding shape: without
// the veto running BEFORE interpret()'s structured-output path, this
// fixture alone would demonstrate=true via the JSON decoder, with the
// seccompArchKillTextMarker text never even being consulted.
const realGoTestJSONSeccompKilled = `{"Time":"2026-08-14T10:00:00Z","Action":"start","Package":"fixture"}
{"Time":"2026-08-14T10:00:00Z","Action":"run","Package":"fixture","Test":"TestHelperKilled"}
{"Time":"2026-08-14T10:00:00Z","Action":"output","Package":"fixture","Test":"TestHelperKilled","Output":"=== RUN   TestHelperKilled\n"}
{"Time":"2026-08-14T10:00:00Z","Action":"output","Package":"fixture","Test":"TestHelperKilled","Output":"    helper_test.go:12: helper: signal: bad system call (core dumped)\n"}
{"Time":"2026-08-14T10:00:00Z","Action":"output","Package":"fixture","Test":"TestHelperKilled","Output":"--- FAIL: TestHelperKilled (0.22s)\n"}
{"Time":"2026-08-14T10:00:00Z","Action":"fail","Package":"fixture","Test":"TestHelperKilled","Elapsed":0.22}
{"Time":"2026-08-14T10:00:00Z","Action":"output","Package":"fixture","Output":"FAIL\n"}
{"Time":"2026-08-14T10:00:00Z","Action":"output","Package":"fixture","Output":"FAIL\tfixture\t0.22s\n"}
{"Time":"2026-08-14T10:00:00Z","Action":"fail","Package":"fixture","Elapsed":0.22}
`

// TestSeccompArchKilled_DirectSignature pins the exit-159 shape a real
// bwrap kill produces for the sandbox's DIRECT command (empty output — the
// process never got to write anything before dying).
func TestSeccompArchKilled_DirectSignature(t *testing.T) {
	res := sandbox.Result{ExitCode: seccompArchKillExitCode}
	if !seccompArchKilled(res, combinedOutput(res)) {
		t.Fatal("exit 159 (128+SIGSYS) must be detected as a seccomp arch-kill")
	}
}

// TestSeccompArchKilled_NestedTextSignature pins the "bad system call" text
// shape a real bwrap kill leaves behind when a NESTED subprocess (not the
// sandboxed COMMAND itself) is the one killed.
func TestSeccompArchKilled_NestedTextSignature(t *testing.T) {
	res := sandbox.Result{ExitCode: 1, Stdout: "--- FAIL: TestHelperKilled (0.22s)\nhelper: signal: bad system call (core dumped)\n"}
	if !seccompArchKilled(res, combinedOutput(res)) {
		t.Fatal("'bad system call' output text must be detected as a seccomp arch-kill")
	}
}

// TestSeccompArchKilled_OrdinaryFailureNotFlagged is the negative control:
// an ordinary test failure (exit 1, no seccomp signature) must NOT trip
// the veto — this fix must not become a new false-negative source.
func TestSeccompArchKilled_OrdinaryFailureNotFlagged(t *testing.T) {
	res := sandbox.Result{ExitCode: 1, Stdout: "--- FAIL: TestAddsWrong (0.00s)\n    fixture_test.go:9: got 2, want 3\nFAIL\n"}
	if seccompArchKilled(res, combinedOutput(res)) {
		t.Fatal("an ordinary test failure must not be flagged as a seccomp arch-kill")
	}
}

// TestInterpret_SeccompArchKilled_DirectCommand_NeverDemonstrated is oracle
// finding B2's core regression for interpret(): a direct-command seccomp
// kill (measured shape: ExitCode=159, no output) must classify as
// environment_error, never demonstrated — regardless of the detected
// ecosystem/cmd.
func TestInterpret_SeccompArchKilled_DirectCommand_NeverDemonstrated(t *testing.T) {
	res := sandbox.Result{ExitCode: seccompArchKillExitCode}
	for _, cmd := range [][]string{
		{"./repro"},
		{"go", "test", "./..."},
		{"pytest", "tests/"},
		{"bash", "-c", "./repro"},
	} {
		v := interpret(res, cmd)
		if v.demonstrated {
			t.Errorf("cmd=%v: seccomp arch-kill (exit 159) must never demonstrate; got demonstrated=true", cmd)
		}
		if v.reason != VerdictReasonEnvironmentError {
			t.Errorf("cmd=%v: reason = %q, want %q", cmd, v.reason, VerdictReasonEnvironmentError)
		}
	}
}

// TestInterpret_SeccompArchKilled_NestedSubprocess_NeverDemonstrated is
// oracle finding B2's SEVERE shape, independently re-derived by both
// oracles: a `go test` whose helper subprocess is seccomp-killed reports
// the death as ordinary "signal: bad system call" TEXT inside a
// STRUCTURED go test -json event stream and exits with the TEST RUNNER's
// own non-159 code — exactly the shape that, before this fix, promoted a
// PASSING repo test to a manufactured "bug reproduced" verdict via
// interpret()'s structured-output path (checked BEFORE the old 0a/0b
// marker tier). Placing seccompArchKilled ahead of that structured path
// (not merely at the 0a/0b tier) is what this test actually exercises:
// removing that ordering (moving the veto below the structured-output
// switch) would make this test fail again.
func TestInterpret_SeccompArchKilled_NestedSubprocess_NeverDemonstrated(t *testing.T) {
	res := sandbox.Result{ExitCode: 1, Stdout: realGoTestJSONSeccompKilled}
	v := interpret(res, []string{"go", "test", "-json", "./..."})
	if v.demonstrated {
		t.Fatalf("a seccomp-killed helper reported via go test -json must never demonstrate; got demonstrated=true, ecosystem=%q", v.ecosystem)
	}
	if v.reason != VerdictReasonEnvironmentError {
		t.Errorf("reason = %q, want %q", v.reason, VerdictReasonEnvironmentError)
	}
	if !strings.Contains(v.summary, "seccomp") {
		t.Errorf("summary should name the seccomp mechanism so the operator sees WHY; got %q", v.summary)
	}
}

// TestPatchVerdict_SeccompArchKilled_NeverFixRejected mirrors the interpret
// regressions above for patchVerdict: a seccomp arch-kill must classify as
// patchVerdictEnvFailure, never patchVerdictFixRejected (which would
// wrongly tell the prover the FIX itself failed) or patchVerdictPassed.
func TestPatchVerdict_SeccompArchKilled_NeverFixRejected(t *testing.T) {
	cases := []struct {
		name string
		res  sandbox.Result
	}{
		{"direct exit 159", sandbox.Result{ExitCode: seccompArchKillExitCode}},
		{"nested text marker", sandbox.Result{ExitCode: 1, Stdout: "signal: bad system call (core dumped)\n"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := patchVerdict(c.res, []string{"go", "test", "./..."})
			if v.kind == patchVerdictFixRejected {
				t.Errorf("seccomp arch-kill must NOT be FixRejected; got kind=%v", v.kind)
			}
			if v.kind == patchVerdictPassed {
				t.Errorf("seccomp arch-kill must not pass; got kind=%v", v.kind)
			}
			if v.kind != patchVerdictEnvFailure {
				t.Errorf("kind = %v, want patchVerdictEnvFailure", v.kind)
			}
		})
	}
}

// TestClassifySmoke_SeccompArchKilled_NeverOK mirrors the same regression
// for classifySmoke: a seccomp arch-kill must classify as
// SmokeCategoryEnvError (which gates BlocksRepro()=true), never
// SmokeCategoryOK.
func TestClassifySmoke_SeccompArchKilled_NeverOK(t *testing.T) {
	cases := []struct {
		name string
		res  sandbox.Result
	}{
		{"direct exit 159", sandbox.Result{ExitCode: seccompArchKillExitCode}},
		{"nested text marker", sandbox.Result{ExitCode: 1, Stdout: "signal: bad system call (core dumped)\n"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := classifySmoke(c.res, []string{"go", "vet", "./..."})
			if v.OK {
				t.Errorf("seccomp arch-kill must never classify OK=true; got OK=true category=%q", v.Category)
			}
			if v.Category != SmokeCategoryEnvError {
				t.Errorf("category = %q, want %q", v.Category, SmokeCategoryEnvError)
			}
			if !v.BlocksRepro() {
				t.Error("a seccomp arch-kill must gate the repro stage (BlocksRepro() = false, want true)")
			}
		})
	}
}

// TestClassifyPlaybookProbe_SeccompArchKilled_Inconclusive mirrors the same
// regression for classifyPlaybookProbe: a seccomp arch-kill must classify
// as Inconclusive, never a confirmed non-Verified failure reason (which
// gate/prompt feedback would read as "the launcher genuinely failed").
func TestClassifyPlaybookProbe_SeccompArchKilled_Inconclusive(t *testing.T) {
	probe := playbookProbe{Ecosystem: sandbox.EcosystemGo, Launcher: "go"}
	cases := []struct {
		name string
		res  sandbox.Result
	}{
		{"direct exit 159", sandbox.Result{ExitCode: seccompArchKillExitCode}},
		{"nested text marker", sandbox.Result{ExitCode: 1, Stdout: "signal: bad system call (core dumped)\n"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := classifyPlaybookProbe(probe, c.res, nil, 30*time.Second)
			if v.Verified {
				t.Error("seccomp arch-kill must never classify as Verified")
			}
			if !v.Inconclusive {
				t.Errorf("Inconclusive = false, want true; Reason=%q", v.Reason)
			}
		})
	}
}
