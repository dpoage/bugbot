package repro

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/sandbox"
	"github.com/dpoage/bugbot/internal/store"
)

// patchPlanBody serializes a PatchPlan to JSON as the agent would emit it.
func patchPlanBody(t *testing.T, p PatchPlan) string {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal patch plan: %v", err)
	}
	return string(b)
}

// goodPatchPlan returns a representative valid patch plan that edits the
// existing calc.go file (must exist in the repo dir used by the test).
func goodPatchPlan() PatchPlan {
	return PatchPlan{
		Files:   map[string]string{"calc.go": "package bug\n\nfunc Divide(a, b int) (int, bool) {\n\tif b == 0 {\n\t\treturn 0, false\n\t}\n\treturn a / b, true\n}\n"},
		Summary: "Add zero-divisor check to Divide",
	}
}

// newRepoDirWithCalc creates a minimal repo with calc.go so the existing-file
// guard in validatePatchPlan can find the target file.
func newRepoDirWithCalc(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module bug\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "calc.go"), []byte("package bug\n\nfunc Divide(a, b int) (int, bool) { return a/b, true }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// buildT1Finding seeds a Tier-1 (repro promoted) finding and a matching Attempt.
func buildT1Finding(t *testing.T, st *store.Store) (store.Finding, *Attempt) {
	t.Helper()
	fp := store.Fingerprint("logic", "calc.go", 5, "Divide ignores zero divisor")
	f := store.Finding{
		Fingerprint: fp,
		Title:       "Divide ignores zero divisor",
		Description: "Divide returns ok=true for a zero divisor.",
		Reasoning:   "Verified: no zero check before the division.",
		Severity:    "high",
		Tier:        1, // already repro-promoted
		Status:      store.StatusOpen,
		Lens:        "logic",
		File:        "calc.go",
		Line:        5,
		CommitSHA:   "abc123",
		FileHash:    "deadbeef",
	}
	stored, err := st.UpsertFinding(context.Background(), f)
	if err != nil {
		t.Fatalf("seed T1 finding: %v", err)
	}
	att := &Attempt{
		FindingID: stored.ID,
		Promoted:  true,
		Plan: &Plan{
			Files:  map[string]string{"bug_test.go": "package bug\nimport \"testing\"\nfunc TestBug(t *testing.T){ t.Fatal(\"boom\") }\n"},
			Cmd:    []string{"go", "test", "-run", "TestBug", "./..."},
			Expect: "TestBug fails",
		},
		Output: "--- FAIL: TestBug\nFAIL",
	}
	return stored, att
}

// --- success path -----------------------------------------------------------

// TestPatchProver_SuccessPath verifies the happy path:
//   - Targeted run exits 0 (fix makes repro pass)
//   - Suite run exits 0 (full suite stays green)
//   - Finding promoted to T0 with diff persisted
//   - Artifact patch.diff written to the bundle dir
func TestPatchProver_SuccessPath(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	repoDir := newRepoDirWithCalc(t)
	artifactDir := t.TempDir()

	finding, att := buildT1Finding(t, st)

	// Ensure the bundle directory exists (mirroring what writeArtifacts does).
	bundleDir := filepath.Join(artifactDir, finding.ID)
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		t.Fatal(err)
	}

	client := newScriptedClient(patchPlanBody(t, goodPatchPlan()))

	// Two sandbox calls: targeted (exit 0) then suite (exit 0).
	sb := sandbox.NewMock(sandbox.MockResponse{})
	sb.EnqueueResponse(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 0, Stdout: "ok\tbug\t0.01s"}})
	sb.EnqueueResponse(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 0, Stdout: "ok\tbug\t0.01s"}})

	prover := &PatchProver{
		client:      client,
		sb:          sb,
		repoDir:     repoDir,
		maxAttempts: 3,
		artifactDir: artifactDir,
	}

	outcome, err := prover.Prove(ctx, st, finding, att)
	if err != nil {
		t.Fatalf("Prove: %v", err)
	}
	if !outcome.FixWitnessed {
		t.Errorf("outcome.FixWitnessed = false, want true")
	}
	if outcome.NeedsHuman {
		t.Errorf("outcome.NeedsHuman = true, want false")
	}

	// Exactly 2 sandbox calls: targeted + suite.
	if sb.CallCount() != 2 {
		t.Errorf("sandbox calls = %d, want 2", sb.CallCount())
	}
	calls := sb.Calls()
	// First call: the repro command.
	if len(calls[0].Spec.Cmd) > 0 && calls[0].Spec.Cmd[0] != "go" {
		t.Errorf("targeted cmd[0] = %q, want go", calls[0].Spec.Cmd[0])
	}
	// Second call: go test ./...
	if len(calls[1].Spec.Cmd) < 3 || calls[1].Spec.Cmd[2] != "./..." {
		t.Errorf("suite cmd = %v, want [go test ./...]", calls[1].Spec.Cmd)
	}
	// Both calls must inject repro test files AND patch files.
	if _, ok := calls[0].Spec.WriteFiles["bug_test.go"]; !ok {
		t.Errorf("targeted run missing repro test file")
	}
	if _, ok := calls[0].Spec.WriteFiles["calc.go"]; !ok {
		t.Errorf("targeted run missing patched calc.go")
	}

	// Finding must be promoted to T0 with FixPatch set.
	got, err := st.GetFindingByFingerprint(ctx, finding.Fingerprint)
	if err != nil {
		t.Fatalf("GetFindingByFingerprint: %v", err)
	}
	if got.Tier != 0 {
		t.Errorf("tier = %d, want 0", got.Tier)
	}
	// FixPatch may be empty if git is not available in the test environment;
	// we do not require the diff to be non-empty (the git binary may be absent
	// in CI).  We only assert the finding was promoted to T0.

	// Artifact patch/ directory must exist.
	patchDir := filepath.Join(bundleDir, "patch")
	if _, err := os.Stat(patchDir); err != nil {
		t.Errorf("patch/ artifact dir missing: %v", err)
	}
}

