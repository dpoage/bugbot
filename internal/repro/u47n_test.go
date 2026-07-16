package repro

import (
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/sandbox"
)

// u47n_test.go covers bugbot-u47n: structured ran-evidence must bind to a
// test the plan itself declared (bindTestEvidence), and a go test plan must
// name its own declared test via -run (validateReproCmd), closing the two
// vacuous-promotion channels the bugbot-c49s review found (an unrelated
// pre-existing failing test in the targeted package/module satisfying the
// ran-evidence gate, and -run being suggested but never enforced).

// TestBindTestEvidence_Go_ForeignFailure_NotPromoted: go test -json reports
// a failing test the plan never declared — bindTestEvidence must downgrade
// the verdict to foreign_test_failure, not leave it demonstrated.
func TestBindTestEvidence_Go_ForeignFailure_NotPromoted(t *testing.T) {
	stdout := `{"Action":"run","Package":"p","Test":"TestPreExistingFlaky"}
{"Action":"fail","Package":"p","Test":"TestPreExistingFlaky"}
{"Action":"fail","Package":"p"}
`
	res := sandbox.Result{ExitCode: 1, Stdout: stdout}
	v := interpret(res, []string{"go", "test", "-json", "-run", "TestMyBug", "./..."})
	if !v.demonstrated {
		t.Fatalf("test setup: interpret() must demonstrate before binding; got reason=%q", v.reason)
	}

	files := map[string]string{"bug_test.go": "package p\n\nimport \"testing\"\n\nfunc TestMyBug(t *testing.T) { t.Fatal(\"boom\") }\n"}
	bound := bindTestEvidence(v, files)
	if bound.demonstrated {
		t.Fatalf("foreign failing test (TestPreExistingFlaky) must NOT promote when plan only declares TestMyBug")
	}
	if bound.reason != VerdictReasonForeignFailure {
		t.Errorf("reason = %q, want %q", bound.reason, VerdictReasonForeignFailure)
	}
	if bound.foreignTest != "TestPreExistingFlaky" {
		t.Errorf("foreignTest = %q, want %q", bound.foreignTest, "TestPreExistingFlaky")
	}
	// feedback() must name the foreign test and steer the agent at -run.
	fb := bound.feedback(&Plan{Cmd: []string{"go", "test", "-json", "-run", "TestMyBug", "./..."}})
	if !strings.Contains(fb, "TestPreExistingFlaky") {
		t.Errorf("feedback does not name the foreign test: %q", fb)
	}
	if !strings.Contains(fb, "-run") {
		t.Errorf("feedback does not steer the agent at -run: %q", fb)
	}
}

// TestBindTestEvidence_Go_DeclaredFailure_StillPromotes: the failing test IS
// the one the plan declared -> unchanged, still demonstrated. Also covers a
// subtest of the declared name.
func TestBindTestEvidence_Go_DeclaredFailure_StillPromotes(t *testing.T) {
	files := map[string]string{"bug_test.go": "package p\n\nimport \"testing\"\n\nfunc TestMyBug(t *testing.T) { t.Fatal(\"boom\") }\n"}

	t.Run("exact_match", func(t *testing.T) {
		res := sandbox.Result{ExitCode: 1, Stdout: `{"Action":"run","Package":"p","Test":"TestMyBug"}
{"Action":"fail","Package":"p","Test":"TestMyBug"}
`}
		v := interpret(res, []string{"go", "test", "-json", "-run", "TestMyBug", "./..."})
		bound := bindTestEvidence(v, files)
		if !bound.demonstrated {
			t.Fatalf("declared test's own failure must still promote; got reason=%q", bound.reason)
		}
	})

	t.Run("subtest_match", func(t *testing.T) {
		res := sandbox.Result{ExitCode: 1, Stdout: `{"Action":"run","Package":"p","Test":"TestMyBug/sub_case"}
{"Action":"fail","Package":"p","Test":"TestMyBug/sub_case"}
`}
		v := interpret(res, []string{"go", "test", "-json", "-run", "TestMyBug", "./..."})
		bound := bindTestEvidence(v, files)
		if !bound.demonstrated {
			t.Fatalf("a subtest of the declared test must still promote; got reason=%q", bound.reason)
		}
	})
}

