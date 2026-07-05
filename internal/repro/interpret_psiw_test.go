package repro

// Tests for bugbot-psiw (marker classification fixes) and bugbot-0obm
// (agent-facing excerpt raised to ~4KB tail).
//
// bugbot-psiw cases:
//   (a) Go "go: " toolchain marker anchored to line starts — a test whose
//       assertion output contains "go: " mid-line must not misclassify as
//       toolchain_error.
//   (b) Ran-evidence beats env markers — a failing Go test whose assertion
//       contains "read-only file system" or "no space left on device" text
//       must classify as demonstrated, not environment_error.
//   (c) Pytest exit 2 + collection-error banner → not_demonstrated (the test
//       never ran), even though Python ranMarkers contain "failed " and
//       "traceback (most recent call last)" which may appear in collection output.
//   (d) Exit-0 run with "sanitizer:" only in fixture/log output → exit_zero
//       (not demonstrated), because sanitizer promotion now requires non-zero exit.
//
// bugbot-0obm cases:
//   feedback() for a build_error whose actual compiler error sits 2KB into the
//   output includes that error text (tailExcerpt takes the tail).

import (
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/sandbox"
)

// --- bugbot-psiw (a): Go "go: " line-start anchoring ---

// TestInterpret_Go_ToolchainMarker_LineAnchored_MidLine pins that "go: "
// appearing mid-line (e.g. inside a test assertion output like "ergo: done")
// does NOT trigger toolchain_error. The marker must be anchored to line starts.
func TestInterpret_Go_ToolchainMarker_LineAnchored_MidLine(t *testing.T) {
	// A test that prints "...ergo: something" mid-line — contains "go: " as a
	// suffix of "ergo: " but NOT at a line start. With line-anchoring this must
	// classify as demonstrated (--- FAIL present), not toolchain_error.
	res := sandbox.Result{
		ExitCode: 1,
		Stdout: "=== RUN   TestErgoPrefix\n" +
			"    assertion.go:42: expected ergo: something, got ergo: nothing\n" +
			"--- FAIL: TestErgoPrefix (0.001s)\n" +
			"FAIL\tgithub.com/example/pkg\n",
	}
	v := interpret(res, []string{"go", "test", "./..."})
	if !v.demonstrated {
		t.Errorf("mid-line 'go: ' substring must not trigger toolchain_error; got reason=%q", v.reason)
	}
}

// TestInterpret_Go_ToolchainMarker_LineAnchored_AtLineStart pins that "go: "
// at the START of a line still correctly classifies as toolchain_error.
func TestInterpret_Go_ToolchainMarker_LineAnchored_AtLineStart(t *testing.T) {
	res := sandbox.Result{
		ExitCode: 1,
		Stderr:   "go: -race requires cgo; enable cgo by setting CGO_ENABLED=1\n",
	}
	v := interpret(res, []string{"go", "test", "-race", "./..."})
	if v.demonstrated {
		t.Errorf("want not demonstrated for toolchain refusal, got demonstrated=true")
	}
	if v.reason != VerdictReasonToolchainError {
		t.Errorf("want reason=toolchain_error for 'go: ' at line start, got %q", v.reason)
	}
}

// --- bugbot-psiw (b): ran-evidence beats env markers ---

// TestInterpret_Go_RanEvidenceBeatsEnvMarker pins that a failing Go test
// whose assertion output contains "read-only file system" or "no space left
// on device" text classifies as demonstrated, not environment_error.
// This is the canonical false-negative: a test asserting on EROFS/ENOSPC
// error handling was permanently misclassified before this fix.
func TestInterpret_Go_RanEvidenceBeatsEnvMarker(t *testing.T) {
	t.Run("read_only_file_system_in_assertion", func(t *testing.T) {
		res := sandbox.Result{
			ExitCode: 1,
			Stdout: "=== RUN   TestCreateFileReadOnly\n" +
				"    fs_test.go:55: got err=open /etc/foo: read-only file system, want nil\n" +
				"--- FAIL: TestCreateFileReadOnly (0.002s)\n" +
				"FAIL\tgithub.com/example/pkg\n",
		}
		v := interpret(res, []string{"go", "test", "./..."})
		if !v.demonstrated {
			t.Errorf("failing Go test containing 'read-only file system' in assertion must classify as demonstrated; got reason=%q", v.reason)
		}
	})

	t.Run("no_space_left_in_assertion", func(t *testing.T) {
		res := sandbox.Result{
			ExitCode: 1,
			Stdout: "=== RUN   TestDiskFull\n" +
				"    disk_test.go:88: write error: no space left on device\n" +
				"--- FAIL: TestDiskFull (0.001s)\n" +
				"FAIL\tgithub.com/example/pkg\n",
		}
		v := interpret(res, []string{"go", "test", "./..."})
		if !v.demonstrated {
			t.Errorf("failing Go test containing 'no space left on device' in assertion must classify as demonstrated; got reason=%q", v.reason)
		}
	})
}

