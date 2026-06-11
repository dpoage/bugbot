package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/sandbox"
)

// --- Fake sandbox for tool tests -----------------------------------------

// fakeSandbox is a minimal sandbox.Sandbox implementation for tool tests.
// It records calls and returns scripted responses.
type fakeSandbox struct {
	results []sandbox.Result
	err     error
	calls   []sandbox.Spec
	callIdx int
}

func (f *fakeSandbox) Exec(_ context.Context, spec sandbox.Spec) (sandbox.Result, error) {
	f.calls = append(f.calls, spec)
	if f.err != nil {
		return sandbox.Result{}, f.err
	}
	if f.callIdx < len(f.results) {
		r := f.results[f.callIdx]
		f.callIdx++
		return r, nil
	}
	// Default: success with zero exit.
	return sandbox.Result{ExitCode: 0, Duration: 10 * time.Millisecond}, nil
}

var _ sandbox.Sandbox = (*fakeSandbox)(nil)

// --- Test helpers --------------------------------------------------------

func newToolWithFake(results []sandbox.Result) (*SandboxExecTool, *fakeSandbox) {
	fs := &fakeSandbox{results: results}
	tool := NewSandboxExecTool(fs, "/repo", 3, nil, nil, nil)
	return tool, fs
}

func runSandboxExecTool(t *testing.T, tool *SandboxExecTool, args interface{}) (string, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return tool.Run(context.Background(), raw)
}

// --- Tests ---------------------------------------------------------------

func TestSandboxExecTool_Def(t *testing.T) {
	tool, _ := newToolWithFake(nil)
	def := tool.Def()
	if def.Name != "sandbox_exec" {
		t.Errorf("tool name = %q, want sandbox_exec", def.Name)
	}
	if def.Description == "" {
		t.Error("tool description is empty")
	}
	// Schema must be valid JSON with a required "cmd" field.
	var schema map[string]interface{}
	if err := json.Unmarshal(def.Parameters, &schema); err != nil {
		t.Fatalf("parameters schema is not valid JSON: %v", err)
	}
}

