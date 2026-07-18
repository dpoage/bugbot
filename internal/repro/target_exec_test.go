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
	reason, detail := ClassifyTargetExecution(files, nil, "src/store/SelectedContractStore.ts", eco.EcosystemPython)
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
	reason, _ := ClassifyTargetExecution(files, nil, "agent/main.py", eco.EcosystemPython)
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
	reason, _ := ClassifyTargetExecution(files, nil, "src/scheduler/timeInTask.ts", eco.EcosystemPython)
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
	reason, detail := ClassifyTargetExecution(files, nil, "agent/main.py", eco.EcosystemPython)
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
	reason, _ := ClassifyTargetExecution(files, nil, "pkg/main.py", eco.EcosystemPython)
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
	if reason, _ := ClassifyTargetExecution(good, nil, "src/store/SelectedContractStore.ts", eco.EcosystemJS); reason != "" {
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
	if reason, _ := ClassifyTargetExecution(bad, nil, "src/store/SelectedContractStore.ts", eco.EcosystemJS); reason != VerdictReasonTargetNotExecuted {
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
	if reason, _ := ClassifyTargetExecution(files, nil, "internal/widget/widget.go", eco.EcosystemGo); reason != "" {
		t.Errorf("colocated Go test flagged: reason = %q", reason)
	}
}

// TestClassifyTargetExecution_GoShellColocation pins the 61dde0 evidence: a
// shell script colocated with a .go target satisfies same-directory
// colocation textually but never compiles into the Go package — only .go
// files gain the implicit colocation edge.
func TestClassifyTargetExecution_GoShellColocation(t *testing.T) {
	files := map[string]string{
		"internal/widget/repro_check.sh": `
python3 <<'EOF'
print(open("widget.go").read())
EOF
`,
	}
	reason, detail := ClassifyTargetExecution(files, "internal/widget/widget.go", eco.EcosystemGo)
	if reason != VerdictReasonTargetNotExecuted {
		t.Errorf("shell script colocated with Go target should be flagged, got reason = %q, detail = %q", reason, detail)
	}
}

// TestClassifyTargetExecution_RustPySourceGrep pins the Rust analog: a
// Python file colocated with a .rs target gains no edge from colocation
// alone (Python files never compile into a Rust crate).
func TestClassifyTargetExecution_RustPySourceGrep(t *testing.T) {
	files := map[string]string{
		"src/widget_check.py": `
with open("src/widget.rs") as f:
    assert "fn new" in f.read()
`,
	}
	reason, detail := ClassifyTargetExecution(files, "src/widget.rs", eco.EcosystemRust)
	if reason != VerdictReasonTargetNotExecuted {
		t.Errorf(".py colocated with Rust target should be flagged, got reason = %q, detail = %q", reason, detail)
	}
}

// TestClassifyTargetExecution_CppShellColocation pins the C++ analog: a
// shell script colocated with a .cc target gains no edge from colocation
// alone (only C++ translation units/headers compile into the target).
func TestClassifyTargetExecution_CppShellColocation(t *testing.T) {
	files := map[string]string{
		"src/repro_check.sh": `
grep "Widget::New" src/widget.cc
`,
	}
	reason, detail := ClassifyTargetExecution(files, "src/widget.cc", eco.EcosystemCpp)
	if reason != VerdictReasonTargetNotExecuted {
		t.Errorf(".sh colocated with C++ target should be flagged, got reason = %q, detail = %q", reason, detail)
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
	if reason, _ := ClassifyTargetExecution(files, nil, "internal/widget/widget.go", eco.EcosystemGo); reason != "" {
		t.Errorf("external test package importing target dir flagged: reason = %q", reason)
	}
}

// TestClassifyTargetExecution_UnknownEcosystemPermissive documents that an
// ecosystem with no edge-detection rule (bazel, unknown, ...) is never
// blocked by this static gate — it is a precision-first filter for the
// observed failure mode, not a universal lint.
func TestClassifyTargetExecution_UnknownEcosystemPermissive(t *testing.T) {
	files := map[string]string{"repro.sh": "echo hello"}
	if reason, _ := ClassifyTargetExecution(files, nil, "src/main.go", eco.EcosystemUnknown); reason != "" {
		t.Errorf("unknown ecosystem should be permissive, got reason = %q", reason)
	}
}

// TestClassifyTargetExecution_NoTargetOrFiles covers the defensive empty
// inputs: no target path (target-file provenance not carried on the Plan)
// or no submitted files must never block a plan on their own.
func TestClassifyTargetExecution_NoTargetOrFiles(t *testing.T) {
	if reason, _ := ClassifyTargetExecution(map[string]string{"a": "import foo"}, nil, "", eco.EcosystemPython); reason != "" {
		t.Errorf("empty targetPath should be permissive, got %q", reason)
	}
	if reason, _ := ClassifyTargetExecution(nil, nil, "agent/main.py", eco.EcosystemPython); reason != "" {
		t.Errorf("no test files should be permissive, got %q", reason)
	}
}

// TestTargetGateEcosystem pins the launcher-fallback rule (bugbot-9fac):
// bazel and unknown cmd ecosystems borrow the TARGET FILE's language edge
// rule so a bazel/launcher-less plan is held to the same executable-edge
// standard as a direct `go test`/`pytest` plan; recognized cmd ecosystems
// and unmapped target extensions pass through unchanged.
func TestTargetGateEcosystem(t *testing.T) {
	cases := []struct {
		cmdEco eco.Ecosystem
		target string
		want   eco.Ecosystem
	}{
		{eco.EcosystemBazel, "molecules/x/cache.go", eco.EcosystemGo},
		{eco.EcosystemBazel, "molecules/x/vehicle.py", eco.EcosystemPython},
		{eco.EcosystemUnknown, "src/scheduler/timeInTask.ts", eco.EcosystemJS},
		{eco.EcosystemGo, "whatever.py", eco.EcosystemGo},       // recognized cmd eco wins
		{eco.EcosystemBazel, "BUILD.bazel", eco.EcosystemBazel}, // unmapped ext: permissive fallback
		{eco.EcosystemUnknown, "run.sh", eco.EcosystemUnknown},
	}
	for _, tc := range cases {
		if got := targetGateEcosystem(tc.cmdEco, tc.target); got != tc.want {
			t.Errorf("targetGateEcosystem(%q, %q) = %q, want %q", tc.cmdEco, tc.target, got, tc.want)
		}
	}
}

// TestClassifyTargetExecution_GoDetailNamesColocation asserts the Go
// rejection detail teaches the ONE edge that always works — same-directory
// colocation — since a `package main` target cannot be imported at all
// (the live pub.Close/os.Exit finding burned a full 36-minute revision
// round on the generic "import the target" instruction, bugbot-9fac).
func TestClassifyTargetExecution_GoDetailNamesColocation(t *testing.T) {
	files := map[string]string{
		"repro/main_shutdown_test.go": "package repro\n\nimport \"testing\"\n\nfunc TestShutdown(t *testing.T) { t.Fatal(\"transliteration\") }\n",
	}
	reason, detail := ClassifyTargetExecution(files, nil, "molecules/robot-control/atoms/bag-scans/main.go", eco.EcosystemGo)
	if reason != VerdictReasonTargetNotExecuted {
		t.Fatalf("reason = %q, want %q", reason, VerdictReasonTargetNotExecuted)
	}
	if !strings.Contains(detail, "SAME DIRECTORY") || !strings.Contains(detail, "package main") {
		t.Errorf("Go detail must teach same-directory colocation and the package-main constraint; got %q", detail)
	}
}

// TestClassifyTargetExecution_CmdReachability_Smuggling pins the
// production evidence in bundle 0000019f62f6295434a646d1444353de: plan.cmd
// runs only a grep script, but plan.files ALSO carries a genuine test
// importing the target that the cmd never executes. Cmd-reachability must
// narrow the checked set to the file the cmd actually runs (the grep
// script), which has no executable edge, so the plan is flagged despite the
// untouched genuine test sitting right next to it.
func TestClassifyTargetExecution_CmdReachability_Smuggling(t *testing.T) {
	files := map[string]string{
		"repro_publish_timer_bug.py": `
import os
with open("molecules/robot-control/atoms/sim-location/src/sim_locations.py") as f:
    source = f.read()
assert "_publish_timer.cancel()" in source, "BUGBOT_REPRO_DEMONSTRATED"
`,
		"molecules/robot-control/atoms/sim-location/tests/test_publish_timer_stop_repro.py": `
from src.sim_locations import SimLocations

def test_stop_cancels_publish_timer():
    sim = SimLocations()
    sim.stop()
    assert sim._publish_timer is None
`,
	}
	cmd := []string{"python3", "repro_publish_timer_bug.py"}
	reason, detail := ClassifyTargetExecution(files, cmd,
		"molecules/robot-control/atoms/sim-location/src/sim_locations.py", eco.EcosystemPython)
	if reason != VerdictReasonTargetNotExecuted {
		t.Fatalf("reason = %q (detail %q), want %q — the genuine companion test was never executed by cmd",
			reason, detail, VerdictReasonTargetNotExecuted)
	}
}

// TestClassifyTargetExecution_CmdReachability_GenuineCmd is the
// counterpart to the smuggling case: same fileset, but cmd actually runs
// the genuine test (not the grep script). Cmd-reachability must narrow to
// the executed file, which DOES reach the target, so no objection.
func TestClassifyTargetExecution_CmdReachability_GenuineCmd(t *testing.T) {
	files := map[string]string{
		"repro_publish_timer_bug.py": `
import os
with open("molecules/robot-control/atoms/sim-location/src/sim_locations.py") as f:
    source = f.read()
assert "_publish_timer.cancel()" in source, "BUGBOT_REPRO_DEMONSTRATED"
`,
		"molecules/robot-control/atoms/sim-location/tests/test_publish_timer_stop_repro.py": `
from src.sim_locations import SimLocations

def test_stop_cancels_publish_timer():
    sim = SimLocations()
    sim.stop()
    assert sim._publish_timer is None
`,
	}
	cmd := []string{"pytest", "molecules/robot-control/atoms/sim-location/tests/test_publish_timer_stop_repro.py"}
	reason, detail := ClassifyTargetExecution(files, cmd,
		"molecules/robot-control/atoms/sim-location/src/sim_locations.py", eco.EcosystemPython)
	if reason != "" {
		t.Fatalf("reason = %q (detail %q), want no objection — cmd genuinely runs the importing test", reason, detail)
	}
}

// TestClassifyTargetExecution_CmdReachability_FallbackUnrecognizedLauncher
// covers acceptance (c): when cmd names no plan file at all (e.g. an
// untranslated bazel label), NO file is cmd-reachable, so the gate falls
// back to checking every submitted file — preserving today's permissive
// behavior for launcher shapes the heuristic cannot parse.
func TestClassifyTargetExecution_CmdReachability_FallbackUnrecognizedLauncher(t *testing.T) {
	files := map[string]string{
		"internal/widget/widget_test.go": `
package widget

func TestWidget(t *testing.T) {}
`,
	}
	cmd := []string{"bazelisk", "test", "//internal/widget:widget_test"}
	reason, detail := ClassifyTargetExecution(files, cmd, "internal/widget/widget.go", eco.EcosystemGo)
	if reason != "" {
		t.Fatalf("reason = %q (detail %q), want no objection — unrecognized launcher must fall back to all files", reason, detail)
	}
}

// TestClassifyTargetExecution_CmdReachability_BashCdDirectory covers
// acceptance (d): a `bash -c "cd <dir> && go test ./"` cmd makes every file
// under <dir> cmd-reachable via the directory-token rule, even though the
// directory never appears as a discrete argv element of its own.
func TestClassifyTargetExecution_CmdReachability_BashCdDirectory(t *testing.T) {
	files := map[string]string{
		"internal/widget/widget_test.go": `
package widget

func TestWidget(t *testing.T) {}
`,
	}
	cmd := []string{"bash", "-c", "cd internal/widget && go test ./"}
	reason, detail := ClassifyTargetExecution(files, cmd, "internal/widget/widget.go", eco.EcosystemGo)
	if reason != "" {
		t.Fatalf("reason = %q (detail %q), want no objection — colocated test under the cd'd directory is cmd-reachable", reason, detail)
	}
}
