package repro

import (
	"testing"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/sandbox"
)

// TestAudit_FlagsGrepTest exercises the pure static path end to end: write a
// bundle whose test only reads the target's source text, then Audit it with
// no sandbox involved at all.
func TestAudit_FlagsGrepTest(t *testing.T) {
	finding := domain.Finding{ID: "f1", File: "src/store/SelectedContractStore.ts"}
	plan := &Plan{
		Files: map[string]string{
			"test_grep.py": "import unittest\n\nclass T(unittest.TestCase):\n" +
				"    def test_x(self):\n        with open('src/store/SelectedContractStore.ts') as f:\n            src = f.read()\n" +
				"        self.assertIn('SelectedContractStore.getValue()', src)\n",
		},
		Cmd: []string{"pytest", "test_grep.py"},
	}
	b := bundleFrom(t, finding, plan, sandbox.Result{ExitCode: 1, Stdout: "FAILED"})

	got := Audit(b)
	if !got.Flagged() {
		t.Fatal("Flagged() = false, want true for a source-grep bundle")
	}
	if got.Reason != VerdictReasonTargetNotExecuted {
		t.Errorf("Reason = %q, want %q", got.Reason, VerdictReasonTargetNotExecuted)
	}
	if got.Detail == "" {
		t.Error("Detail is empty, want a human-readable explanation")
	}
}

// TestAudit_PassesGenuineBehavioralTest ensures a bundle whose test actually
// imports and exercises the target is NOT flagged.
func TestAudit_PassesGenuineBehavioralTest(t *testing.T) {
	finding := domain.Finding{ID: "f2", File: "agent/main.py"}
	plan := &Plan{
		Files: map[string]string{
			"test_behavior.py": "import unittest\nfrom agent.main import compute_sleep_time\n\n" +
				"class T(unittest.TestCase):\n    def test_no_race(self):\n        self.assertGreaterEqual(compute_sleep_time(), 0)\n",
		},
		Cmd: []string{"pytest", "test_behavior.py"},
	}
	b := bundleFrom(t, finding, plan, sandbox.Result{ExitCode: 1, Stdout: "FAILED"})

	got := Audit(b)
	if got.Flagged() {
		t.Errorf("Flagged() = true (reason=%q detail=%q), want false for a genuine behavioral test", got.Reason, got.Detail)
	}
}
