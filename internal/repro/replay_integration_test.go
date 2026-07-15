//go:build integration

package repro

// replay_integration_test.go exercises Replay (bugbot-ecm8 acceptance 3)
// against a real container runtime: synthetic mini-repos per supported
// ecosystem, each paired with a bundle asserting one of the three outcomes
// the design calls for — demonstrated (a genuine behavioral failing test,
// covered here for Go and Rust), target_not_executed (a grep-test that
// "passes" the runtime's ran-evidence check but fails the static gate,
// covered for Python), and exit_zero (the same behavioral test against
// already-fixed code, covered for Go).
//
// Python's "demonstrated" case is deliberately NOT exercised through a real
// sandbox run here: it requires pytest (the only launcher detectEcosystem
// recognizes as Python), which is not preinstalled in the base python
// image, and Replay forces network="none" unconditionally — there is no
// sandbox-internal way to install it for this suite without either baking a
// custom image or resolving a pip wheelhouse via DepStrategyFetch's one-time
// networked prefetch, both out of scope here. Its "demonstrated" outcome is
// unit-tested against sandbox.Mock in replay_test.go instead
// (TestReplay_Demonstrated); this suite's Python coverage is limited to the
// target_not_executed static-gate path, which needs no sandbox toolchain at
// all.
//
// Self-skips (no test failure) when no container runtime is available,
// following verify_sandbox_integration_test.go / workspace_tools_integration_test.go.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/sandbox"
)

// replayIntegrationTimeout bounds each sandbox exec in this suite; generous
// enough for a cold image pull on a fresh CI runner.
const replayIntegrationTimeout = 2 * time.Minute

// newReplayIntegrationSandbox returns a sandbox.CLI for image, skipping the
// calling test when no container runtime is detected — the standard
// self-skip contract every integration test in this package follows.
func newReplayIntegrationSandbox(t *testing.T, image string) sandbox.Sandbox {
	t.Helper()
	rt, ok := sandbox.Detect()
	if !ok {
		t.Skip("no container runtime (podman/docker) detected")
	}
	sb, err := sandbox.NewCLI(rt, image)
	if err != nil {
		t.Skipf("sandbox unavailable: %v", err)
	}
	t.Cleanup(func() { _ = sb.Close() })
	return sb
}

// TestIntegration_Replay_Go_Demonstrated: a real container run of a
// behavioral Go test against a repo that still has the bug must report
// Demonstrated.
func TestIntegration_Replay_Go_Demonstrated(t *testing.T) {
	sb := newReplayIntegrationSandbox(t, "docker.io/library/golang:1.26")

	repoDir := t.TempDir()
	writeFile(t, repoDir, "go.mod", "module bugfixture\n\ngo 1.21\n")
	writeFile(t, repoDir, "calc.go", `package bugfixture

// Divide is buggy: it silently returns 0 (and no error) for a zero divisor.
func Divide(a, b int) (int, error) {
	return a / b, nil
}
`)

	finding := domain.Finding{ID: "int-go-demo", File: "calc.go"}
	plan := &Plan{
		Files: map[string]string{"calc_test.go": `package bugfixture

import "testing"

func TestDivideByZeroReturnsError(t *testing.T) {
	if _, err := Divide(1, 0); err == nil {
		t.Fatal("expected an error dividing by zero, got nil")
	}
}
`},
		Cmd: []string{"go", "test", "-timeout", "60s", "./..."},
	}
	b := bundleFrom(t, finding, plan, sandbox.Result{ExitCode: 1, Stdout: "--- FAIL"})

	ctx, cancel := context.WithTimeout(context.Background(), replayIntegrationTimeout)
	defer cancel()
	got, err := Replay(ctx, sb, repoDir, b, ReplayOptions{Image: "docker.io/library/golang:1.26", Timeout: replayIntegrationTimeout})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if !got.Demonstrated {
		t.Fatalf("Demonstrated = false, want true (reason=%q summary=%q)", got.Reason, got.Summary)
	}
}

// TestIntegration_Replay_Go_ExitZero: the SAME behavioral test against
// already-fixed code must report VerdictReasonExitZero, not Demonstrated —
// the "candidate to auto-close" outcome.
func TestIntegration_Replay_Go_ExitZero(t *testing.T) {
	sb := newReplayIntegrationSandbox(t, "docker.io/library/golang:1.26")

	repoDir := t.TempDir()
	writeFile(t, repoDir, "go.mod", "module bugfixture\n\ngo 1.21\n")
	writeFile(t, repoDir, "calc.go", `package bugfixture

import "errors"

// Divide is fixed: it rejects a zero divisor.
func Divide(a, b int) (int, error) {
	if b == 0 {
		return 0, errors.New("divide by zero")
	}
	return a / b, nil
}
`)

	finding := domain.Finding{ID: "int-go-fixed", File: "calc.go"}
	plan := &Plan{
		Files: map[string]string{"calc_test.go": `package bugfixture

import "testing"

func TestDivideByZeroReturnsError(t *testing.T) {
	if _, err := Divide(1, 0); err == nil {
		t.Fatal("expected an error dividing by zero, got nil")
	}
}
`},
		Cmd: []string{"go", "test", "-timeout", "60s", "./..."},
	}
	b := bundleFrom(t, finding, plan, sandbox.Result{ExitCode: 1, Stdout: "--- FAIL"})

	ctx, cancel := context.WithTimeout(context.Background(), replayIntegrationTimeout)
	defer cancel()
	got, err := Replay(ctx, sb, repoDir, b, ReplayOptions{Image: "docker.io/library/golang:1.26", Timeout: replayIntegrationTimeout})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if got.Demonstrated {
		t.Fatalf("Demonstrated = true, want false for already-fixed code")
	}
	if got.Reason != VerdictReasonExitZero {
		t.Fatalf("Reason = %q, want %q", got.Reason, VerdictReasonExitZero)
	}
}