// TestInterpret_EnvMarker_NoRanEvidence_StillEnvironmentError is the negative
// regression: a genuine environment failure (no ran-evidence, just env marker)
// must still classify as environment_error.
func TestInterpret_EnvMarker_NoRanEvidence_StillEnvironmentError(t *testing.T) {
	res := sandbox.Result{
		ExitCode: 1,
		Stderr:   "mkdir /go/pkg: read-only file system\n",
	}
	v := interpret(res, []string{"go", "test", "./..."})
	if v.demonstrated {
		t.Errorf("genuine env failure (no ran-evidence) must not demonstrate; got demonstrated=true")
	}
	if v.reason != VerdictReasonEnvironmentError {
		t.Errorf("want reason=environment_error, got %q", v.reason)
	}
}

// --- bugbot-psiw (c): pytest exit 2 + collection error → not_demonstrated ---

// TestInterpret_Pytest_CollectionError_NotDemonstrated pins that a pytest run
// that exits 2 (collection error) classifies as not_demonstrated — even though
// Python ranMarkers contain "failed ", "traceback (most recent call last)", and
// "short test summary" which may appear in collection error output.
// This test uses conftest RuntimeError / fixture-setup failures that have NO
// build marker (no importerror/syntaxerror) so the classification is driven
// purely by the exit-2 + notRanMarker gate (bugbot-psiw).
func TestInterpret_Pytest_CollectionError_NotDemonstrated(t *testing.T) {
	t.Run("conftest_runtimeerror_exit2", func(t *testing.T) {
		// Oracle regression: conftest.py raises RuntimeError during collection.
		// Output contains "traceback (most recent call last)" and "short test summary"
		// (both ranMarkers) plus "errors during collection" (notRanMarker).
		// Exit 2 + notRanMarker must win → not_demonstrated.
		// No build marker present (RuntimeError is not a SyntaxError/ImportError).
		res := sandbox.Result{
			ExitCode: 2,
			Stdout: "============================= test session starts ==============================\n" +
				"collected 0 items / 1 error\n" +
				"\n" +
				"================================== ERRORS ==================================\n" +
				"_______________________ ERROR collecting tests/ ________________________\n" +
				"conftest.py:8: in <module>\n" +
				"    setup_environment()\n" +
				"conftest.py:5: in setup_environment\n" +
				"    raise RuntimeError('database not available')\n" +
				"RuntimeError: database not available\n" +
				"=========================== 1 error in 0.09s ============================\n",
			Stderr: "ERROR: errors during collection\n" +
				"Traceback (most recent call last):\n" +
				"  File \"conftest.py\", line 5, in setup_environment\n" +
				"    raise RuntimeError('database not available')\n" +
				"RuntimeError: database not available\n" +
				"short test summary info\n" +
				"ERROR conftest.py - RuntimeError: database not available\n",
		}
		v := interpret(res, []string{"pytest", "tests/"})
		if v.demonstrated {
			t.Errorf("conftest RuntimeError collection error (exit 2) must not classify as demonstrated; got demonstrated=true")
		}
		if v.reason != VerdictReasonNotDemonstrated {
			t.Errorf("want reason=not_demonstrated, got %q", v.reason)
		}
	})

	t.Run("collection_error_banner_no_build_marker", func(t *testing.T) {
		// Pytest exit 2 with "errors during collection" banner.
		// No importerror/syntaxerror/nameerror — classification must use the
		// exit-code-aware not-ran gate, not build markers.
		res := sandbox.Result{
			ExitCode: 2,
			Stdout: "collected 0 items / 2 errors\n" +
				"errors during collection\n" +
				"Traceback (most recent call last):\n" +
				"  File \"conftest.py\", line 3\n" +
				"    raise RuntimeError('fixture failed')\n" +
				"RuntimeError: fixture failed\n" +
				"FAILED (errors during collection)\n",
		}
		v := interpret(res, []string{"python3", "-m", "pytest", "tests/"})
		if v.demonstrated {
			t.Errorf("pytest collection error (exit 2, no build marker) must not classify as demonstrated; got demonstrated=true")
		}
	})

	t.Run("no_tests_ran_exit4", func(t *testing.T) {
		// Pytest exit 4 = collected 0 items (e.g. -k filter matched nothing).
		res := sandbox.Result{
			ExitCode: 4,
			Stdout: "============================= test session starts ==============================\n" +
				"collected 0 items\n" +
				"\n" +
				"========================= no tests ran =========================\n",
		}
		v := interpret(res, []string{"pytest", "tests/"})
		if v.demonstrated {
			t.Errorf("pytest with no tests collected (exit 4) must not classify as demonstrated; got demonstrated=true")
		}
	})
}

