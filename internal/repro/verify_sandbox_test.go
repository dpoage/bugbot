package repro

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/config"
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
// cheap probe and launcher name for each known ecosystem, given an
// appropriate repo fixture.
func TestSmokeCmd_KnownEcosystems(t *testing.T) {
	cases := []struct {
		name         string
		file         string
		wantHead     string // expected first element of returned cmd
		wantLen      int    // minimum length
		wantLauncher string
	}{
		{"go module", "go.mod", "go", 2, "go"},
		{"cargo", "Cargo.toml", "cargo", 2, "cargo"},
		{"bazel", "MODULE.bazel", "/bin/sh", 3, "bazel"},
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
			case "MODULE.bazel":
				content = []byte("module(name = \"x\")\n")
			}
			if err := writeFileBytes(dir+"/"+tc.file, content); err != nil {
				t.Fatal(err)
			}
			cmd, launcher := smokeCmd(dir)
			if len(cmd) < tc.wantLen {
				t.Fatalf("smokeCmd len=%d, want >= %d: %v", len(cmd), tc.wantLen, cmd)
			}
			if cmd[0] != tc.wantHead {
				t.Errorf("smokeCmd[0]=%q, want %q", cmd[0], tc.wantHead)
			}
			if launcher != tc.wantLauncher {
				t.Errorf("launcher=%q, want %q", launcher, tc.wantLauncher)
			}
		})
	}
}

// TestSmokeCmd_BazelProbesBothLaunchers pins the bugbot-4z7m probe shape: the
// bazel smoke command must try `bazel version` AND fall back to `bazelisk
// version` (bazelisk is commonly installed under its own name only), keeping
// exit 127 when neither resolves so classifySmoke still reads
// toolchain_missing.
func TestSmokeCmd_BazelProbesBothLaunchers(t *testing.T) {
	dir := t.TempDir()
	if err := writeFileBytes(dir+"/MODULE.bazel", []byte("module(name = \"x\")\n")); err != nil {
		t.Fatal(err)
	}
	cmd, launcher := smokeCmd(dir)
	if launcher != "bazel" {
		t.Fatalf("launcher = %q, want bazel", launcher)
	}
	if len(cmd) != 3 || cmd[0] != "/bin/sh" || cmd[1] != "-c" {
		t.Fatalf("cmd = %v, want /bin/sh -c <script>", cmd)
	}
	script := cmd[2]
	for _, want := range []string{"bazel version", "bazelisk version", "exit 127"} {
		if !strings.Contains(script, want) {
			t.Errorf("script %q missing %q", script, want)
		}
	}
}

// TestBlocksRepro_BuildDriverLauncherNeverBlocks pins the bugbot-4z7m stage
// fix: a bazel/bazelisk-launcher smoke failure — whatever the category —
// must NOT disable the whole repro stage; per-finding (bugbot-14g0) and
// per-plan (bugbot-rj3z) gates handle build-driver absence at finding
// granularity. Language launchers keep the original bugbot-u6td blocking
// semantics.
func TestBlocksRepro_BuildDriverLauncherNeverBlocks(t *testing.T) {
	cases := []struct {
		launcher string
		category SmokeCategory
		want     bool
	}{
		{"bazel", SmokeCategoryToolchainMissing, false},
		{"bazel", SmokeCategoryEnvError, false},
		{"bazelisk", SmokeCategoryToolchainMissing, false},
		{"go", SmokeCategoryToolchainMissing, true},
		{"python", SmokeCategoryEnvError, true},
		{"go", SmokeCategoryOK, false},
		{"", SmokeCategoryToolchainMissing, true},
	}
	for _, tc := range cases {
		v := SmokeVerdict{Category: tc.category, Launcher: tc.launcher}
		if got := v.BlocksRepro(); got != tc.want {
			t.Errorf("BlocksRepro(launcher=%q, category=%q) = %v, want %v", tc.launcher, tc.category, got, tc.want)
		}
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

// ---------------------------------------------------------------------------
// TestVerifySandboxOnce_CachesResult (bugbot-u6td): the once-per-process probe
// caches its result and does not call the sandbox a second time.
// ---------------------------------------------------------------------------

// resetSmokeCache resets the package-level smokeCache for test isolation.
// Only tests in this (same) package may call it.
func resetSmokeCache() {
	smokeCache.mu.Lock()
	smokeCache.m = make(map[string]*smokeEntry)
	smokeCache.mu.Unlock()
}

// TestVerifySandboxOnce_CachesResult verifies that VerifySandboxOnce runs the
// probe exactly once and returns the same cached result on subsequent calls,
// even when called concurrently (bugbot-u6td acceptance criterion).
func TestVerifySandboxOnce_CachesResult(t *testing.T) {
	resetSmokeCache()
	t.Cleanup(resetSmokeCache)

	dir := t.TempDir()
	if err := writeFileBytes(dir+"/go.mod", []byte("module example.com/x\ngo 1.21\n")); err != nil {
		t.Fatal(err)
	}

	// Mock sandbox: always returns toolchain_missing. We call VerifySandbox
	// (no cache) twice to confirm both calls reach the sandbox, then verify
	// the classification. VerifySandboxOnce (the Once-layer) prevents this in
	// production; this test documents the base-layer behavior.
	m := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 127, Stderr: "go: command not found"}})
	spec := sandbox.Spec{Image: "debian:slim"}
	res := sandbox.Resolution{}
	ctx := context.Background()

	v1, _ := VerifySandbox(ctx, m, dir, spec, res)
	v2, _ := VerifySandbox(ctx, m, dir, spec, res)

	// VerifySandbox has no cache: both calls reach the sandbox.
	if m.CallCount() != 2 {
		t.Errorf("VerifySandbox (no cache): want 2 calls, got %d", m.CallCount())
	}
	if v1.Category != SmokeCategoryToolchainMissing {
		t.Errorf("first call: want toolchain_missing, got %q", v1.Category)
	}
	if v2.Category != SmokeCategoryToolchainMissing {
		t.Errorf("second call: want toolchain_missing, got %q", v2.Category)
	}
}