// TestBindTestEvidence_Python_ForeignFailure_NotPromoted mirrors the Go case
// for the JUnit XML structured path: a failing testcase whose name does not
// contain any declared test name must not promote.
func TestBindTestEvidence_Python_ForeignFailure_NotPromoted(t *testing.T) {
	junit := `<testsuites><testsuite><testcase classname="tests.test_other" name="test_unrelated_pre_existing_bug"><failure message="AssertionError">boom</failure></testcase></testsuite></testsuites>`
	res := sandbox.Result{ExitCode: 1, Captured: map[string][]byte{structuredJUnitXMLPath: []byte(junit)}}
	v := interpret(res, []string{"pytest", "--junitxml=" + structuredJUnitXMLPath, "test_mybug.py"})
	if !v.demonstrated {
		t.Fatalf("test setup: interpret() must demonstrate before binding; got reason=%q", v.reason)
	}

	files := map[string]string{"test_mybug.py": "def test_my_bug():\n    assert False\n"}
	bound := bindTestEvidence(v, files)
	if bound.demonstrated {
		t.Fatal("foreign failing testcase must NOT promote when plan only declares test_my_bug")
	}
	if bound.reason != VerdictReasonForeignFailure {
		t.Errorf("reason = %q, want %q", bound.reason, VerdictReasonForeignFailure)
	}
}

// TestBindTestEvidence_Python_DeclaredFailure_StillPromotes: pytest's own
// module/class-qualified testcase name (e.g. "tests.test_mybug::test_my_bug")
// must still match via substring against the bare declared name.
func TestBindTestEvidence_Python_DeclaredFailure_StillPromotes(t *testing.T) {
	junit := `<testsuites><testsuite><testcase classname="tests.test_mybug" name="tests.test_mybug::test_my_bug"><failure message="AssertionError">boom</failure></testcase></testsuite></testsuites>`
	res := sandbox.Result{ExitCode: 1, Captured: map[string][]byte{structuredJUnitXMLPath: []byte(junit)}}
	v := interpret(res, []string{"pytest", "--junitxml=" + structuredJUnitXMLPath, "test_mybug.py"})

	files := map[string]string{"test_mybug.py": "def test_my_bug():\n    assert False\n"}
	bound := bindTestEvidence(v, files)
	if !bound.demonstrated {
		t.Fatalf("declared test's own failure must still promote; got reason=%q", bound.reason)
	}
}

// TestBindTestEvidence_NoExtractableNames_Unchanged: a bare script / test
// file this scan's dumb regex cannot see any declared name in must leave
// the verdict completely unchanged — binding never turns "couldn't extract
// a name" into a rejection.
func TestBindTestEvidence_NoExtractableNames_Unchanged(t *testing.T) {
	res := sandbox.Result{ExitCode: 1, Stdout: `{"Action":"run","Package":"p","Test":"TestSomething"}
{"Action":"fail","Package":"p","Test":"TestSomething"}
`}
	v := interpret(res, []string{"go", "test", "-json", "./..."})
	if !v.demonstrated {
		t.Fatalf("test setup: interpret() must demonstrate; got reason=%q", v.reason)
	}
	// files declares NO func Test... at all (e.g. a package stub / helper file).
	files := map[string]string{"helper.go": "package p\n\nfunc helper() {}\n"}
	bound := bindTestEvidence(v, files)
	if !bound.demonstrated {
		t.Fatalf("no extractable declared names must leave the verdict unchanged; got reason=%q", bound.reason)
	}
}