// TestInterpret_Pytest_MixedRun_Exit1_StillDemonstrated pins the mixed-run
// contract: when pytest exits 1 (some tests ran and failed, even if other
// modules had collection errors), the run must still classify as demonstrated.
// Exit-1 is intentionally excluded from the collection-error gate.
func TestInterpret_Pytest_MixedRun_Exit1_StillDemonstrated(t *testing.T) {
	// Mixed run: test_good.py collected cleanly and failed; test_bad.py had a
	// collection error. Pytest exits 1 (not 2). "errors during collection"
	// appears but a real FAIL is also present. Must classify as demonstrated.
	res := sandbox.Result{
		ExitCode: 1,
		Stdout: "============================= test session starts ==============================\n" +
			"collected 1 item / 1 error\n" +
			"\n" +
			"================================== ERRORS ==================================\n" +
			"errors during collection\n" +
			"RuntimeError: fixture for test_bad.py failed\n" +
			"\n" +
			"================================= FAILURES =================================\n" +
			"tests/test_good.py::test_logic FAILED\n" +
			"E       AssertionError: assert 1 == 2\n" +
			"short test summary info\n" +
			"FAILED tests/test_good.py::test_logic - AssertionError: assert 1 == 2\n" +
			"= 1 failed, 1 error in 0.15s =\n",
	}
	v := interpret(res, []string{"pytest", "tests/"})
	if !v.demonstrated {
		t.Errorf("mixed pytest run (exit 1, real FAIL + collection error) must classify as demonstrated; got reason=%q", v.reason)
	}
}

// TestInterpret_Pytest_RealFailure_StillDemonstrated is the negative regression:
// a genuine pytest test failure (a test that RAN and failed) must still classify
// as demonstrated after the notRanMarkers fix.
func TestInterpret_Pytest_RealFailure_StillDemonstrated(t *testing.T) {
	res := sandbox.Result{
		ExitCode: 1,
		Stdout: "============================= test session starts ==============================\n" +
			"collected 1 item\n" +
			"\n" +
			"tests/test_foo.py::test_bar FAILED                                       [100%]\n" +
			"\n" +
			"================================= FAILURES =================================\n" +
			"_________________________________ test_bar _________________________________\n" +
			"\n" +
			"    def test_bar():\n" +
			">       assert add(1, 2) == 4\n" +
			"E       AssertionError: assert 3 == 4\n" +
			"\n" +
			"short test summary info\n" +
			"FAILED tests/test_foo.py::test_bar - AssertionError: assert 3 == 4\n" +
			"= 1 failed in 0.08s =\n",
	}
	v := interpret(res, []string{"pytest", "tests/"})
	if !v.demonstrated {
		t.Errorf("genuine pytest failure must classify as demonstrated; got reason=%q", v.reason)
	}
}

// --- bugbot-psiw (d): exit-0 sanitizer → exit_zero, not demonstrated ---

// TestInterpret_SanitizerMarker_ExitZero_NotDemonstrated pins that a run
// exiting 0 whose fixture or log output incidentally contains "sanitizer:"
// does NOT promote to demonstrated. Sanitizer promotion now requires non-zero
// exit (bugbot-psiw).
func TestInterpret_SanitizerMarker_ExitZero_NotDemonstrated(t *testing.T) {
	res := sandbox.Result{
		ExitCode: 0,
		Stdout: "running tests...\n" +
			"fixture setup: sanitizer: environment check passed\n" +
			"all tests passed\n",
	}
	v := interpret(res, []string{"go", "test", "./..."})
	if v.demonstrated {
		t.Errorf("exit-0 run with 'sanitizer:' in fixture output must not demonstrate; got demonstrated=true")
	}
	if v.reason != VerdictReasonExitZero {
		t.Errorf("want reason=exit_zero for exit-0 sanitizer log, got %q", v.reason)
	}
}

// TestInterpret_SanitizerMarker_NonZero_StillDemonstrated is the negative
// regression: a genuine sanitizer violation (non-zero exit) must still classify
// as demonstrated after the exit-0 gating fix.
func TestInterpret_SanitizerMarker_NonZero_StillDemonstrated(t *testing.T) {
	res := sandbox.Result{
		ExitCode: 1,
		Stderr: "==12==ERROR: AddressSanitizer: heap-use-after-free on address 0x...\n" +
			"READ of size 4\n",
	}
	v := interpret(res, []string{"./build/test_binary"})
	if !v.demonstrated {
		t.Errorf("non-zero exit with ASan output must classify as demonstrated; got reason=%q", v.reason)
	}
}

// --- bugbot-0obm: agent-facing excerpt includes tail (4KB) ---