// TestPatchProver_CarriesSetupCmds verifies that PatchProver.setupCmds are
// threaded into every sandbox Spec via execSandbox.
func TestPatchProver_CarriesSetupCmds(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	repoDir := newRepoDirWithCalc(t)
	artifactDir := t.TempDir()

	finding, att := buildT1Finding(t, st)
	if err := os.MkdirAll(filepath.Join(artifactDir, finding.ID), 0o755); err != nil {
		t.Fatal(err)
	}

	client := newScriptedClient(patchPlanBody(t, goodPatchPlan()))
	sb := sandbox.NewMock(sandbox.MockResponse{})
	sb.EnqueueResponse(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 0}})
	sb.EnqueueResponse(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 0}})

	setupCmds := [][]string{{"npm", "ci", "--offline"}}

	prover := &PatchProver{
		client:      client,
		sb:          sb,
		repoDir:     repoDir,
		maxAttempts: 3,
		artifactDir: artifactDir,
		setupCmds:   setupCmds,
	}

	if _, err := prover.Prove(ctx, st, finding, att); err != nil {
		t.Fatalf("Prove: %v", err)
	}

	calls := sb.Calls()
	if len(calls) != 2 {
		t.Fatalf("sandbox calls = %d, want 2", len(calls))
	}
	for i, c := range calls {
		if len(c.Spec.SetupCmds) != 1 || c.Spec.SetupCmds[0][0] != "npm" {
			t.Errorf("call %d SetupCmds = %v, want [[npm ci --offline]]", i, c.Spec.SetupCmds)
		}
	}
}

// TestPatchProver_CarriesDepMountsAndEnv verifies the patch-prover's targeted
// and suite sandbox runs both carry the resolved dependency mounts/env.
func TestPatchProver_CarriesDepMountsAndEnv(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	repoDir := newRepoDirWithCalc(t)
	artifactDir := t.TempDir()

	finding, att := buildT1Finding(t, st)
	if err := os.MkdirAll(filepath.Join(artifactDir, finding.ID), 0o755); err != nil {
		t.Fatal(err)
	}

	client := newScriptedClient(patchPlanBody(t, goodPatchPlan()))
	sb := sandbox.NewMock(sandbox.MockResponse{})
	sb.EnqueueResponse(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 0}})
	sb.EnqueueResponse(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 0}})

	mounts := []sandbox.ROMount{{HostPath: "/host/cache", ContainerPath: "/modcache"}}
	env := []string{"GOMODCACHE=/modcache", "GOPROXY=off"}

	prover := &PatchProver{
		client:      client,
		sb:          sb,
		repoDir:     repoDir,
		maxAttempts: 3,
		artifactDir: artifactDir,
		depMounts:   mounts,
		depEnv:      env,
	}

	if _, err := prover.Prove(ctx, st, finding, att); err != nil {
		t.Fatalf("Prove: %v", err)
	}

	calls := sb.Calls()
	if len(calls) != 2 {
		t.Fatalf("sandbox calls = %d, want 2", len(calls))
	}
	for i, c := range calls {
		if len(c.Spec.ROMounts) != 1 || c.Spec.ROMounts[0].ContainerPath != "/modcache" {
			t.Errorf("call %d ROMounts = %+v, want one /modcache mount", i, c.Spec.ROMounts)
		}
		if !contains(c.Spec.Env, "GOPROXY=off") {
			t.Errorf("call %d env = %v, want GOPROXY=off", i, c.Spec.Env)
		}
	}
}

// --- targeted-pass but suite-fail -> feedback -> exhaustion -> NeedsHuman ---

