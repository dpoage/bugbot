package repro

import (
	"strings"
	"testing"

	eco "github.com/dpoage/bugbot/internal/ecosystem"
)

// TestClassifyTargetExecution_PythonSourceGrep pins the "the_cloud"
// SelectedContractStore evidence: a Python test that asserts on the TARGET
// FILE'S SOURCE TEXT (assertIn on a read/read_text() string) never imports
// the target, so it must be rejected.
func TestClassifyTargetExecution_PythonSourceGrep(t *testing.T) {
	files := map[string]string{
		"test_grep.py": `
import unittest

class TestSourceContainsFix(unittest.TestCase):
    def test_uses_get_value(self):
        with open("src/store/SelectedContractStore.ts") as f:
            src = f.read()
        self.assertIn("SelectedContractStore.getValue()", src)
`,
	}
	reason, detail := ClassifyTargetExecution(files, "src/store/SelectedContractStore.ts", eco.EcosystemPython)
	if reason != VerdictReasonTargetNotExecuted {
		t.Fatalf("reason = %q, want %q", reason, VerdictReasonTargetNotExecuted)
	}
	if !strings.Contains(detail, "SelectedContractStore.ts") {
		t.Errorf("detail %q does not name the target file", detail)
	}
}

// TestClassifyTargetExecution_ImportAbsenceLint pins the "_sleep_time" data
// race evidence: a Python test asserting main.py's SOURCE TEXT does not
// contain a threading primitive — a lint check, never executes main.py.
func TestClassifyTargetExecution_ImportAbsenceLint(t *testing.T) {
	files := map[string]string{
		"test_no_threading.py": `
def test_main_does_not_use_threading():
    with open("agent/main.py") as f:
        src = f.read()
    assert "threading.Lock" not in src
`,
	}
	reason, _ := ClassifyTargetExecution(files, "agent/main.py", eco.EcosystemPython)
	if reason != VerdictReasonTargetNotExecuted {
		t.Fatalf("reason = %q, want %q", reason, VerdictReasonTargetNotExecuted)
	}
}

// TestClassifyTargetExecution_Transliteration pins the "timeInTask
// -Infinity" evidence: a Python script that MIRRORS the buggy TS IIFE
// instead of calling the repo's own code. No import of the target at all.
func TestClassifyTargetExecution_Transliteration(t *testing.T) {
	files := map[string]string{
		"repro.py": `
# Reimplementation of the buggy timeInTask() logic for demonstration.
def time_in_task(start, now):
    return now - start  # mirrors src/scheduler/timeInTask.ts

def test_negative_infinity_bug():
    result = time_in_task(float("inf"), 0)
    assert result != float("-inf"), "BUGBOT_REPRO_DEMONSTRATED"
`,
	}
	reason, _ := ClassifyTargetExecution(files, "src/scheduler/timeInTask.ts", eco.EcosystemPython)
	if reason != VerdictReasonTargetNotExecuted {
		t.Fatalf("reason = %q, want %q", reason, VerdictReasonTargetNotExecuted)
	}
}

// TestClassifyTargetExecution_GenuineBehavioralPython is the acceptance-(d)
// counterpart: a test that actually imports the target module and calls its
// function must NOT be flagged.
func TestClassifyTargetExecution_GenuineBehavioralPython(t *testing.T) {
	files := map[string]string{
		"test_behavior.py": `
import unittest
from agent.main import compute_sleep_time

class TestSleepTime(unittest.TestCase):
    def test_no_race(self):
        self.assertGreaterEqual(compute_sleep_time(), 0)
`,
	}
	reason, detail := ClassifyTargetExecution(files, "agent/main.py", eco.EcosystemPython)
	if reason != "" {
		t.Fatalf("reason = %q (detail %q), want no objection", reason, detail)
	}
}

// TestClassifyTargetExecution_PythonRelativeImport covers the "from . import
// X" / "from .. import X" package-relative forms, which carry no dotted
// package prefix.
func TestClassifyTargetExecution_PythonRelativeImport(t *testing.T) {
	files := map[string]string{
		"pkg/test_relative.py": `
from . import main

def test_x():
    assert main.compute() == 1
`,
	}
	reason, _ := ClassifyTargetExecution(files, "pkg/main.py", eco.EcosystemPython)
	if reason != "" {
		t.Fatalf("reason = %q, want no objection for relative import", reason)
	}
}

