package repro

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/sandbox"
)

// TestClassifySmoke_OK covers a clean exit (toolchain ran successfully).
func TestClassifySmoke_OK(t *testing.T) {
	res := sandbox.Result{ExitCode: 0, Stdout: "ok\n"}
	v := classifySmoke(res, []string{"go", "vet", "./..."})
	if !v.OK || v.Category != SmokeCategoryOK {
		t.Errorf("clean exit: got ok=%v category=%q, want ok=true category=ok", v.OK, v.Category)
	}
}

// TestClassifySmoke_Timeout covers a timed-out run.
func TestClassifySmoke_Timeout(t *testing.T) {
	res := sandbox.Result{ExitCode: -1, TimedOut: true, Stderr: "killed"}
	v := classifySmoke(res, []string{"go", "vet", "./..."})
	if v.OK || v.Category != SmokeCategoryTimeout {
		t.Errorf("timeout: got ok=%v category=%q, want ok=false category=timeout", v.OK, v.Category)
	}
}

// TestClassifySmoke_Exit125 covers exit 125 (container runtime / shell failure).
func TestClassifySmoke_Exit125(t *testing.T) {
	res := sandbox.Result{ExitCode: 125, Stderr: "setup cmd failed"}
	v := classifySmoke(res, []string{"go", "vet", "./..."})
	if v.OK || v.Category != SmokeCategoryToolchainMissing {
		t.Errorf("exit 125: got ok=%v category=%q, want ok=false category=toolchain_missing", v.OK, v.Category)
	}
}

// TestClassifySmoke_Exit126 covers exit 126 (command not executable).
func TestClassifySmoke_Exit126(t *testing.T) {
	res := sandbox.Result{ExitCode: 126, Stderr: "permission denied"}
	v := classifySmoke(res, []string{"cargo", "metadata"})
	if v.OK || v.Category != SmokeCategoryToolchainMissing {
		t.Errorf("exit 126: got ok=%v category=%q, want ok=false category=toolchain_missing", v.OK, v.Category)
	}
}

// TestClassifySmoke_Exit127 covers exit 127 (command not found at shell level).
func TestClassifySmoke_Exit127(t *testing.T) {
	res := sandbox.Result{ExitCode: 127, Stderr: "go: command not found"}
	v := classifySmoke(res, []string{"go", "vet", "./..."})
	if v.OK || v.Category != SmokeCategoryToolchainMissing {
		t.Errorf("exit 127: got ok=%v category=%q, want ok=false category=toolchain_missing", v.OK, v.Category)
	}
}

// TestClassifySmoke_EnvMarkers covers environment-level failures (read-only fs, disk full, etc.).
func TestClassifySmoke_EnvMarkers(t *testing.T) {
	cases := []struct {
		name   string
		output string
	}{
		{"read-only fs", "error: read-only file system\n"},
		{"disk full", "write /tmp/x: no space left on device\n"},
		{"build cache init", "failed to initialize build cache\n"},
		{"cannot create tmp", "cannot create temporary directory\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := sandbox.Result{ExitCode: 1, Stderr: tc.output}
			v := classifySmoke(res, []string{"go", "vet", "./..."})
			if v.OK || v.Category != SmokeCategoryEnvError {
				t.Errorf("%s: got ok=%v category=%q, want ok=false category=env_error", tc.name, v.OK, v.Category)
			}
		})
	}
}

// TestClassifySmoke_ToolchainMissing covers command-not-found style output
// without the 125/126/127 exit codes (some runtimes emit these as exit 1).
func TestClassifySmoke_ToolchainMissing(t *testing.T) {
	cases := []struct {
		name   string
		output string
	}{
		{"bash command not found", "bash: go: command not found\n"},
		{"shell executable not found", "/bin/sh: go: executable file not found in $PATH\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := sandbox.Result{ExitCode: 1, Stderr: tc.output}
			v := classifySmoke(res, []string{"go", "vet", "./..."})
			if v.OK || v.Category != SmokeCategoryToolchainMissing {
				t.Errorf("%s: got ok=%v category=%q, want ok=false category=toolchain_missing", tc.name, v.OK, v.Category)
			}
		})
	}
}