func TestPatchProver_SuiteFailLeadsToNeedsHuman(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	repoDir := newRepoDirWithCalc(t)
	artifactDir := t.TempDir()

	finding, att := buildT1Finding(t, st)

	bundleDir := filepath.Join(artifactDir, finding.ID)
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Two attempts: each time targeted passes but suite fails.
	client := newScriptedClient(
		patchPlanBody(t, goodPatchPlan()),
		patchPlanBody(t, goodPatchPlan()),
	)

	sb := sandbox.NewMock(sandbox.MockResponse{})
	// attempt 1: targeted pass, suite fail
	sb.EnqueueResponse(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 0, Stdout: "ok"}})
	sb.EnqueueResponse(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 1, Stdout: "FAIL: TestOther"}})
	// attempt 2: targeted pass, suite fail again
	sb.EnqueueResponse(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 0, Stdout: "ok"}})
	sb.EnqueueResponse(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 1, Stdout: "FAIL: TestOther"}})

	prover := &PatchProver{
		client:      client,
		sb:          sb,
		repoDir:     repoDir,
		maxAttempts: 2,
		artifactDir: artifactDir,
	}

	outcome, err := prover.Prove(ctx, st, finding, att)
	if err != nil {
		t.Fatalf("Prove: %v", err)
	}
	if outcome.FixWitnessed {
		t.Errorf("FixWitnessed = true, want false")
	}
	if !outcome.NeedsHuman {
		t.Errorf("NeedsHuman = false, want true")
	}

	// Exactly 4 sandbox calls (2 per attempt).
	if sb.CallCount() != 4 {
		t.Errorf("sandbox calls = %d, want 4", sb.CallCount())
	}

	// Tier stays 1.
	got, err := st.GetFindingByFingerprint(ctx, finding.Fingerprint)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Tier != 1 {
		t.Errorf("tier = %d, want 1", got.Tier)
	}
	if !got.NeedsHuman {
		t.Errorf("needs_human = false in store, want true")
	}
	if !strings.Contains(got.Reasoning, "PATCH-PROVER") {
		t.Errorf("reasoning missing PATCH-PROVER marker:\n%s", got.Reasoning)
	}

	// Second agent request must carry "suite fails" feedback.
	second := client.taskText(1)
	if !strings.Contains(second, "suite") {
		t.Errorf("revision task missing suite feedback: %q", second)
	}
}

// --- test-file-touching plan rejected then revised --------------------------

func TestPatchProver_TestFileRejected(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	repoDir := newRepoDirWithCalc(t)
	artifactDir := t.TempDir()

	finding, att := buildT1Finding(t, st)
	bundleDir := filepath.Join(artifactDir, finding.ID)
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// First plan touches a test file (rejected); second plan is valid.
	badPlan := PatchPlan{
		Files:   map[string]string{"calc_test.go": "package bug\n"},
		Summary: "Modifies test file (invalid)",
	}
	client := newScriptedClient(
		patchPlanBody(t, badPlan),
		patchPlanBody(t, goodPatchPlan()),
	)

	// Two sandbox passes for the valid second plan.
	sb := sandbox.NewMock(sandbox.MockResponse{})
	sb.EnqueueResponse(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 0, Stdout: "ok"}})
	sb.EnqueueResponse(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 0, Stdout: "ok"}})

	prover := &PatchProver{
		client:      client,
		sb:          sb,
		repoDir:     repoDir,
		maxAttempts: 3,
		artifactDir: artifactDir,
	}

	outcome, err := prover.Prove(ctx, st, finding, att)
	if err != nil {
		t.Fatalf("Prove: %v", err)
	}
	if !outcome.FixWitnessed {
		t.Errorf("FixWitnessed = false; valid plan after rejection should succeed")
	}

	// Sandbox should NOT have been called for the bad plan (validation rejects
	// before execution).
	if sb.CallCount() != 2 {
		t.Errorf("sandbox calls = %d, want 2 (for valid second plan only)", sb.CallCount())
	}

	// The second agent request must carry feedback about the test-file rejection.
	second := client.taskText(1)
	if !strings.Contains(second, "rejected") && !strings.Contains(second, "test file") {
		t.Errorf("revision task missing test-file rejection feedback: %q", second)
	}
}

// --- nonexistent-path plan rejected -----------------------------------------