// TestClassifyTargetExecution_JSRequire covers the JS/TS require()/import
// executable-edge detection (basename match on the specifier).
func TestClassifyTargetExecution_JSRequire(t *testing.T) {
	good := map[string]string{
		"repro.test.js": `
const { getValue } = require("../src/store/SelectedContractStore");
test("stale value", () => {
  expect(getValue()).not.toBe(undefined);
});
`,
	}
	if reason, _ := ClassifyTargetExecution(good, "src/store/SelectedContractStore.ts", eco.EcosystemJS); reason != "" {
		t.Errorf("genuine require() flagged: reason = %q", reason)
	}

	bad := map[string]string{
		"repro.test.js": `
test("stale value", () => {
  const fs = require("fs");
  const src = fs.readFileSync("src/store/SelectedContractStore.ts", "utf8");
  expect(src).toContain("getValue()");
});
`,
	}
	if reason, _ := ClassifyTargetExecution(bad, "src/store/SelectedContractStore.ts", eco.EcosystemJS); reason != VerdictReasonTargetNotExecuted {
		t.Errorf("readFileSync-only test not flagged: reason = %q, want %q", reason, VerdictReasonTargetNotExecuted)
	}
}

// TestClassifyTargetExecution_GoColocation covers Go's implicit executable
// edge: any test file in the SAME package directory as the target compiles
// (and therefore links) against it, with no import statement required.
func TestClassifyTargetExecution_GoColocation(t *testing.T) {
	files := map[string]string{
		"internal/widget/widget_test.go": `
package widget

func TestWidget(t *testing.T) {}
`,
	}
	if reason, _ := ClassifyTargetExecution(files, "internal/widget/widget.go", eco.EcosystemGo); reason != "" {
		t.Errorf("colocated Go test flagged: reason = %q", reason)
	}
}

// TestClassifyTargetExecution_GoExternalTestPackage covers the external
// test-package form (package foo_test importing ".../foo") when the test
// file lives elsewhere.
func TestClassifyTargetExecution_GoExternalTestPackage(t *testing.T) {
	files := map[string]string{
		"cmd/repro/main_test.go": `
package repro_test

import (
	"testing"

	"github.com/dpoage/bugbot/internal/widget"
)

func TestWidget(t *testing.T) {
	_ = widget.New()
}
`,
	}
	if reason, _ := ClassifyTargetExecution(files, "internal/widget/widget.go", eco.EcosystemGo); reason != "" {
		t.Errorf("external test package importing target dir flagged: reason = %q", reason)
	}
}

// TestClassifyTargetExecution_UnknownEcosystemPermissive documents that an
// ecosystem with no edge-detection rule (bazel, unknown, ...) is never
// blocked by this static gate — it is a precision-first filter for the
// observed failure mode, not a universal lint.
func TestClassifyTargetExecution_UnknownEcosystemPermissive(t *testing.T) {
	files := map[string]string{"repro.sh": "echo hello"}
	if reason, _ := ClassifyTargetExecution(files, "src/main.go", eco.EcosystemUnknown); reason != "" {
		t.Errorf("unknown ecosystem should be permissive, got reason = %q", reason)
	}
}

// TestClassifyTargetExecution_NoTargetOrFiles covers the defensive empty
// inputs: no target path (target-file provenance not carried on the Plan)
// or no submitted files must never block a plan on their own.
func TestClassifyTargetExecution_NoTargetOrFiles(t *testing.T) {
	if reason, _ := ClassifyTargetExecution(map[string]string{"a": "import foo"}, "", eco.EcosystemPython); reason != "" {
		t.Errorf("empty targetPath should be permissive, got %q", reason)
	}
	if reason, _ := ClassifyTargetExecution(nil, "agent/main.py", eco.EcosystemPython); reason != "" {
		t.Errorf("no test files should be permissive, got %q", reason)
	}
}