// TestClassifySmoke_DepMissing covers dependency resolution failures.
func TestClassifySmoke_DepMissing(t *testing.T) {
	cases := []struct {
		name   string
		output string
	}{
		{"go mod missing", "no required module provides package foo/bar\n"},
		{"go cannot find module", "cannot find module providing package foo\n"},
		{"python no module", "ModuleNotFoundError: No module named 'pytest'\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := sandbox.Result{ExitCode: 1, Stderr: tc.output}
			v := classifySmoke(res, []string{"go", "vet", "./..."})
			if v.OK || v.Category != SmokeCategoryDepMissing {
				t.Errorf("%s: got ok=%v category=%q, want ok=false category=dep_missing", tc.name, v.OK, v.Category)
			}
		})
	}
}

// TestClassifySmoke_RealFailureNotMisread is the critical correctness test:
// a genuine compilation or test failure (toolchain ran, but things are broken)
// MUST be classified as ok=true, NOT as toolchain_missing or env_error.
// This ensures we never misclassify a real test failure as a toolchain problem.
func TestClassifySmoke_RealFailureNotMisread(t *testing.T) {
	cases := []struct {
		name   string
		cmd    []string
		output string
	}{
		{
			name:   "go vet compile error",
			cmd:    []string{"go", "vet", "./..."},
			output: "# mypackage\n./foo.go:10:5: undefined: Bar\n",
		},
		{
			name:   "go test real failure",
			cmd:    []string{"go", "test", "./..."},
			output: "--- FAIL: TestFoo (0.01s)\n    foo_test.go:12: got 1 want 2\nFAIL\tgithub.com/example/foo\n",
		},
		{
			name:   "cargo build error",
			cmd:    []string{"cargo", "metadata", "--no-deps"},
			output: "error[E0308]: mismatched types\n  --> src/main.rs:5:5\n",
		},
		{
			name:   "pytest collection error (import but pytest ran)",
			cmd:    []string{"python", "-m", "pytest", "--collect-only"},
			output: "collected 0 items / 1 error\nImportError while importing test module\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := sandbox.Result{ExitCode: 1, Stdout: tc.output}
			v := classifySmoke(res, tc.cmd)
			// The toolchain RAN and reported a real failure.
			// We want ok=true ("toolchain is present") — we are NOT checking
			// whether the project is green, only whether the toolchain exists.
			if !v.OK || v.Category != SmokeCategoryOK {
				t.Errorf("%s: got ok=%v category=%q, want ok=true category=ok\noutput: %s",
					tc.name, v.OK, v.Category, tc.output)
			}
		})
	}
}

// TestVerifySandbox_MockOK exercises the full VerifySandbox path against a Mock
// sandbox returning a clean result.
func TestVerifySandbox_MockOK(t *testing.T) {
	ctx := context.Background()
	m := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 0, Stdout: "ok\n"}})

	// Use a temp dir with a go.mod so detectSuiteCmd returns a Go command.
	dir := t.TempDir()
	if err := writeFileBytes(dir+"/go.mod", []byte("module example.com/x\ngo 1.21\n")); err != nil {
		t.Fatal(err)
	}

	spec := sandbox.Spec{Image: "golang:1.21"}
	res := sandbox.Resolution{}
	verdict, err := VerifySandbox(ctx, m, dir, spec, res)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !verdict.OK || verdict.Category != SmokeCategoryOK {
		t.Errorf("got ok=%v category=%q, want ok=true category=ok", verdict.OK, verdict.Category)
	}
	if m.CallCount() != 1 {
		t.Errorf("expected 1 sandbox call, got %d", m.CallCount())
	}
}

// TestVerifySandbox_MockToolchainMissing exercises exit-127 classification
// through the full VerifySandbox path.
func TestVerifySandbox_MockToolchainMissing(t *testing.T) {
	ctx := context.Background()
	m := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{
		ExitCode: 127,
		Stderr:   "go: command not found",
	}})

	dir := t.TempDir()
	if err := writeFileBytes(dir+"/go.mod", []byte("module example.com/x\ngo 1.21\n")); err != nil {
		t.Fatal(err)
	}

	spec := sandbox.Spec{Image: "debian:slim"}
	res := sandbox.Resolution{}
	verdict, err := VerifySandbox(ctx, m, dir, spec, res)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if verdict.OK || verdict.Category != SmokeCategoryToolchainMissing {
		t.Errorf("got ok=%v category=%q, want ok=false category=toolchain_missing", verdict.OK, verdict.Category)
	}
}

