package funnel

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/sandbox"
)

// --- detectTestCmd -----------------------------------------------------------

func TestDetectTestCmd_GoModule(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := detectTestCmd(dir)
	want := []string{"go", "test", "./..."}
	if !sliceEq(got, want) {
		t.Errorf("detectTestCmd(go.mod) = %v, want %v", got, want)
	}
}

func TestDetectTestCmd_Cargo(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Cargo.toml"), []byte("[package]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := detectTestCmd(dir)
	want := []string{"cargo", "test"}
	if !sliceEq(got, want) {
		t.Errorf("detectTestCmd(Cargo.toml) = %v, want %v", got, want)
	}
}

func TestDetectTestCmd_NPM(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := detectTestCmd(dir)
	want := []string{"npm", "test"}
	if !sliceEq(got, want) {
		t.Errorf("detectTestCmd(package.json) = %v, want %v", got, want)
	}
}

func TestDetectTestCmd_Python(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[tool.pytest]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := detectTestCmd(dir)
	want := []string{"python", "-m", "pytest"}
	if !sliceEq(got, want) {
		t.Errorf("detectTestCmd(pyproject.toml) = %v, want %v", got, want)
	}
}

func TestDetectTestCmd_EmptyDir_ReturnsNil(t *testing.T) {
	dir := t.TempDir()
	got := detectTestCmd(dir)
	if got != nil {
		t.Errorf("detectTestCmd(empty) = %v, want nil", got)
	}
}

func TestDetectTestCmd_CMake(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "CMakeLists.txt"), []byte("cmake_minimum_required(VERSION 3.20)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := detectTestCmd(dir)
	want := []string{"bash", "-c", "cmake -S . -B build -DCMAKE_BUILD_TYPE=Debug && cmake --build build --parallel 4 && ctest --test-dir build --output-on-failure --no-tests=ignore"}
	if !sliceEq(got, want) {
		t.Errorf("detectTestCmd(CMakeLists.txt) = %v, want %v", got, want)
	}
}

func TestDetectTestCmd_Meson(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "meson.build"), []byte("project('x', 'cpp')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := detectTestCmd(dir)
	want := []string{"bash", "-c", "meson setup build && meson test -C build --print-errorlogs"}
	if !sliceEq(got, want) {
		t.Errorf("detectTestCmd(meson.build) = %v, want %v", got, want)
	}
}

func TestDetectTestCmd_Bazel(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "MODULE.bazel"), []byte("module(name = \"x\")\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := detectTestCmd(dir)
	want := []string{"bazel", "test", "--build_tests_only", "--test_output=errors", "//..."}
	if !sliceEq(got, want) {
		t.Errorf("detectTestCmd(MODULE.bazel) = %v, want %v", got, want)
	}
}

// --- buildRunTestsTool -------------------------------------------------------

// fakeRunTestsSandbox is a minimal sandbox for buildRunTestsTool tests.
type fakeRunTestsSandbox struct{}

func (f *fakeRunTestsSandbox) Exec(_ context.Context, _ sandbox.Spec) (sandbox.Result, error) {
	return sandbox.Result{ExitCode: 0}, nil
}

var _ sandbox.Sandbox = (*fakeRunTestsSandbox)(nil)

func TestBuildRunTestsTool_FeatureOff(t *testing.T) {
	_, repo := openFixture(t)
	f := &Funnel{
		repo: repo,
		opts: Options{SandboxOpts: SandboxOpts{Enabled: false}},
	}
	var execs atomic.Int32
	var millis atomic.Int64
	if tool := f.buildRunTestsTool(&execs, &millis); tool != nil {
		t.Error("buildRunTestsTool must return nil when feature is disabled")
	}
}

func TestBuildRunTestsTool_NilSandbox(t *testing.T) {
	_, repo := openFixture(t)
	f := &Funnel{
		repo: repo,
		opts: Options{SandboxOpts: SandboxOpts{Enabled: true, Sandbox: nil}},
	}
	var execs atomic.Int32
	var millis atomic.Int64
	if tool := f.buildRunTestsTool(&execs, &millis); tool != nil {
		t.Error("buildRunTestsTool must return nil when sandbox is nil")
	}
}

func TestBuildRunTestsTool_EnabledWithGoModule_ReturnsTool(t *testing.T) {
	_, repo := openFixture(t)
	// The fixture repo must have a go.mod for the funnel tests to compile.
	cmd := detectTestCmd(repo.Root())
	if len(cmd) == 0 {
		t.Skip("fixture repo has no recognised build system")
	}

	sb := &fakeRunTestsSandbox{}
	var execs atomic.Int32
	var millis atomic.Int64
	f := &Funnel{
		repo: repo,
		opts: Options{SandboxOpts: SandboxOpts{Enabled: true, Sandbox: sb, MaxExecs: 2}},
	}
	tool := f.buildRunTestsTool(&execs, &millis)
	if tool == nil {
		t.Fatal("buildRunTestsTool should return a tool for enabled sandbox + Go repo")
	}
	if tool.Def().Name != "run_tests" {
		t.Errorf("tool name = %q, want run_tests", tool.Def().Name)
	}
}

func TestBuildRunTestsTool_OnExecAggregatesStats(t *testing.T) {
	_, repo := openFixture(t)
	cmd := detectTestCmd(repo.Root())
	if len(cmd) == 0 {
		t.Skip("fixture repo has no recognised build system")
	}

	sb := &fakeRunTestsSandbox{}
	var execs atomic.Int32
	var millis atomic.Int64
	f := &Funnel{
		repo: repo,
		opts: Options{SandboxOpts: SandboxOpts{Enabled: true, Sandbox: sb, MaxExecs: 3}},
	}
	tool := f.buildRunTestsTool(&execs, &millis)
	if tool == nil {
		t.Fatal("buildRunTestsTool returned nil")
	}

	raw, _ := json.Marshal(map[string]interface{}{})
	out, err := tool.Run(context.Background(), raw)
	if err != nil {
		t.Fatalf("tool.Run() error: %v", err)
	}
	if out == "" {
		t.Error("tool.Run() returned empty output")
	}
	// onExec must have incremented the shared counter.
	if execs.Load() != 1 {
		t.Errorf("sbExecs = %d after one run, want 1", execs.Load())
	}
}

// --- hasRunTests -------------------------------------------------------------

func TestHasRunTests_FindsTool(t *testing.T) {
	if hasRunTests(nil) {
		t.Error("hasRunTests(nil) = true, want false")
	}
	if hasRunTests([]agent.Tool{}) {
		t.Error("hasRunTests(empty slice) = true, want false")
	}
	// A tool with a different name must not be detected.
	sb := &fakeRunTestsSandbox{}
	notRunTests := agent.NewSandboxExecTool(sb, "/repo", 1, nil, nil, nil, nil)
	if hasRunTests([]agent.Tool{notRunTests}) {
		t.Error("hasRunTests([sandbox_exec]) = true, want false")
	}
	// A RunTestsTool must be detected.
	rt := agent.NewRunTestsTool(sb, "/repo", []string{"go", "test", "./..."}, 1, nil, nil, nil, nil)
	if !hasRunTests([]agent.Tool{rt}) {
		t.Error("hasRunTests([run_tests]) = false, want true")
	}
}

// --- helpers ----------------------------------------------------------------

func sliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