// TestFeedback_BuildError_CompilerErrorInTail pins that feedback() for a
// build_error whose actual compiler error sits 2KB into the output includes
// that error text. tailExcerpt takes the TAIL so the diagnostic survives even
// when the head is download/configure noise (bugbot-0obm).
func TestFeedback_BuildError_CompilerErrorInTail(t *testing.T) {
	// Build a combined output where the first 2KB is noise (configure/module
	// download output that does NOT start lines with "go: ") and the compiler
	// error appears after that. With the old trunc(out,400) the error was lost;
	// with tailExcerpt(out,4096) the last 4096 bytes include it.
	//
	// The noise here is realistic: stderr from `go test` often starts with
	// "# github.com/.../pkg" module lines and tidy/verify output before the
	// actual compiler errors. We use build-step noise that doesn't trigger the
	// line-anchored "go: " toolchain marker.
	noise := strings.Repeat("# verifying module cache entry github.com/some/dep@v1.2.3\n", 55) // ~2.7KB
	compilerError := "pkg/foo.go:42:5: undefined: missingFunc\n" +
		"pkg/foo.go:43:10: cannot use x (type int) as type string\n" +
		"FAIL\tgithub.com/example/pkg [build failed]\n"
	out := noise + compilerError

	res := sandbox.Result{
		ExitCode: 1,
		Stderr:   out,
	}
	plan := &Plan{Cmd: []string{"go", "test", "./..."}}
	v := interpret(res, plan.Cmd)

	if v.demonstrated {
		t.Errorf("build error must not demonstrate; got demonstrated=true")
	}
	if v.reason != VerdictReasonBuildError {
		t.Errorf("want reason=build_error, got %q", v.reason)
	}

	fb := v.feedback(plan)
	if !strings.Contains(fb, "undefined: missingFunc") {
		t.Errorf("feedback must include compiler error from tail of output; got feedback:\n%s", fb)
	}
	if !strings.Contains(fb, "cannot use x") {
		t.Errorf("feedback must include second compiler error line; got feedback:\n%s", fb)
	}
}

// TestFeedback_TailExcerpt_RuneSafe pins that tailExcerpt never splits a
// multi-byte UTF-8 rune at the cut point.
func TestFeedback_TailExcerpt_RuneSafe(t *testing.T) {
	// Build a string where the cut boundary would land mid-rune without rune-safety.
	// U+4e2d (中) is 3 bytes: 0xe4 0xb8 0xad.
	// Construct a string of exactly n+1 bytes where the last 3 bytes are this rune.
	n := 16
	prefix := strings.Repeat("A", n-2) // 14 'A' bytes
	rune3 := "中"                       // 3 bytes
	s := prefix + rune3                // 17 bytes total; cut at 17-16=1 → byte 1 is mid-rune

	got := tailExcerpt(s, n)
	// Result must be valid UTF-8 — the rune must not be split.
	if strings.ContainsRune(got, '\uFFFD') {
		t.Errorf("tailExcerpt produced invalid UTF-8 (replacement rune); got %q", got)
	}
	// The full 3-byte rune must survive intact.
	if !strings.Contains(got, "中") {
		t.Errorf("tailExcerpt dropped the multi-byte rune; got %q", got)
	}
}

// TestInterpret_CappedOutput_FailAtTail_Demonstrated verifies the end-to-end
// acceptance criterion for bugbot-yjm1 + bugbot-0obm: a sandbox Result whose
// combined output was capped (head+tail) still classifies as demonstrated when
// the tail contains "--- FAIL". This exercises the interpret() path over a
// realistic truncated output produced by a cappedBuffer.
func TestInterpret_CappedOutput_FailAtTail_Demonstrated(t *testing.T) {
	// Simulate a cappedBuffer result: the head is full of build noise
	// and the tail (after the gap marker) contains the failure summary.
	// interpret() must find "--- fail" in the tail and return demonstrated.
	// NOTE: head noise must NOT start lines with "go: " (line-anchored toolchain
	// marker) — use build-step "# github.com/..." lines instead.
	head := strings.Repeat("# github.com/some/dep: verifying module integrity\n", 200) // ~6KB
	gapMarker := "\n... [12345 bytes elided by sandbox] ...\n"
	tail := "=== RUN   TestDataRace\n" +
		"--- FAIL: TestDataRace (0.543s)\n" +
		"FAIL\tgithub.com/example/pkg\n"

	combined := head + gapMarker + tail
	res := sandbox.Result{
		ExitCode: 1,
		Stdout:   combined,
	}
	v := interpret(res, []string{"go", "test", "-race", "./..."})
	if !v.demonstrated {
		t.Errorf("capped output with FAIL in tail must classify as demonstrated; got reason=%q", v.reason)
	}
}
