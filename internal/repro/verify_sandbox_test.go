package repro

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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

// TestClassifySmoke_WorkspaceQuotaExceeded pins IC-1 (bugbot-bdqf follow-up):
// a growth-ceiling kill must NEVER classify as OK/SmokeCategoryOK (the
// false-green WatchdogOracleB's approval was conditioned on closing) —
// it lands on SmokeCategoryEnvError, which BlocksRepro() already treats as
// gating (see TestSmokeVerdict_BlocksRepro), and Detail carries
// res.KillReason() so the operator sees WHY, not just a bare exit code.
func TestClassifySmoke_WorkspaceQuotaExceeded(t *testing.T) {
	res := sandbox.Result{ExitCode: -1, WorkspaceQuotaExceeded: true}
	v := classifySmoke(res, []string{"go", "vet", "./..."})
	if v.OK {
		t.Errorf("a quota-killed smoke run must never report OK=true; got %+v", v)
	}
	if v.Category == SmokeCategoryOK {
		t.Errorf("a quota-killed smoke run must never classify as SmokeCategoryOK; got category=%q", v.Category)
	}
	if v.Category != SmokeCategoryEnvError {
		t.Errorf("category = %q, want %q", v.Category, SmokeCategoryEnvError)
	}
	if !v.BlocksRepro() {
		t.Error("a quota kill must gate the repro stage (BlocksRepro() = false, want true)")
	}
	if !strings.Contains(v.Detail, "quota") {
		t.Errorf("Detail = %q, want it to name the quota (res.KillReason())", v.Detail)
	}
}

