package ecosystem_test

import (
	"testing"

	"github.com/dpoage/bugbot/internal/ecosystem"
)

// TestWitnessRulesFor_KnownAndUnknown pins which ecosystems can provide an
// execution witness (Go, Python, Rust, JS, C++) and which cannot (Bazel,
// unknown) — the split bugbot-qb4r's downgrade-to-witness-only path relies
// on.
func TestWitnessRulesFor_KnownAndUnknown(t *testing.T) {
	witnessCapable := []string{
		ecosystem.EcosystemGo,
		ecosystem.EcosystemPython,
		ecosystem.EcosystemRust,
		ecosystem.EcosystemJS,
		ecosystem.EcosystemCpp,
	}
	for _, name := range witnessCapable {
		if _, ok := ecosystem.WitnessRulesFor(name); !ok {
			t.Errorf("WitnessRulesFor(%q) = not found, want an entry", name)
		}
	}
	notCapable := []string{ecosystem.EcosystemBazel, ecosystem.EcosystemUnknown}
	for _, name := range notCapable {
		if _, ok := ecosystem.WitnessRulesFor(name); ok {
			t.Errorf("WitnessRulesFor(%q) = found, want no entry (ecosystem cannot provide a witness)", name)
		}
	}
}

// TestHasTargetWitness_Python covers a genuine pytest traceback frame naming
// the target file (positive) versus a passing-looking output or one that
// merely names the test file itself (negative).
func TestHasTargetWitness_Python(t *testing.T) {
	rules, ok := ecosystem.WitnessRulesFor(ecosystem.EcosystemPython)
	if !ok {
		t.Fatal("python witness rules missing")
	}
	target := "agent/main.py"

	positive := `
FAILED tests/test_behavior.py::test_no_race - AssertionError
Traceback (most recent call last):
  File "tests/test_behavior.py", line 6, in test_no_race
    self.assertGreaterEqual(compute_sleep_time(), 0)
  File "agent/main.py", line 42, in compute_sleep_time
    return _sleep_time - drift
AssertionError
`
	if !rules.HasTargetWitness(positive, target) {
		t.Error("expected witness for traceback frame naming agent/main.py")
	}

	negativeGrepOnly := `
FAILED tests/test_grep.py::test_uses_get_value - AssertionError: assert 'SelectedContractStore.getValue()' in '...'
Traceback (most recent call last):
  File "tests/test_grep.py", line 8, in test_uses_get_value
    self.assertIn("SelectedContractStore.getValue()", src)
AssertionError
`
	if rules.HasTargetWitness(negativeGrepOnly, target) {
		t.Error("grep-test traceback (never naming agent/main.py) must not witness")
	}
}

// TestHasTargetWitness_Go covers a panic stack trace line naming the
// target's file:line, and a not-in-stack negative case (only the test file
// appears).
func TestHasTargetWitness_Go(t *testing.T) {
	rules, ok := ecosystem.WitnessRulesFor(ecosystem.EcosystemGo)
	if !ok {
		t.Fatal("go witness rules missing")
	}
	target := "internal/widget/widget.go"

	positive := "--- FAIL: TestWidget (0.00s)\npanic: nil pointer\n\ninternal/widget/widget.go:42 +0x1a\n"
	if !rules.HasTargetWitness(positive, target) {
		t.Error("expected witness for panic stack frame naming widget.go")
	}

	negative := "--- FAIL: TestWidget (0.00s)\n    widget_test.go:10: assertion failed\n"
	if rules.HasTargetWitness(negative, target) {
		t.Error("stack referencing only the test file must not witness the target")
	}
}

// TestHasTargetWitness_EmptyInputs covers the defensive zero-value cases:
// no target path, and an ecosystem with no witness rules (zero value).
func TestHasTargetWitness_EmptyInputs(t *testing.T) {
	rules, _ := ecosystem.WitnessRulesFor(ecosystem.EcosystemPython)
	if rules.HasTargetWitness("File \"agent/main.py\", line 1", "") {
		t.Error("empty targetPath must never witness")
	}
	var zero ecosystem.WitnessRules
	if zero.HasTargetWitness("File \"agent/main.py\", line 1", "agent/main.py") {
		t.Error("zero-value WitnessRules (no patterns) must never witness")
	}
}