func TestSandboxExecTool_NormalRun_ZeroExit(t *testing.T) {
	result := sandbox.Result{
		ExitCode: 0,
		Stdout:   "ok\n",
		Stderr:   "",
		Duration: 50 * time.Millisecond,
	}
	tool, fs := newToolWithFake([]sandbox.Result{result})

	out, err := runSandboxExecTool(t, tool, map[string]interface{}{
		"cmd": []string{"go", "test", "./..."},
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if !strings.Contains(out, "exit_code=0") {
		t.Errorf("output missing exit_code=0: %q", out)
	}
	if !strings.Contains(out, "ok") {
		t.Errorf("output missing stdout content: %q", out)
	}
	if len(fs.calls) != 1 {
		t.Errorf("sandbox called %d times, want 1", len(fs.calls))
	}
	if fs.calls[0].Network != "none" {
		t.Errorf("network = %q, want none", fs.calls[0].Network)
	}
	if fs.calls[0].RepoDir != "/repo" {
		t.Errorf("repoDir = %q, want /repo", fs.calls[0].RepoDir)
	}
}

func TestSandboxExecTool_NonZeroExit_IsNormalResult(t *testing.T) {
	// Non-zero exit must NOT be a tool error; it is a normal result the model
	// interprets.
	result := sandbox.Result{
		ExitCode: 1,
		Stdout:   "FAIL\n",
		Stderr:   "panic: nil pointer\n",
		Duration: 100 * time.Millisecond,
	}
	tool, _ := newToolWithFake([]sandbox.Result{result})

	out, err := runSandboxExecTool(t, tool, map[string]interface{}{
		"cmd": []string{"go", "test", "-run", "TestFoo", "./..."},
	})
	if err != nil {
		t.Fatalf("non-zero exit must not be a tool error; got: %v", err)
	}
	if !strings.Contains(out, "exit_code=1") {
		t.Errorf("output missing exit_code=1: %q", out)
	}
	if !strings.Contains(out, "FAIL") {
		t.Errorf("output missing stdout: %q", out)
	}
	if !strings.Contains(out, "panic: nil pointer") {
		t.Errorf("output missing stderr: %q", out)
	}
}

func TestSandboxExecTool_TimedOut(t *testing.T) {
	result := sandbox.Result{
		ExitCode: -1,
		TimedOut: true,
		Duration: 60 * time.Second,
	}
	tool, _ := newToolWithFake([]sandbox.Result{result})

	out, err := runSandboxExecTool(t, tool, map[string]interface{}{
		"cmd": []string{"go", "test", "./..."},
	})
	if err != nil {
		t.Fatalf("timeout must not be a tool error; got: %v", err)
	}
	if !strings.Contains(out, "timed_out=true") {
		t.Errorf("output missing timed_out=true: %q", out)
	}
}

func TestSandboxExecTool_EmptyCmd_IsToolError(t *testing.T) {
	tool, _ := newToolWithFake(nil)
	_, err := runSandboxExecTool(t, tool, map[string]interface{}{
		"cmd": []string{},
	})
	if err == nil {
		t.Fatal("empty cmd must return a tool error")
	}
	if !strings.Contains(err.Error(), "cmd") {
		t.Errorf("error should mention cmd: %v", err)
	}
}

func TestSandboxExecTool_MissingCmd_IsToolError(t *testing.T) {
	tool, _ := newToolWithFake(nil)
	_, err := runSandboxExecTool(t, tool, map[string]interface{}{
		"files": map[string]string{"test.go": "package x"},
	})
	if err == nil {
		t.Fatal("missing cmd must return a tool error")
	}
}

func TestSandboxExecTool_WithFiles(t *testing.T) {
	tool, fs := newToolWithFake(nil)
	_, err := runSandboxExecTool(t, tool, map[string]interface{}{
		"cmd":   []string{"go", "test", "./..."},
		"files": map[string]string{"probe_test.go": "package x\nimport \"testing\"\nfunc TestProbe(t *testing.T) {}"},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(fs.calls) != 1 {
		t.Fatalf("want 1 sandbox call, got %d", len(fs.calls))
	}
	if len(fs.calls[0].WriteFiles) != 1 {
		t.Errorf("WriteFiles count = %d, want 1", len(fs.calls[0].WriteFiles))
	}
}

func TestSandboxExecTool_InfraError_IsToolError(t *testing.T) {
	fs := &fakeSandbox{err: errors.New("podman not found")}
	tool := NewSandboxExecTool(fs, "/repo", 3, nil, nil, nil)

	_, err := runSandboxExecTool(t, tool, map[string]interface{}{
		"cmd": []string{"go", "test", "./..."},
	})
	if err == nil {
		t.Fatal("infra error must return a tool error")
	}
	if !strings.Contains(err.Error(), "sandbox execution failed") {
		t.Errorf("error should wrap infra error: %v", err)
	}
}

func TestSandboxExecTool_BudgetExhausted(t *testing.T) {
	tool, fs := newToolWithFake(nil)
	// maxExec = 2
	tool2 := NewSandboxExecTool(fs, "/repo", 2, nil, nil, nil)

	// First two calls succeed.
	args := map[string]interface{}{"cmd": []string{"go", "test", "./..."}}
	for i := 0; i < 2; i++ {
		if _, err := runSandboxExecTool(t, tool2, args); err != nil {
			t.Fatalf("call %d should succeed: %v", i+1, err)
		}
	}
	// Third call must fail with a budget message.
	_, err := runSandboxExecTool(t, tool2, args)
	if err == nil {
		t.Fatal("third call should return a budget-exhausted error")
	}
	if !strings.Contains(err.Error(), "budget exhausted") {
		t.Errorf("error should mention budget: %v", err)
	}
	// The sandbox should have been called exactly twice (budget check fires before 3rd exec).
	if len(fs.calls) != 2 {
		t.Errorf("sandbox called %d times, want 2 (budget check fires before 3rd exec)", len(fs.calls))
	}
	_ = tool // silence unused warning
}

func TestSandboxExecTool_StdoutTruncation(t *testing.T) {
	// Build output that exceeds the 16 KiB cap.
	bigOutput := strings.Repeat("x", sandboxOutputCap+100)
	result := sandbox.Result{
		ExitCode: 0,
		Stdout:   bigOutput,
		Duration: 10 * time.Millisecond,
	}
	tool, _ := newToolWithFake([]sandbox.Result{result})

	out, err := runSandboxExecTool(t, tool, map[string]interface{}{
		"cmd": []string{"go", "test", "./..."},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !strings.Contains(out, "[stdout truncated at 16KiB]") {
		t.Errorf("expected truncation marker in output: %q", out[:100])
	}
}

func TestSandboxExecTool_StderrTruncation(t *testing.T) {
	bigErr := strings.Repeat("e", sandboxOutputCap+50)
	result := sandbox.Result{
		ExitCode: 1,
		Stderr:   bigErr,
		Duration: 10 * time.Millisecond,
	}
	tool, _ := newToolWithFake([]sandbox.Result{result})

	out, err := runSandboxExecTool(t, tool, map[string]interface{}{
		"cmd": []string{"go", "test", "./..."},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !strings.Contains(out, "[stderr truncated at 16KiB]") {
		t.Errorf("expected stderr truncation marker: %q", out[:100])
	}
}

func TestSandboxExecTool_DurationRecorded(t *testing.T) {
	result := sandbox.Result{
		ExitCode: 0,
		Duration: 123 * time.Millisecond,
	}
	var recorded time.Duration
	fs := &fakeSandbox{results: []sandbox.Result{result}}
	tool := NewSandboxExecTool(fs, "/repo", 3, nil, nil, func(d time.Duration) {
		recorded = d
	})

	if _, err := runSandboxExecTool(t, tool, map[string]interface{}{
		"cmd": []string{"go", "test", "./..."},
	}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if recorded != 123*time.Millisecond {
		t.Errorf("recorded duration = %v, want 123ms", recorded)
	}
}

func TestRenderSandboxResult_EmptyStreams(t *testing.T) {
	r := sandbox.Result{
		ExitCode: 0,
		Duration: 5 * time.Millisecond,
	}
	out := renderSandboxResult(r)
	if !strings.Contains(out, "exit_code=0") {
		t.Errorf("missing exit_code=0: %q", out)
	}
	if !strings.Contains(out, "(empty)") {
		t.Errorf("expected (empty) for blank streams: %q", out)
	}
}