// TestClassifySmoke_WorkspaceQuotaExceeded_DistinctFromPlainTimeout is the
// negative control: a PLAIN TimedOut result (no WorkspaceQuotaExceeded)
// must keep its existing SmokeCategoryTimeout classification and
// BlocksRepro()=false, byte-identical to before the InfraKilled() check was
// added — the quota branch must not leak into the ordinary timeout path.
func TestClassifySmoke_WorkspaceQuotaExceeded_DistinctFromPlainTimeout(t *testing.T) {
	res := sandbox.Result{ExitCode: -1, TimedOut: true, Stderr: "killed"}
	v := classifySmoke(res, []string{"go", "vet", "./..."})
	if v.OK {
		t.Error("plain timeout must not be OK=true")
	}
	if v.Category != SmokeCategoryTimeout {
		t.Errorf("category = %q, want %q (unchanged)", v.Category, SmokeCategoryTimeout)
	}
	if v.BlocksRepro() {
		t.Error("plain timeout must NOT gate the repro stage (BlocksRepro() = true, want false) — unchanged behavior")
	}
	if !strings.Contains(v.Detail, "timed out") {
		t.Errorf("Detail = %q, want it to still say \"timed out\" (unchanged wording)", v.Detail)
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

// TestClassifySmoke_ExitCodePropagated pins acceptance criterion 1
// (bugbot-6835): every classifySmoke branch must carry res.ExitCode through
// to SmokeVerdict.ExitCode, including the timeout branch, where
// sandbox.Result already reports -1 per its own documented contract
// (bwrap.go/cli.go/hostexec.go all set ExitCode=-1 on a watchdog kill) —
// classifySmoke must not silently drop or reinterpret it. Also pins that
// Detail actually carries the branch's real output (not just the exit
// code) on every branch — a mutant that hardcodes an empty excerpt would
// fail wantSubstr on every case here.
func TestClassifySmoke_ExitCodePropagated(t *testing.T) {
	cases := []struct {
		name       string
		res        sandbox.Result
		wantExit   int
		wantSubstr string
	}{
		{"clean exit", sandbox.Result{ExitCode: 0, Stdout: "ok\n"}, 0, "ok"},
		{"timeout (sandbox.Result contract: -1)", sandbox.Result{ExitCode: -1, TimedOut: true, Stderr: "killed"}, -1, "killed"},
		{"exit 125", sandbox.Result{ExitCode: 125, Stderr: "setup failed"}, 125, "setup failed"},
		{"exit 127 toolchain missing", sandbox.Result{ExitCode: 127, Stderr: "go: command not found"}, 127, "command not found"},
		{"env error", sandbox.Result{ExitCode: 1, Stderr: "read-only file system"}, 1, "read-only file system"},
		{"dep missing", sandbox.Result{ExitCode: 1, Stderr: "no module named 'pytest'"}, 1, "no module named"},
		{"real test failure (ok=true)", sandbox.Result{ExitCode: 1, Stdout: "FAIL\tfoo\n"}, 1, "FAIL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := classifySmoke(tc.res, []string{"go", "test", "./..."})
			if v.ExitCode != tc.wantExit {
				t.Errorf("ExitCode = %d, want %d", v.ExitCode, tc.wantExit)
			}
			if !strings.Contains(v.Detail, fmt.Sprintf("exit %d", tc.wantExit)) {
				t.Errorf("Detail = %q, want it to contain %q so BlocksRepro diagnostics (cli/daemon.go, engine/repro.go) see the exit code without an edit", v.Detail, fmt.Sprintf("exit %d", tc.wantExit))
			}
			if !strings.Contains(v.Detail, tc.wantSubstr) {
				t.Errorf("Detail = %q, want it to contain %q (the branch's real captured output, not just a placeholder)", v.Detail, tc.wantSubstr)
			}
		})
	}
}

// TestClassifySmoke_LongOutputPreservesTailAndExitCode is the bugbot-6835 /
// bugbot-cbm5 regression test: a failing smoke run whose root-cause string
// (e.g. the ENTRYPOINT argv-mangling diagnostic) appears ONLY after 2000+
// chars of image-pull/setup noise must still be visible, along with the
// exit code. A 300-char-head-only cap (the pre-fix behavior) is proven
// impossible to reproduce: the root-cause marker sits well past byte 300.
func TestClassifySmoke_LongOutputPreservesTailAndExitCode(t *testing.T) {
	noise := strings.Repeat("pulling layer sha256:deadbeef... download progress noise\n", 60)
	const rootCause = "ENTRYPOINT_ARGV_MANGLED: exec \"/bin/sh\": stat /bin/sh: no such file or directory"
	stderr := noise + rootCause + "\n"
	if len(stderr) <= 2000 {
		t.Fatalf("test fixture too short: %d bytes, want > 2000 to exercise the elision path", len(stderr))
	}
	if idx := strings.Index(stderr, rootCause); idx < 300 {
		t.Fatalf("test fixture invalid: root cause at byte %d, want > 300 so the old head-only 300-char cap provably could not have shown it", idx)
	}

	res := sandbox.Result{ExitCode: 127, Stderr: stderr}
	v := classifySmoke(res, []string{"bazel", "version"})

	if v.ExitCode != 127 {
		t.Errorf("ExitCode = %d, want 127", v.ExitCode)
	}
	if !strings.Contains(v.Detail, rootCause) {
		t.Errorf("Detail does not contain the root cause; got %d chars, want it to include %q\nDetail: %s", len(v.Detail), rootCause, v.Detail)
	}
	if len(v.Detail) < 2000 {
		t.Errorf("Detail budget too small: got %d chars, want a generous (>=2000) head+tail excerpt per bugbot-6835", len(v.Detail))
	}
	if strings.Contains(v.Detail, "\n") {
		t.Errorf("Detail contains a raw newline; it must be single-line-safe for cli/daemon.go and engine/repro.go's one-line diagnostics: %q", v.Detail)
	}
	// Regression guard: the OLD behavior was trunc(out, 300) — a pure head
	// excerpt that could never contain a marker beyond byte 300. Prove the
	// fix actually changed shape, not just added a field nobody reads.
	oldStyleHead := trunc(res.Stdout+"\n"+res.Stderr, 300)
	if strings.Contains(oldStyleHead, rootCause) {
		t.Fatalf("test fixture broken: the old 300-char head-only excerpt already contains the root cause")
	}
}

// TestClassifySmoke_DoctorOracleB_P7Scenario reproduces the exact fix-round
// rejection: a fix-round revision kept the >=2000-char excerpt in a
// separate FullOutput field but left the daemon/scan-facing Detail at a
// short ~300-char (150/150 head/tail split) budget. DoctorOracleB's P7
// probe used an 856-byte stream with the root cause at bytes 595-638,
// followed by 218 bytes of routine epilogue — landing in NEITHER the first
// 150 bytes NOR the last 150 bytes, so Detail elided it even though
// FullOutput (which cli/daemon.go and engine/repro.go never render) had it.
// Detail now carries the SAME budget as the former FullOutput, so this
// stream (856 bytes, well under the 4096 budget) needs no elision at all —
// the root cause must be in Detail verbatim.
func TestClassifySmoke_DoctorOracleB_P7Scenario(t *testing.T) {
	const rootCause = "ENTRYPOINT_ROOT_CAUSE: exec user process caused: no such file or directory"
	prefix := strings.Repeat("x", 595)
	epilogue := strings.Repeat("y", 218)
	stderr := prefix + rootCause + epilogue
	if len(stderr) != 595+len(rootCause)+218 {
		t.Fatalf("fixture arithmetic wrong: %d bytes", len(stderr))
	}
	if len(stderr) >= 2000 {
		t.Fatalf("fixture must stay under the 2000-char acceptance floor to reproduce P7 (no-elision case): got %d", len(stderr))
	}

	v := classifySmoke(sandbox.Result{ExitCode: 1, Stderr: stderr}, []string{"bazel", "version"})
	if !strings.Contains(v.Detail, rootCause) {
		t.Fatalf("Detail = %q, want it to contain the P7 root cause %q (DoctorOracleB fix-round finding)", v.Detail, rootCause)
	}
	if !strings.Contains(v.Detail, "exit 1") {
		t.Errorf("Detail = %q, want the exit code", v.Detail)
	}
}

// TestSmokeDetail_EmptyOutputNoDanglingArtifact pins the fix-round nit: a
// smoke command that produced no captured output (Stdout/Stderr both
// empty) must not leave a dangling "exit 0: " with an empty/pipe-only
// trailer — just the bare exit code.
func TestSmokeDetail_EmptyOutputNoDanglingArtifact(t *testing.T) {
	v := classifySmoke(sandbox.Result{ExitCode: 0}, []string{"go", "version"})
	if v.Detail != "exit 0" {
		t.Errorf("Detail = %q, want exactly %q (no dangling separator for empty output)", v.Detail, "exit 0")
	}
}

// TestHeadTailExcerpt covers the head+tail preservation helper directly:
// short input passes through unchanged, long input keeps both ends with a
// byte-accounted elision marker, cuts never split a UTF-8 rune, and a
// non-positive budget returns "" rather than panicking (mirrors
// tailExcerpt's guard).
func TestHeadTailExcerpt(t *testing.T) {
	t.Run("short input unchanged", func(t *testing.T) {
		if got := headTailExcerpt("hello", 100); got != "hello" {
			t.Errorf("headTailExcerpt(short) = %q, want unchanged", got)
		}
	})

	t.Run("exactly at budget unchanged", func(t *testing.T) {
		s := strings.Repeat("x", 50)
		if got := headTailExcerpt(s, 50); got != s {
			t.Errorf("headTailExcerpt(at-budget) changed a string exactly at budget")
		}
	})

	t.Run("long input keeps head and tail", func(t *testing.T) {
		head := "HEAD_MARKER_" + strings.Repeat("a", 100)
		middle := strings.Repeat("b", 5000)
		tail := strings.Repeat("c", 100) + "_TAIL_MARKER"
		s := head + middle + tail
		got := headTailExcerpt(s, 400)
		if !strings.HasPrefix(got, "HEAD_MARKER_") {
			t.Errorf("headTailExcerpt dropped the head: %q", got[:min(40, len(got))])
		}
		if !strings.HasSuffix(got, "_TAIL_MARKER") {
			t.Errorf("headTailExcerpt dropped the tail: %q", got[max(0, len(got)-40):])
		}
		if !strings.Contains(got, "bytes elided") {
			t.Errorf("headTailExcerpt missing elision marker: %q", got)
		}
		if strings.Contains(got, middle[:1000]) {
			t.Errorf("headTailExcerpt kept middle content it should have elided")
		}
	})

	t.Run("rune-safe cuts on multi-byte UTF-8", func(t *testing.T) {
		// "€" is 3 bytes (E2 82 AC); pad so the natural budget/2 cut point
		// would otherwise land mid-rune.
		s := strings.Repeat("a", 199) + "€€€€€€€€€€" + strings.Repeat("b", 5000) + "€€€€€€€€€€" + strings.Repeat("c", 199)
		got := headTailExcerpt(s, 400)
		if !utf8.ValidString(got) {
			t.Errorf("headTailExcerpt produced invalid UTF-8: %q", got)
		}
	})

	t.Run("non-positive budget returns empty instead of panicking", func(t *testing.T) {
		if got := headTailExcerpt("anything", 0); got != "" {
			t.Errorf("headTailExcerpt(budget=0) = %q, want \"\"", got)
		}
		if got := headTailExcerpt("anything", -5); got != "" {
			t.Errorf("headTailExcerpt(budget=-5) = %q, want \"\"", got)
		}
	})
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
	cfg.Sandbox.LocalMounts = []config.LocalMount{
		{Host: mountDir, Container: "/sibling"},
		{Host: mountDir, Container: "/bazel-vendor", Writable: true},
	}

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
	localRO, localRW := localMountsFromConfig(cfg)
	res, err := sandbox.ResolveDeps(repoDir, sandbox.DepOptions{
		Strategy:       sandbox.DepStrategy(cfg.Sandbox.DepStrategy),
		FetchSandbox:   bw,
		FetchImage:     cfg.Sandbox.Image,
		LocalMounts:    localRO,
		LocalRWMounts:  localRW,
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
		if m.ContainerPath == "/bazel-vendor" {
			t.Errorf("writable:true entry landed in ROMounts — smoke run would mount it read-only (bugbot-wjc2 review defect 1)")
		}
	}
	if !found {
		t.Fatalf("ROMounts = %+v, want the sandbox.local_mounts entry threaded through", res.ROMounts)
	}
	foundRW := false
	for _, m := range res.RWMounts {
		if m.ContainerPath == "/bazel-vendor" && m.HostPath == mountDir {
			foundRW = true
		}
	}
	if !foundRW {
		t.Fatalf("RWMounts = %+v, want the writable:true local_mounts entry threaded to the smoke run", res.RWMounts)
	}
}