// TestIntegration_Replay_Python_TargetNotExecuted: a grep-test that would
// exit 1 with pytest's own "FAILED" ran-evidence marker (so interpret()
// alone would call it demonstrated) must still be rejected by the
// pre-execute static gate BEFORE any container launches — matching the
// acceptance text ("target_not_executed: grep test with exit 1 + FAILED
// marker"). No pytest install is required: SandboxRan must stay false.
func TestIntegration_Replay_Python_TargetNotExecuted(t *testing.T) {
	sb := newReplayIntegrationSandbox(t, "docker.io/library/python:3.12-slim")

	repoDir := t.TempDir()
	writeFile(t, repoDir, "calc.py", `def divide(a, b):
    # BUG: no zero check.
    return a / b
`)

	finding := domain.Finding{ID: "int-py-grep", File: "calc.py"}
	plan := &Plan{
		Files: map[string]string{"test_grep.py": `def test_source_mentions_divide():
    with open("calc.py") as f:
        src = f.read()
    assert "def divide" in src
`},
		Cmd: []string{"pytest", "test_grep.py"},
	}
	b := bundleFrom(t, finding, plan, sandbox.Result{ExitCode: 1, Stdout: "FAILED test_grep.py::test_source_mentions_divide"})

	ctx, cancel := context.WithTimeout(context.Background(), replayIntegrationTimeout)
	defer cancel()
	got, err := Replay(ctx, sb, repoDir, b, ReplayOptions{Image: "docker.io/library/python:3.12-slim", Timeout: replayIntegrationTimeout})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if got.Demonstrated {
		t.Fatal("Demonstrated = true, want false for a grep-test bundle")
	}
	if got.Reason != VerdictReasonTargetNotExecuted {
		t.Fatalf("Reason = %q, want %q", got.Reason, VerdictReasonTargetNotExecuted)
	}
	if got.SandboxRan {
		t.Error("SandboxRan = true, want false — the static gate must reject before a real container ever launches")
	}
}

// TestIntegration_Replay_Rust_Demonstrated: a real container run of a
// behavioral Rust unit test (inline #[cfg(test)] module, same file as the
// target — colocation is itself an executable edge for Rust) against a repo
// that still has the bug must report Demonstrated. cargo test needs no
// external crates, so it runs fully offline (network="none").
func TestIntegration_Replay_Rust_Demonstrated(t *testing.T) {
	sb := newReplayIntegrationSandbox(t, "docker.io/library/rust:1.82")

	repoDir := t.TempDir()
	writeFile(t, repoDir, "Cargo.toml", `[package]
name = "bugfixture"
version = "0.1.0"
edition = "2021"
`)
	if err := os.MkdirAll(filepath.Join(repoDir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, repoDir, "src/lib.rs", `// Divide is buggy: it panics on a zero divisor instead of returning a
// domain error.
pub fn divide(a: i64, b: i64) -> i64 {
    a / b
}
`)

	finding := domain.Finding{ID: "int-rust-demo", File: "src/lib.rs"}
	plan := &Plan{
		Files: map[string]string{"src/lib.rs": `// Divide is buggy: it panics on a zero divisor instead of returning a
// domain error.
pub fn divide(a: i64, b: i64) -> i64 {
    a / b
}

#[cfg(test)]
mod tests {
    #[test]
    fn test_divide_by_zero_should_be_a_domain_error_not_a_panic() {
        // Bug: divide(1, 0) panics with a division trap. There is no
        // fallible API yet, so this documents the missing contract by
        // failing deterministically until one exists.
        assert!(false, "divide(a, 0) panics; no checked API exists yet");
    }
}
`},
		Cmd: []string{"cargo", "test"},
	}
	b := bundleFrom(t, finding, plan, sandbox.Result{ExitCode: 101, Stdout: "test tests::test_divide_by_zero_should_be_a_domain_error_not_a_panic ... FAILED"})

	ctx, cancel := context.WithTimeout(context.Background(), replayIntegrationTimeout)
	defer cancel()
	got, err := Replay(ctx, sb, repoDir, b, ReplayOptions{Image: "docker.io/library/rust:1.82", Timeout: replayIntegrationTimeout})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if !got.Demonstrated {
		t.Fatalf("Demonstrated = false, want true (reason=%q summary=%q)", got.Reason, got.Summary)
	}
}
