//go:build integration

// Integration tests for the run_tests tool against a real container runtime.
// Run with:
//
//	go test -tags integration ./internal/agent/...
//
// Skips automatically when no container runtime (podman/docker) is detected or
// the image pull fails. Kept well under 90 seconds with a tight per-test timeout.
package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/sandbox"
)

// requireRuntime skips the test when no container runtime is available.
func requireRuntime(t *testing.T) sandbox.Sandbox {
	t.Helper()
	if _, ok := sandbox.Detect(); !ok {
		t.Skip("no container runtime (podman/docker) detected")
	}
	const image = "docker.io/library/golang:1.26-alpine"
	sb, err := sandbox.NewCLI("", image)
	if err != nil {
		t.Skipf("sandbox unavailable: %v", err)
	}
	return sb
}

// seedFile materialises a file under dir for integration tests.
func seedFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", name, err)
	}
}

// TestRunTestsTool_Integration_GoPassingSuite runs the test suite of a tiny
// seeded Go module that has no bugs — all tests pass, exit 0. Confirms the
// tool fires, the sandbox executes, and the summary shows exit_code=0.
func TestRunTestsTool_Integration_GoPassingSuite(t *testing.T) {
	sb := requireRuntime(t)

	repoDir := t.TempDir()
	seedFile(t, repoDir, "go.mod", "module integfixture\n\ngo 1.21\n")
	seedFile(t, repoDir, "math.go", `package integfixture

func Add(a, b int) int { return a + b }
`)
	seedFile(t, repoDir, "math_test.go", `package integfixture

import "testing"

func TestAdd(t *testing.T) {
	if Add(1, 2) != 3 {
		t.Fatalf("got %d, want 3", Add(1, 2))
	}
}
`)

	tool := NewRunTestsTool(
		sb,
		repoDir,
		[]string{"go", "test", "./..."},
		3,
		nil, nil, nil, nil,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Second)
	defer cancel()

	raw, _ := json.Marshal(map[string]interface{}{})
	out, err := tool.Run(ctx, raw)
	if err != nil {
		t.Fatalf("Run() returned error: %v", err)
	}
	if !strings.Contains(out, "exit_code=0") {
		t.Errorf("expected exit_code=0 in output; got:\n%s", out)
	}
}

// TestRunTestsTool_Integration_GoFailingSuite seeds a module with a test that
// is guaranteed to fail, then confirms the tool returns a normal result
// (not a tool error) and the output contains the failure evidence.
func TestRunTestsTool_Integration_GoFailingSuite(t *testing.T) {
	sb := requireRuntime(t)

	repoDir := t.TempDir()
	seedFile(t, repoDir, "go.mod", "module integfixture\n\ngo 1.21\n")
	seedFile(t, repoDir, "bug.go", `package integfixture

func Broken() bool { return false }
`)
	seedFile(t, repoDir, "bug_test.go", `package integfixture

import "testing"

func TestBroken(t *testing.T) {
	if !Broken() {
		t.Fatal("Broken() must return true but returned false")
	}
}
`)

	tool := NewRunTestsTool(
		sb,
		repoDir,
		[]string{"go", "test", "./..."},
		3,
		nil, nil, nil, nil,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Second)
	defer cancel()

	raw, _ := json.Marshal(map[string]interface{}{})
	out, err := tool.Run(ctx, raw)
	// A failing test is NOT a tool error; it is a normal result.
	if err != nil {
		t.Fatalf("Run() returned error (want normal result): %v", err)
	}
	if !strings.Contains(out, "exit_code=1") {
		t.Errorf("expected exit_code=1 in output; got:\n%s", out)
	}
	if !strings.Contains(out, "TestBroken") {
		t.Errorf("expected failing test name in output; got:\n%s", out)
	}
}

// TestRunTestsTool_Integration_BudgetEnforced_Real verifies that the budget
// guard fires even against a real sandbox: the first call succeeds, the second
// (budget=1) is rejected before reaching the container.
func TestRunTestsTool_Integration_BudgetEnforced_Real(t *testing.T) {
	sb := requireRuntime(t)

	repoDir := t.TempDir()
	seedFile(t, repoDir, "go.mod", "module integfixture\n\ngo 1.21\n")
	seedFile(t, repoDir, "x_test.go", `package integfixture

import "testing"

func TestPass(t *testing.T) {}
`)

	tool := NewRunTestsTool(sb, repoDir, []string{"go", "test", "./..."}, 1, nil, nil, nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Second)
	defer cancel()

	raw, _ := json.Marshal(map[string]interface{}{})
	if _, err := tool.Run(ctx, raw); err != nil {
		t.Fatalf("first call (within budget) failed: %v", err)
	}
	// Second call must be rejected by the budget guard before the sandbox runs.
	_, err := tool.Run(ctx, raw)
	if err == nil {
		t.Fatal("second call should be budget-rejected")
	}
	if !strings.Contains(err.Error(), "budget exhausted") {
		t.Errorf("budget error message missing 'budget exhausted': %v", err)
	}
	if tool.ExecCount() != 2 {
		t.Errorf("ExecCount = %d, want 2", tool.ExecCount())
	}
}