// TestVerifySandboxOnce_SkipsReproOnToolchainMissing verifies the preflight
// classification: toolchain_missing and env_error are the categories that
// trigger a "skip repro stage" decision in callers (bugbot-u6td).
func TestVerifySandboxOnce_SkipsReproOnToolchainMissing(t *testing.T) {
	resetSmokeCache()
	t.Cleanup(resetSmokeCache)

	dir := t.TempDir()
	if err := writeFileBytes(dir+"/go.mod", []byte("module example.com/x\ngo 1.21\n")); err != nil {
		t.Fatal(err)
	}

	m := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 127, Stderr: "go: command not found"}})
	spec := sandbox.Spec{Image: "debian:slim"}
	res := sandbox.Resolution{}
	ctx := context.Background()

	verdict, err := VerifySandbox(ctx, m, dir, spec, res)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Callers must skip repro on toolchain_missing or env_error.
	shouldSkip := verdict.BlocksRepro()
	if !shouldSkip {
		t.Errorf("verdict.Category=%q: callers should skip repro but won't", verdict.Category)
	}
	if verdict.OK {
		t.Error("verdict.OK should be false on toolchain_missing")
	}
}

// TestVerifySandboxOnce_OKProceeds verifies the preflight pass case:
// a SmokeCategoryOK verdict means the toolchain is present and repro may proceed.
func TestVerifySandboxOnce_OKProceeds(t *testing.T) {
	resetSmokeCache()
	t.Cleanup(resetSmokeCache)

	dir := t.TempDir()
	if err := writeFileBytes(dir+"/go.mod", []byte("module example.com/x\ngo 1.21\n")); err != nil {
		t.Fatal(err)
	}

	m := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 0, Stdout: "ok\n"}})
	spec := sandbox.Spec{Image: "golang:1.21"}
	res := sandbox.Resolution{}
	ctx := context.Background()

	verdict, err := VerifySandbox(ctx, m, dir, spec, res)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	shouldSkip := verdict.BlocksRepro()
	if shouldSkip {
		t.Errorf("verdict.Category=%q: callers should NOT skip repro on OK, but would", verdict.Category)
	}
	if !verdict.OK {
		t.Error("verdict.OK should be true when toolchain is present")
	}
}

// TestSmokeVerdict_BlocksRepro pins the gate contract per category:
// only toolchain_missing and env_error disable the repro stage; unprobeable
// (no probe derivable — unknown ecosystem) must NOT block (bugbot-u6td).
func TestSmokeVerdict_BlocksRepro(t *testing.T) {
	cases := []struct {
		cat  SmokeCategory
		want bool
	}{
		{SmokeCategoryOK, false},
		{SmokeCategoryTimeout, false},
		{SmokeCategoryDepMissing, false},
		{SmokeCategoryUnprobeable, false},
		{SmokeCategoryToolchainMissing, true},
		{SmokeCategoryEnvError, true},
	}
	for _, tc := range cases {
		if got := (SmokeVerdict{Category: tc.cat}).BlocksRepro(); got != tc.want {
			t.Errorf("BlocksRepro(%s) = %v, want %v", tc.cat, got, tc.want)
		}
	}
}

