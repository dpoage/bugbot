package ecosystem_test

import (
	"testing"

	"github.com/dpoage/bugbot/internal/ecosystem"
)

// TestWitnessRulesFor_KnownAndUnknown pins which ecosystems have a
// standardized, parseable coverage-report format (Go, Python, Rust, JS,
// C++) and which don't (Bazel, unknown) — the split bugbot-qb4r's
// downgrade-to-witness-only path relies on.
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

// TestTargetCoverage_Python covers a coverage.py / pytest-cov terminal
// report row: a genuine nonzero-coverage row (positive), an explicit 0%
// row (trusted negative evidence), and plain test output with no coverage
// report at all (found=false — permissive, no evidence either way).
func TestTargetCoverage_Python(t *testing.T) {
	rules, ok := ecosystem.WitnessRulesFor(ecosystem.EcosystemPython)
	if !ok {
		t.Fatal("python witness rules missing")
	}
	target := "agent/main.py"

	covered := "Name              Stmts   Miss  Cover\n---------------------------------\nagent/main.py        42      5    88%\n"
	if pct, found := rules.TargetCoverage(covered, target); !found || pct <= 0 {
		t.Errorf("TargetCoverage(covered) = (%v, %v), want positive coverage", pct, found)
	}

	untouched := "Name              Stmts   Miss  Cover\n---------------------------------\nagent/main.py        42     42     0%\n"
	if pct, found := rules.TargetCoverage(untouched, target); !found || pct != 0 {
		t.Errorf("TargetCoverage(untouched) = (%v, %v), want (0, true)", pct, found)
	}

	noCoverageReport := "FAILED tests/test_behavior.py::test_no_race - AssertionError\n"
	if _, found := rules.TargetCoverage(noCoverageReport, target); found {
		t.Error("plain failure output with no coverage report must be found=false (no evidence)")
	}
}

// TestTargetCoverage_Go covers `go tool cover -func` per-function rows.
func TestTargetCoverage_Go(t *testing.T) {
	rules, ok := ecosystem.WitnessRulesFor(ecosystem.EcosystemGo)
	if !ok {
		t.Fatal("go witness rules missing")
	}
	target := "internal/widget/widget.go"

	covered := "internal/widget/widget.go:10:  New     100.0%\ntotal:                         (statements)    62.5%\n"
	if pct, found := rules.TargetCoverage(covered, target); !found || pct <= 0 {
		t.Errorf("TargetCoverage(covered) = (%v, %v), want positive coverage", pct, found)
	}

	// Ordinary go test failure output (no -func report at all): must be
	// permissive, not a negative signal, even though it names the TEST file.
	plainFailure := "--- FAIL: TestWidget (0.00s)\n    widget_test.go:10: assertion failed\nFAIL\n"
	if _, found := rules.TargetCoverage(plainFailure, target); found {
		t.Error("plain test failure output (no coverage report) must be found=false")
	}
}

// TestTargetCoverage_EmptyInputs covers the defensive zero-value cases.
func TestTargetCoverage_EmptyInputs(t *testing.T) {
	rules, _ := ecosystem.WitnessRulesFor(ecosystem.EcosystemPython)
	if _, found := rules.TargetCoverage("agent/main.py 1 0 100%", ""); found {
		t.Error("empty targetPath must never be found")
	}
	var zero ecosystem.WitnessRules
	if _, found := zero.TargetCoverage("agent/main.py 1 0 100%", "agent/main.py"); found {
		t.Error("zero-value WitnessRules (no patterns) must never be found")
	}
}