func TestPatchProver_NonexistentPathRejected(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	repoDir := newRepoDirWithCalc(t)
	artifactDir := t.TempDir()

	finding, att := buildT1Finding(t, st)
	bundleDir := filepath.Join(artifactDir, finding.ID)
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Plan references a new file that does not exist in the repo.
	badPlan := PatchPlan{
		Files:   map[string]string{"newfile.go": "package bug\n"},
		Summary: "Adds a new file (invalid — must only edit existing files)",
	}
	client := newScriptedClient(
		patchPlanBody(t, badPlan),
		patchPlanBody(t, goodPatchPlan()),
	)

	sb := sandbox.NewMock(sandbox.MockResponse{})
	sb.EnqueueResponse(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 0}})
	sb.EnqueueResponse(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 0}})

	prover := &PatchProver{
		client:      client,
		sb:          sb,
		repoDir:     repoDir,
		maxAttempts: 3,
		artifactDir: artifactDir,
	}

	outcome, err := prover.Prove(ctx, st, finding, att)
	if err != nil {
		t.Fatalf("Prove: %v", err)
	}
	if !outcome.FixWitnessed {
		t.Errorf("FixWitnessed = false; valid plan after rejection should succeed")
	}
	// No sandbox call for the rejected plan.
	if sb.CallCount() != 2 {
		t.Errorf("sandbox calls = %d, want 2 (valid plan only)", sb.CallCount())
	}
	// Feedback must mention the path or out-of-scope.
	second := client.taskText(1)
	if !strings.Contains(second, "rejected") {
		t.Errorf("revision task missing rejection feedback: %q", second)
	}
}

// --- environment failure stops without NeedsHuman ---------------------------

func TestPatchProver_EnvironmentFailureStops(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	repoDir := newRepoDirWithCalc(t)
	artifactDir := t.TempDir()

	finding, att := buildT1Finding(t, st)
	bundleDir := filepath.Join(artifactDir, finding.ID)
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		t.Fatal(err)
	}

	client := newScriptedClient(patchPlanBody(t, goodPatchPlan()))

	// Targeted run fails with an environment error (exit 125).
	sb := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 125, Stderr: "podman: no such runtime"}})

	prover := &PatchProver{
		client:      client,
		sb:          sb,
		repoDir:     repoDir,
		maxAttempts: 3,
		artifactDir: artifactDir,
	}

	_, err := prover.Prove(ctx, st, finding, att)
	if err == nil {
		t.Fatal("expected error for environment failure, got nil")
	}
	// bugbot-vig: the prover now distinguishes "environment cannot run
	// repro" (env failure — sandbox refused to run the test) from
	// "fix rejected" (the test ran and still failed). The
	// distinctive substring here proves we are in the env-failure
	// branch, not a fix-rejection that happens to be a non-zero
	// exit.
	if !strings.Contains(err.Error(), "environment cannot run repro") {
		t.Errorf("error = %q, want distinctive 'environment cannot run repro' message", err)
	}

	// Finding must NOT be flagged NeedsHuman (env failure, not a fix-refusal).
	got, ferr := st.GetFindingByFingerprint(ctx, finding.Fingerprint)
	if ferr != nil {
		t.Fatalf("get: %v", ferr)
	}
	if got.NeedsHuman {
		t.Errorf("needs_human = true after env failure; want false (env failure is not a fix-refusal)")
	}
}

// --- patchVerdict inversion tests -------------------------------------------

func TestPatchVerdict_ExitZeroIsPass(t *testing.T) {
	v := patchVerdict(sandbox.Result{ExitCode: 0, Stdout: "ok"}, []string{"go", "test", "./..."})
	if !v.passed {
		t.Errorf("exit 0 should be a PASS in patch context; got passed=%v", v.passed)
	}
	if v.envFailure {
		t.Errorf("exit 0 should not be an env failure")
	}
}

func TestPatchVerdict_NonZeroIsFail(t *testing.T) {
	// The repro test that previously demonstrated the bug is
	// re-run with the patch applied. If the test still fails
	// (non-zero exit, no env / toolchain / build markers), the
	// fix is rejected — passed=false, envFailure=false. The
	// "FAIL" stdout is a degenerate case that the legacy test
	// relied on; under the new classification it falls through
	// to the default "non-zero, no env markers" branch, which
	// is fixRejected (mutually exclusive with envFailure).
	v := patchVerdict(sandbox.Result{ExitCode: 1, Stdout: "FAIL"}, []string{"go", "test", "./..."})
	if v.passed {
		t.Errorf("exit 1 should be a FAIL in patch context")
	}
	if v.envFailure {
		t.Errorf("exit 1 test failure should not be env failure")
	}
	if !v.fixRejected {
		t.Errorf("exit 1 test failure should be classified as fixRejected; got %+v", v)
	}
}

func TestPatchVerdict_EnvironmentErrors(t *testing.T) {
	cases := []struct {
		name string
		res  sandbox.Result
	}{
		{"timeout", sandbox.Result{ExitCode: -1, TimedOut: true}},
		{"exit 125", sandbox.Result{ExitCode: 125}},
		{"exit 126", sandbox.Result{ExitCode: 126}},
		{"exit 127", sandbox.Result{ExitCode: 127}},
		{"build cache", sandbox.Result{ExitCode: 1, Stderr: "failed to initialize build cache"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := patchVerdict(tc.res, []string{"go", "test", "./..."})
			if !v.envFailure {
				t.Errorf("patchVerdict(%+v).envFailure = false, want true", tc.res)
			}
			if v.passed {
				t.Errorf("patchVerdict(%+v).passed = true, want false", tc.res)
			}
		})
	}
}