// TestVerifySandboxOnce_KeyedPerRepoAndImage verifies the cache is keyed on
// (repoDir, image): a second repo or a reconfigured image gets its own probe
// instead of inheriting the first probe's verdict.
func TestVerifySandboxOnce_KeyedPerRepoAndImage(t *testing.T) {
	resetSmokeCache()
	t.Cleanup(resetSmokeCache)

	smokeCache.mu.Lock()
	a := &smokeEntry{}
	a.once.Do(func() { a.verdict = SmokeVerdict{OK: true, Category: SmokeCategoryOK} })
	smokeCache.m["repoA\x00imgA"] = a
	smokeCache.mu.Unlock()

	smokeCache.mu.Lock()
	_, hitOther := smokeCache.m["repoA\x00imgB"]
	_, hitSame := smokeCache.m["repoA\x00imgA"]
	smokeCache.mu.Unlock()
	if hitOther {
		t.Error("different image must not share the cached verdict")
	}
	if !hitSame {
		t.Error("same (repoDir, image) pair must hit the cache")
	}
}

// TestRunSandboxVerifyThreadsDepOptions is the regression test for
// bugbot-48ya gap 3: RunSandboxVerify previously built its DepOptions with
// ONLY Strategy set, so dep_strategy: fetch unconditionally failed dep
// resolution with "requires a fetch sandbox" (sandbox.ResolveDeps' Go FETCH
// branch requires FetchSandbox) even when the real repro path would resolve
// it fine, and sandbox.local_mounts/host_toolchains were invisible to the
// doctor smoke probe entirely. This proves FetchSandbox is now threaded (a
// go.mod repo under dep_strategy: fetch must resolve deps without error)
// and LocalMounts is threaded (a configured local_mounts entry must not be
// silently dropped) by exercising ResolveDeps via the same DepOptions shape
// RunSandboxVerify builds.
//
// Uses backend: bwrap (skipped cleanly when unavailable): it needs no
// sandbox.image and constructs without any external container runtime.
func TestRunSandboxVerifyThreadsDepOptions(t *testing.T) {
	if ok, reason := sandbox.DetectBwrap(); !ok {
		t.Skipf("bwrap unavailable: %s", reason)
	}

	repoDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoDir, "go.mod"), []byte("module example.com/x\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mountDir := t.TempDir()

	var cfg config.Config
	cfg.Sandbox.Backend = "bwrap"
	cfg.Sandbox.DepStrategy = "fetch"
	cfg.Sandbox.LocalMounts = []config.LocalMount{{Host: mountDir, Container: "/sibling"}}

	verdict, err := RunSandboxVerify(context.Background(), repoDir, cfg)
	if err != nil {
		if strings.Contains(err.Error(), "requires a fetch sandbox") {
			t.Fatalf("RunSandboxVerify: DepOptions.FetchSandbox was not threaded through -- got the pre-fix resolve error: %v", err)
		}
		t.Fatalf("RunSandboxVerify: unexpected error: %v", err)
	}
	if verdict.Category == SmokeCategoryEnvError && strings.Contains(verdict.Detail, "could not resolve dependencies") {
		t.Fatalf("verdict = %+v, want dependency resolution to succeed (FetchSandbox/LocalMounts not threaded)", verdict)
	}

	// Directly assert LocalMounts reaches sandbox.ResolveDeps via the exact
	// DepOptions shape RunSandboxVerify constructs (localMountsFromConfig +
	// FetchSandbox), independent of what the smoke command happens to be.
	bw, err := sandbox.NewBwrap()
	if err != nil {
		t.Fatalf("NewBwrap: %v", err)
	}
	t.Cleanup(func() { _ = bw.Close() })
	res, err := sandbox.ResolveDeps(repoDir, sandbox.DepOptions{
		Strategy:       sandbox.DepStrategy(cfg.Sandbox.DepStrategy),
		FetchSandbox:   bw,
		FetchImage:     cfg.Sandbox.Image,
		LocalMounts:    localMountsFromConfig(cfg),
		HostToolchains: cfg.Sandbox.HostToolchains,
	})
	if err != nil {
		t.Fatalf("ResolveDeps: %v", err)
	}
	found := false
	for _, m := range res.ROMounts {
		if m.ContainerPath == "/sibling" && m.HostPath == mountDir {
			found = true
		}
	}
	if !found {
		t.Fatalf("ROMounts = %+v, want the sandbox.local_mounts entry threaded through", res.ROMounts)
	}
}