// TestVerifySandbox_ResolutionMountsForwarded verifies that the Resolution's
// ROMounts and Env are forwarded to the sandbox Spec.
func TestVerifySandbox_ResolutionMountsForwarded(t *testing.T) {
	ctx := context.Background()
	var captured sandbox.Spec
	m := &sandbox.Mock{}
	m.ResponseFunc = func(_ int, spec sandbox.Spec) (sandbox.Result, error) {
		captured = spec
		return sandbox.Result{ExitCode: 0}, nil
	}

	dir := t.TempDir()
	if err := writeFileBytes(dir+"/go.mod", []byte("module example.com/x\ngo 1.21\n")); err != nil {
		t.Fatal(err)
	}

	spec := sandbox.Spec{
		Image: "golang:1.21",
		Env:   []string{"GOFLAGS=-mod=vendor"},
		ROMounts: []sandbox.ROMount{
			{HostPath: "/host/modcache", ContainerPath: "/root/go/pkg/mod"},
		},
	}
	res := sandbox.Resolution{
		Env: []string{"GOPATH=/go"},
		ROMounts: []sandbox.ROMount{
			{HostPath: "/host/cache2", ContainerPath: "/tmp/cache2"},
		},
		SetupCmds: [][]string{{"mkdir", "-p", "/go/pkg/mod"}},
	}
	if _, err := VerifySandbox(ctx, m, dir, spec, res); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Env must contain both spec and resolution env.
	wantEnv := map[string]bool{"GOFLAGS=-mod=vendor": true, "GOPATH=/go": true}
	for _, e := range captured.Env {
		delete(wantEnv, e)
	}
	if len(wantEnv) > 0 {
		t.Errorf("missing env entries in captured spec: %v (got %v)", wantEnv, captured.Env)
	}

	// Mounts must contain both spec and resolution mounts.
	if len(captured.ROMounts) < 2 {
		t.Errorf("expected >=2 ROMounts, got %d: %v", len(captured.ROMounts), captured.ROMounts)
	}

	// SetupCmds from resolution must be forwarded.
	if len(captured.SetupCmds) == 0 {
		t.Errorf("SetupCmds not forwarded to sandbox spec")
	}

	// Network must be "none".
	if captured.Network != "none" {
		t.Errorf("network=%q, want %q", captured.Network, "none")
	}
}

// TestVerifySandbox_Timeout exercises the TimedOut path through VerifySandbox.
func TestVerifySandbox_Timeout(t *testing.T) {
	ctx := context.Background()
	m := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{
		TimedOut: true,
		Duration: smokeTimeout,
	}})

	dir := t.TempDir()
	if err := writeFileBytes(dir+"/go.mod", []byte("module example.com/x\ngo 1.21\n")); err != nil {
		t.Fatal(err)
	}

	verdict, err := VerifySandbox(ctx, m, dir, sandbox.Spec{}, sandbox.Resolution{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if verdict.OK || verdict.Category != SmokeCategoryTimeout {
		t.Errorf("got ok=%v category=%q, want ok=false category=timeout", verdict.OK, verdict.Category)
	}
}

// TestSmokeCmd_KnownEcosystems verifies that smokeCmd returns the correct
// cheap probe for each known ecosystem, given an appropriate repo fixture.
func TestSmokeCmd_KnownEcosystems(t *testing.T) {
	cases := []struct {
		name     string
		file     string
		wantHead string // expected first element of returned cmd
		wantLen  int    // minimum length
	}{
		{"go module", "go.mod", "go", 2},
		{"cargo", "Cargo.toml", "cargo", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			content := []byte("placeholder\n")
			switch tc.file {
			case "go.mod":
				content = []byte("module example.com/x\ngo 1.21\n")
			case "Cargo.toml":
				content = []byte("[package]\nname = \"x\"\nversion = \"0.1.0\"\n")
			}
			if err := writeFileBytes(dir+"/"+tc.file, content); err != nil {
				t.Fatal(err)
			}
			cmd := smokeCmd(dir)
			if len(cmd) < tc.wantLen {
				t.Fatalf("smokeCmd len=%d, want >= %d: %v", len(cmd), tc.wantLen, cmd)
			}
			if cmd[0] != tc.wantHead {
				t.Errorf("smokeCmd[0]=%q, want %q", cmd[0], tc.wantHead)
			}
		})
	}
}

// writeFile writes content to path. Used in tests so they don't depend on os helpers.
func writeFileBytes(path string, content []byte) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = f.Write(content)
	return err
}

// Compile-time check that smokeTimeout is a time.Duration (avoids drift).
var _ time.Duration = smokeTimeout