// TestPatchVerdict_EnvFailureTranscript_NotFixRejected is the
// acceptance-criterion-3 regression test: a transcript that is
// unambiguously an environment failure (cgo refusal — the same Go
// toolchain marker as the repro-stage regression test) must be
// reported as envFailure=true, NOT as fixRejected. The two
// categories must remain mutually exclusive so the prover can
// distinguish "the sandbox could not run the repro" from "the fix
// is wrong".
func TestPatchVerdict_EnvFailureTranscript_NotFixRejected(t *testing.T) {
	cases := []struct {
		name string
		res  sandbox.Result
		cmd  []string
	}{
		{
			"cgo refusal (Go toolchain marker)",
			sandbox.Result{ExitCode: 2, Stderr: "go: -race requires cgo; enable cgo by setting CGO_ENABLED=1"},
			[]string{"go", "test", "-race", "./..."},
		},
		{
			"read-only filesystem (cross-ecosystem env marker)",
			sandbox.Result{ExitCode: 1, Stderr: "mkdir /data: Read-only file system"},
			[]string{"go", "test", "./..."},
		},
		{
			"pytest collection error (Python toolchain marker)",
			sandbox.Result{ExitCode: 2, Stderr: "ImportError: cannot import name 'foo'"},
			[]string{"pytest", "tests/"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := patchVerdict(tc.res, tc.cmd)
			if !v.envFailure {
				t.Errorf("env-failure transcript must set envFailure=true; got %+v", v)
			}
			if v.fixRejected {
				t.Errorf("env-failure transcript must NOT set fixRejected; got %+v", v)
			}
			if v.passed {
				t.Errorf("env-failure transcript must not pass; got %+v", v)
			}
		})
	}
}

// TestPatchVerdict_FixRejectedTranscript asserts the complementary case:
// a non-zero exit with no env / toolchain / build markers IS classified
// as fixRejected. This is the default that keeps the legacy
// "non-zero means the test still failed" semantics for the case where
// the run issued a non-zero exit and we have no positive evidence of
// env failure.
func TestPatchVerdict_FixRejectedTranscript(t *testing.T) {
	v := patchVerdict(sandbox.Result{ExitCode: 1, Stdout: "--- FAIL: TestDivide (0.00s)\nFAIL"}, []string{"go", "test", "./..."})
	if v.envFailure {
		t.Errorf("fix-rejected transcript must not set envFailure; got %+v", v)
	}
	if !v.fixRejected {
		t.Errorf("fix-rejected transcript must set fixRejected; got %+v", v)
	}
	if v.passed {
		t.Errorf("fix-rejected transcript must not pass; got %+v", v)
	}
}

// --- validatePatchPlan tests ------------------------------------------------

func TestValidatePatchPlan(t *testing.T) {
	repoDir := newRepoDirWithCalc(t)
	prover := &PatchProver{repoDir: repoDir}

	reproPlan := &Plan{
		Files: map[string]string{"bug_test.go": "package bug\n"},
		Cmd:   []string{"go", "test", "./..."},
	}

	t.Run("valid", func(t *testing.T) {
		plan := &PatchPlan{Files: map[string]string{"calc.go": "new content"}}
		if err := prover.validatePatchPlan(plan, reproPlan); err != nil {
			t.Errorf("valid plan rejected: %v", err)
		}
	})

	t.Run("empty_files", func(t *testing.T) {
		plan := &PatchPlan{Files: map[string]string{}}
		if err := prover.validatePatchPlan(plan, reproPlan); err == nil {
			t.Error("empty files should be rejected")
		}
	})

	t.Run("test_file", func(t *testing.T) {
		plan := &PatchPlan{Files: map[string]string{"foo_test.go": "package bug\n"}}
		if err := prover.validatePatchPlan(plan, reproPlan); err == nil {
			t.Error("test file should be rejected")
		}
	})

	t.Run("repro_collision", func(t *testing.T) {
		// bug_test.go is in the repro plan's files.
		plan := &PatchPlan{Files: map[string]string{"bug_test.go": "package bug\n"}}
		if err := prover.validatePatchPlan(plan, reproPlan); err == nil {
			t.Error("repro file collision should be rejected")
		}
	})

	t.Run("path_escape", func(t *testing.T) {
		plan := &PatchPlan{Files: map[string]string{"../outside.go": "package x\n"}}
		if err := prover.validatePatchPlan(plan, reproPlan); err == nil {
			t.Error("path escape should be rejected")
		}
	})

	t.Run("nonexistent_file", func(t *testing.T) {
		plan := &PatchPlan{Files: map[string]string{"does_not_exist.go": "package bug\n"}}
		if err := prover.validatePatchPlan(plan, reproPlan); err == nil {
			t.Error("nonexistent file should be rejected")
		}
	})
}