// TestBindTestEvidence_MarkerPath_Unchanged: a marker-path demonstration
// (structuredFailingTests never populated) must pass through bindTestEvidence
// untouched regardless of declared names — this is the "bare script/unknown
// ecosystem plans unaffected" acceptance criterion.
func TestBindTestEvidence_MarkerPath_Unchanged(t *testing.T) {
	res := sandbox.Result{ExitCode: 1, Stdout: "--- FAIL: TestUnrelated (0.00s)\nFAIL\n"}
	// Plain-text stdout: parseGoTestEvents yields ok=false, so interpret()
	// demonstrates via the marker cascade, not the structured path.
	v := interpret(res, []string{"bash", "-c", "go test ./..."})
	if !v.demonstrated {
		t.Fatalf("test setup: marker path must demonstrate; got reason=%q", v.reason)
	}
	files := map[string]string{"bug_test.go": "package p\n\nimport \"testing\"\n\nfunc TestMyBug(t *testing.T) {}\n"}
	bound := bindTestEvidence(v, files)
	if !bound.demonstrated || bound.reason != "" {
		t.Errorf("marker-path demonstration must be unaffected by binding; got demonstrated=%v reason=%q", bound.demonstrated, bound.reason)
	}
}

// TestValidateReproCmd_RunEnforcement_RejectsMissingRun: a go test plan that
// declares a Go test but omits -run must be rejected with corrective
// feedback naming the declared test.
func TestValidateReproCmd_RunEnforcement_RejectsMissingRun(t *testing.T) {
	files := map[string]string{"bug_test.go": "package p\n\nimport \"testing\"\n\nfunc TestMyBug(t *testing.T) { t.Fatal(\"x\") }\n"}
	err := validateReproCmd([]string{"go", "test", "-timeout", "60s", "./..."}, files)
	if err == nil {
		t.Fatal("want an error for a go test plan missing -run when a declared test name exists")
	}
	if !strings.Contains(err.Error(), "-run") || !strings.Contains(err.Error(), "TestMyBug") {
		t.Errorf("error %q must mention -run and the declared test name", err.Error())
	}
}

// TestValidateReproCmd_RunEnforcement_AcceptsRunNamingDeclaredTest verifies
// the happy path and the no-extractable-names no-op.
func TestValidateReproCmd_RunEnforcement_AcceptsRunNamingDeclaredTest(t *testing.T) {
	files := map[string]string{"bug_test.go": "package p\n\nimport \"testing\"\n\nfunc TestMyBug(t *testing.T) { t.Fatal(\"x\") }\n"}
	if err := validateReproCmd([]string{"go", "test", "-timeout", "60s", "-run", "TestMyBug", "./..."}, files); err != nil {
		t.Errorf("valid -run naming the declared test must not be rejected: %v", err)
	}

	// No extractable Go test names (bare script / non-_test.go file): the
	// -run rule must be a no-op, exactly like before this feature.
	bareFiles := map[string]string{"repro.sh": "#!/bin/bash\necho hi\n"}
	if err := validateReproCmd([]string{"go", "test", "-timeout", "60s", "./..."}, bareFiles); err != nil {
		t.Errorf("no extractable declared names must leave -run unenforced: %v", err)
	}
}

// TestValidatePlan_RunEnforcement_Integration exercises the rule through
// validatePlan (the real entry point Attempt uses).
func TestValidatePlan_RunEnforcement_Integration(t *testing.T) {
	p := &Plan{
		Files:  map[string]string{"bug_test.go": "package p\n\nimport \"testing\"\n\nfunc TestMyBug(t *testing.T) { t.Fatal(\"x\") }\n"},
		Cmd:    []string{"go", "test", "-timeout", "60s", "./..."},
		Expect: "x",
	}
	if err := validatePlan(p, ""); err == nil {
		t.Fatal("validatePlan must reject a go test plan missing -run when a declared test exists")
	}
}
