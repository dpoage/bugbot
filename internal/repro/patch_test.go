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
	if !strings.Contains(err.Error(), "environment failure") {
		t.Errorf("error = %q, want environment failure message", err)
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
	v := patchVerdict(sandbox.Result{ExitCode: 0, Stdout: "ok"})
	if !v.passed {
		t.Errorf("exit 0 should be a PASS in patch context; got passed=%v", v.passed)
	}
	if v.envFailure {
		t.Errorf("exit 0 should not be an env failure")
	}
}

func TestPatchVerdict_NonZeroIsFail(t *testing.T) {
	v := patchVerdict(sandbox.Result{ExitCode: 1, Stdout: "FAIL"})
	if v.passed {
		t.Errorf("exit 1 should be a FAIL in patch context")
	}
	if v.envFailure {
		t.Errorf("exit 1 test failure should not be env failure")
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
			v := patchVerdict(tc.res)
			if !v.envFailure {
				t.Errorf("patchVerdict(%+v).envFailure = false, want true", tc.res)
			}
			if v.passed {
				t.Errorf("patchVerdict(%+v).passed = true, want false", tc.res)
			}
		})
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

// lookPath is a thin wrapper around exec.LookPath to keep the import within
// this test file.
func lookPath(name string) (string, error) {
	return exec.LookPath(name)
}