// --- PromoteAll with PatchProver wired --------------------------------------

func TestPromoteAll_WithPatchProver_FixWitnessed(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	repoDir := newRepoDirWithCalc(t)
	artifactDir := t.TempDir()

	// Seed a Tier-2 finding.
	fp := store.Fingerprint("logic", "calc.go", 5, "Divide ignores zero divisor")
	finding, err := st.UpsertFinding(ctx, store.Finding{
		Fingerprint: fp,
		Title:       "Divide ignores zero divisor",
		Description: "Divide returns ok=true for a zero divisor.",
		Reasoning:   "Verified: no zero check before the division.",
		Severity:    "high",
		Tier:        2,
		Status:      store.StatusOpen,
		Lens:        "logic",
		File:        "calc.go",
		Line:        5,
		CommitSHA:   "abc123",
		FileHash:    "deadbeef",
	})
	if err != nil {
		t.Fatalf("seed finding: %v", err)
	}

	// Four scripted responses: repro plan + patch plan.
	reproP := goodPlan()
	reproP.Files = map[string]string{"bug_test.go": "package bug\nimport \"testing\"\nfunc TestBug(t *testing.T){ t.Fatal(\"boom\") }\n"}
	patchP := goodPatchPlan()
	client := newScriptedClient(
		planBody(t, reproP),      // repro plan
		patchPlanBody(t, patchP), // patch plan
	)

	// ResponseFunc to route sandbox calls:
	//   call 0: repro (exit 1 — bug demonstrated)
	//   call 1: targeted patch (exit 0 — fix works)
	//   call 2: suite (exit 0 — suite stays green)
	sb := sandbox.NewMock(sandbox.MockResponse{})
	sb.ResponseFunc = func(n int, spec sandbox.Spec) (sandbox.Result, error) {
		switch n {
		case 0:
			return sandbox.Result{ExitCode: 1, Stdout: "--- FAIL: TestBug\nFAIL"}, nil
		case 1:
			return sandbox.Result{ExitCode: 0, Stdout: "ok\tbug\t0.01s"}, nil
		case 2:
			return sandbox.Result{ExitCode: 0, Stdout: "ok\tbug\t0.01s"}, nil
		default:
			return sandbox.Result{ExitCode: 0}, nil
		}
	}

	r, err := New(client, sb, repoDir, Options{
		ArtifactDir:      artifactDir,
		PatchProver:      true,
		PatchMaxAttempts: 3,
	})
	if err != nil {
		t.Fatal(err)
	}

	summary, err := r.PromoteAll(ctx, st, []store.Finding{finding})
	if err != nil {
		t.Fatalf("PromoteAll: %v", err)
	}
	if summary.Promoted != 1 {
		t.Errorf("Promoted = %d, want 1", summary.Promoted)
	}
	if summary.FixWitnessed != 1 {
		t.Errorf("FixWitnessed = %d, want 1", summary.FixWitnessed)
	}
	if summary.NeedsHuman != 0 {
		t.Errorf("NeedsHuman = %d, want 0", summary.NeedsHuman)
	}

	got, err := st.GetFindingByFingerprint(ctx, finding.Fingerprint)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Tier != 0 {
		t.Errorf("tier = %d, want 0", got.Tier)
	}
}

// --- diff generation test ---------------------------------------------------

func TestComputeDiff(t *testing.T) {
	// Requires git on PATH; skip gracefully if absent.
	if _, err := lookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	repoDir := t.TempDir()
	orig := "package bug\n\nfunc Divide(a, b int) (int, bool) { return a/b, true }\n"
	if err := os.WriteFile(filepath.Join(repoDir, "calc.go"), []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}

	patched := "package bug\n\nfunc Divide(a, b int) (int, bool) {\n\tif b == 0 {\n\t\treturn 0, false\n\t}\n\treturn a / b, true\n}\n"

	diff, err := computeDiff(repoDir, map[string]string{"calc.go": patched})
	if err != nil {
		t.Fatalf("computeDiff: %v", err)
	}
	// The diff must reference calc.go and include the change.
	if !strings.Contains(diff, "calc.go") {
		t.Errorf("diff missing calc.go reference:\n%s", diff)
	}
	if !strings.Contains(diff, "+") {
		t.Errorf("diff missing + lines:\n%s", diff)
	}
}

