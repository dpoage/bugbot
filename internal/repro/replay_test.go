package repro

import (
	"context"
	"testing"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/sandbox"
)

// bundleFrom writes a bundle via writeArtifacts and loads it back, for tests
// that need a real on-disk Bundle rather than a hand-built Manifest.
func bundleFrom(t *testing.T, finding domain.Finding, plan *Plan, res sandbox.Result) *Bundle {
	t.Helper()
	dir, err := writeArtifacts(t.TempDir(), finding, plan, res, "golang:1.21", "none")
	if err != nil {
		t.Fatalf("writeArtifacts: %v", err)
	}
	b, err := LoadBundle(dir)
	if err != nil {
		t.Fatalf("LoadBundle: %v", err)
	}
	return b
}

// TestReplay_Demonstrated exercises the happy path: the static gate raises no
// objection, the sandbox reports the same failing run, and Replay reports
// Demonstrated with the interpreted verdict.
func TestReplay_Demonstrated(t *testing.T) {
	finding := domain.Finding{ID: "f1", File: "pkg/calc.go"}
	plan := &Plan{
		Files: map[string]string{"calc_test.go": "package pkg\n\nimport \"pkg\"\n\nfunc TestBug(t *testing.T){}\n"},
		Cmd:   []string{"go", "test", "./..."},
	}
	seedRes := sandbox.Result{ExitCode: 1, Stdout: "--- FAIL: TestBug\nFAIL"}
	b := bundleFrom(t, finding, plan, seedRes)

	mock := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 1, Stdout: "--- FAIL: TestBug\nFAIL"}})
	got, err := Replay(context.Background(), mock, t.TempDir(), b, ReplayOptions{})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if !got.Demonstrated {
		t.Errorf("Demonstrated = false, want true (reason=%q summary=%q)", got.Reason, got.Summary)
	}
	if !got.SandboxRan {
		t.Error("SandboxRan = false, want true")
	}
	if mock.CallCount() != 1 {
		t.Fatalf("sandbox Exec called %d times, want 1", mock.CallCount())
	}
	if got := mock.Calls()[0].Spec.Network; got != "none" {
		t.Errorf("Spec.Network = %q, want %q (replay must preserve network=none)", got, "none")
	}
}

// TestReplay_ExitZero covers the "likely fixed" outcome: the sandbox now
// exits cleanly, so Replay must NOT report Demonstrated.
func TestReplay_ExitZero(t *testing.T) {
	finding := domain.Finding{ID: "f2", File: "pkg/calc.go"}
	plan := &Plan{
		Files: map[string]string{"calc_test.go": "package pkg\n\nimport \"pkg\"\n\nfunc TestBug(t *testing.T){}\n"},
		Cmd:   []string{"go", "test", "./..."},
	}
	b := bundleFrom(t, finding, plan, sandbox.Result{ExitCode: 1, Stdout: "--- FAIL: TestBug\nFAIL"})

	mock := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 0, Stdout: "ok\tpkg\t0.002s"}})
	got, err := Replay(context.Background(), mock, t.TempDir(), b, ReplayOptions{})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if got.Demonstrated {
		t.Error("Demonstrated = true, want false for a clean exit")
	}
	if got.Reason != VerdictReasonExitZero {
		t.Errorf("Reason = %q, want %q", got.Reason, VerdictReasonExitZero)
	}
}

// TestReplay_GateRejectsWithoutSandbox proves the pre-execute static gate
// short-circuits Replay before any sandbox call when the bundle's own test
// files never reach the target through an executable edge — a grep-test
// bundle must not even spend a sandbox run to be classified.
func TestReplay_GateRejectsWithoutSandbox(t *testing.T) {
	finding := domain.Finding{ID: "f3", File: "pkg/calc.py"}
	plan := &Plan{
		Files: map[string]string{
			"test_grep.py": "import unittest\n\nclass T(unittest.TestCase):\n" +
				"    def test_x(self):\n        with open('pkg/calc.py') as f:\n            src = f.read()\n" +
				"        self.assertIn('def add', src)\n",
		},
		Cmd: []string{"pytest", "test_grep.py"},
	}
	b := bundleFrom(t, finding, plan, sandbox.Result{ExitCode: 1, Stdout: "FAILED (failures=1)"})

	mock := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 1, Stdout: "FAILED (failures=1)"}})
	got, err := Replay(context.Background(), mock, t.TempDir(), b, ReplayOptions{})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if got.Demonstrated {
		t.Error("Demonstrated = true, want false for a grep-test bundle")
	}
	if got.Reason != VerdictReasonTargetNotExecuted {
		t.Errorf("Reason = %q, want %q", got.Reason, VerdictReasonTargetNotExecuted)
	}
	if got.SandboxRan {
		t.Error("SandboxRan = true, want false — the static gate must reject before spending a sandbox run")
	}
	if mock.CallCount() != 0 {
		t.Errorf("sandbox Exec called %d times, want 0", mock.CallCount())
	}
}