func TestComputeDiff_Truncation(t *testing.T) {
	if _, err := lookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	repoDir := t.TempDir()
	// Write a large file so the diff exceeds patchMaxDiffBytes.
	var sb strings.Builder
	sb.WriteString("package bug\n\n")
	for i := 0; i < 5000; i++ {
		sb.WriteString("// line\n")
	}
	orig := sb.String()
	if err := os.WriteFile(filepath.Join(repoDir, "large.go"), []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}

	patched := orig + "\nfunc Extra() {}\n"
	diff, err := computeDiff(repoDir, map[string]string{"large.go": patched})
	if err != nil {
		t.Fatalf("computeDiff: %v", err)
	}
	if len(diff) > patchMaxDiffBytes+100 {
		t.Errorf("diff not truncated: len = %d (limit %d)", len(diff), patchMaxDiffBytes)
	}
	if len(diff) >= patchMaxDiffBytes && !strings.Contains(diff, "truncated") {
		t.Errorf("truncated diff missing marker")
	}
}

// TestComputeDiff_CleanHeaders asserts that computeDiff rewrites the
// `git diff --no-index` header lines so the temp staging dir and the
// repo-absolute prefix never appear in the stored patch. The fix-witness
// text is shown verbatim in reports and issue bodies, so a stray
// `bugbot-patch-diff-XXX` path would leak the sandbox layout to every
// downstream consumer.
func TestComputeDiff_CleanHeaders(t *testing.T) {
	if _, err := lookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	repoDir := t.TempDir()
	orig := "package bug\n\nfunc Divide(a, b int) (int, bool) { return a/b, true }\n"
	if err := os.WriteFile(filepath.Join(repoDir, "calc.go"), []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	patched := "package bug\n\nfunc Divide(a, b int) (int, bool) {\n\tif b == 0 {\n\t\treturn 0, false\n\t}\n\treturn a / b, true\n}\n"

	diff, err := computeDiff(repoDir, map[string]string{"calc.go": patched})
	if err != nil {
		t.Fatalf("computeDiff: %v", err)
	}

	// Headers must use the clean repo-relative form.
	if !strings.Contains(diff, "a/calc.go") {
		t.Errorf("diff missing `a/calc.go` header:\n%s", diff)
	}
	if !strings.Contains(diff, "b/calc.go") {
		t.Errorf("diff missing `b/calc.go` header:\n%s", diff)
	}

	// No temp staging components should leak into the stored patch.
	if strings.Contains(diff, "bugbot-patch-diff-") {
		t.Errorf("diff leaks bugbot-patch-diff-* temp dir:\n%s", diff)
	}
	if strings.Contains(diff, "/tmp/") {
		t.Errorf("diff leaks /tmp/ path component:\n%s", diff)
	}

	// Sanity: the hunk body must still be present and unaltered.
	if !strings.Contains(diff, "+") {
		t.Errorf("diff missing + hunk lines:\n%s", diff)
	}
}

// lookPath is a thin wrapper around exec.LookPath to keep the import within
// this test file.
func lookPath(name string) (string, error) {
	return exec.LookPath(name)
}

// TestDetectSuiteCmd pins the marker-file detection table and the nil result
// for unknown toolchains.
func TestDetectSuiteCmd(t *testing.T) {
	cases := []struct {
		marker string
		want   string
	}{
		{"go.mod", "go test ./..."},
		{"Cargo.toml", "cargo test"},
		{"package.json", "npm test"},
		{"pyproject.toml", "python -m pytest"},
		{"setup.py", "python -m pytest"},
	}
	for _, tc := range cases {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, tc.marker), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		got := detectSuiteCmd(dir)
		if strings.Join(got, " ") != tc.want {
			t.Errorf("marker %s: detected %v, want %q", tc.marker, got, tc.want)
		}
	}
	if got := detectSuiteCmd(t.TempDir()); got != nil {
		t.Errorf("empty repo: detected %v, want nil", got)
	}
}

// TestDetectSuiteCmdExtended covers the new build-system cases: Bazel,
// go.work (with and without a root go.mod), and pnpm-workspace / turbo / nx.
// The original marker table cases are unchanged and still tested above.
func TestDetectSuiteCmdExtended(t *testing.T) {
	cases := []struct {
		name    string
		markers []string // files to create
		want    string   // "" means want nil
	}{
		// Bazel workspace → bazel test //...
		{
			name:    "MODULE.bazel",
			markers: []string{"MODULE.bazel"},
			want:    "bazel test //...",
		},
		{
			name:    "WORKSPACE",
			markers: []string{"WORKSPACE"},
			want:    "bazel test //...",
		},
		{
			name:    "WORKSPACE.bazel",
			markers: []string{"WORKSPACE.bazel"},
			want:    "bazel test //...",
		},
		// Bazel beats go.mod in priority.
		{
			name:    "Bazel+go.mod",
			markers: []string{"MODULE.bazel", "go.mod"},
			want:    "bazel test //...",
		},
		// go.work WITH root go.mod → go test ./...
		{
			name:    "go.work+go.mod",
			markers: []string{"go.work", "go.mod"},
			want:    "go test ./...",
		},
		// go.work WITHOUT root go.mod → nil (multi-module, out of scope).
		{
			name:    "go.work-only",
			markers: []string{"go.work"},
			want:    "",
		},
		// pnpm workspace → pnpm test.
		{
			name:    "pnpm-workspace.yaml",
			markers: []string{"pnpm-workspace.yaml"},
			want:    "pnpm test",
		},
		// turbo.json → npm test (closest portable default).
		{
			name:    "turbo.json",
			markers: []string{"turbo.json"},
			want:    "npm test",
		},
		// nx.json → npm test.
		{
			name:    "nx.json",
			markers: []string{"nx.json"},
			want:    "npm test",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, m := range tc.markers {
				if err := os.WriteFile(filepath.Join(dir, m), []byte("x"), 0o644); err != nil {
					t.Fatalf("write %s: %v", m, err)
				}
			}
			got := detectSuiteCmd(dir)
			gotStr := strings.Join(got, " ")
			if gotStr != tc.want {
				t.Errorf("markers %v: got %q, want %q", tc.markers, gotStr, tc.want)
			}
		})
	}
}

// TestProve_SkipsWhenSuiteCmdUnknown pins the decline-don't-guess behavior: an
// unidentifiable toolchain with no configured suite_cmd skips the prover
// without flagging needs-human and without any sandbox execution.
func TestProve_SkipsWhenSuiteCmdUnknown(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	f := seedFinding(t, st)

	repoDir := t.TempDir() // no marker files at all
	if err := os.WriteFile(filepath.Join(repoDir, "code.zig"), []byte("// zig"), 0o644); err != nil {
		t.Fatal(err)
	}

	sb := sandbox.NewMock(sandbox.MockResponse{})
	p := &PatchProver{
		client:      newScriptedClient(),
		sb:          sb,
		repoDir:     repoDir,
		maxAttempts: 3,
		artifactDir: t.TempDir(),
	}
	att := &Attempt{Plan: &Plan{Files: map[string]string{"x_test.zig": "t"}, Cmd: []string{"zig", "test", "x_test.zig"}}}

	out, err := p.Prove(ctx, st, f, att)
	if err != nil {
		t.Fatalf("Prove: %v", err)
	}
	if !out.SkippedNoSuiteCmd {
		t.Error("expected SkippedNoSuiteCmd")
	}
	if out.NeedsHuman || out.FixWitnessed {
		t.Errorf("skip must not flag needs-human or fix-witnessed: %+v", out)
	}
	if calls := len(sb.Calls()); calls != 0 {
		t.Errorf("skip must not execute the sandbox, made %d calls", calls)
	}
	got, err := st.GetFindingByFingerprint(ctx, f.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if got.NeedsHuman {
		t.Error("store finding must not be flagged needs-human on skip")
	}
}

// TestIsTestPath pins the cross-language test-file guard.
func TestIsTestPath(t *testing.T) {
	yes := []string{
		"pkg/foo_test.go", "deep/nested/bar_test.go",
		"test_util.py", "pkg/test_thing.py", "pkg/thing_test.py",
		"src/foo.test.ts", "src/foo.spec.jsx", "lib/user_spec.rb",
		"src/FooTest.java", "src/FooTests.cs",
		"tests/anything.go", "test/x.c", "__tests__/y.js", "spec/z.rb", "testdata/fixture.go",
	}
	for _, p := range yes {
		if !isTestPath(p) {
			t.Errorf("isTestPath(%q) = false, want true", p)
		}
	}
	no := []string{
		"pkg/foo.go", "contest/winner.go", "src/latest.ts", "attest.go",
		"pkg/protest.py", "respec.rb", "testify.go",
	}
	for _, p := range no {
		if isTestPath(p) {
			t.Errorf("isTestPath(%q) = true, want false", p)
		}
	}
}

// TestFlagNeedsHuman_Idempotent pins that repeated exhaustion runs do not grow
// Reasoning with duplicate PATCH-PROVER appends.
func TestFlagNeedsHuman_Idempotent(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	f := seedFinding(t, st)

	for i := 0; i < 3; i++ {
		if err := flagNeedsHuman(ctx, st, f, 3, "no fix found"); err != nil {
			t.Fatalf("flagNeedsHuman run %d: %v", i, err)
		}
	}
	got, err := st.GetFindingByFingerprint(ctx, f.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if !got.NeedsHuman {
		t.Error("NeedsHuman must be set")
	}
	if n := strings.Count(got.Reasoning, "PATCH-PROVER:"); n != 1 {
		t.Errorf("Reasoning contains %d PATCH-PROVER appends, want exactly 1:\n%s", n, got.Reasoning)
	}
}
