package repro

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/sandbox"
	"github.com/dpoage/bugbot/internal/store"
)

// planBody renders a Plan as the JSON the agent would emit.
func planBody(t *testing.T, p Plan) string {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	return string(b)
}

// goodPlan is a representative valid repro plan.
func goodPlan() Plan {
	return Plan{
		Files:  map[string]string{"bug_test.go": "package bug\n\nimport \"testing\"\n\nfunc TestBug(t *testing.T){ t.Fatal(\"boom\") }\n"},
		Cmd:    []string{"go", "test", "-run", "TestBug", "./..."},
		Expect: "TestBug fails because Divide(1,0) returns 0 instead of erroring",
	}
}

// newRepoDir creates a minimal repo directory the agent's read-only tools can
// be rooted at (the agent never actually reads it in these tests, but New
// requires the dir to exist).
func newRepoDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module bug\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// seedFinding inserts a Tier-2 verified finding and returns it.
func seedFinding(t *testing.T, st *store.Store) store.Finding {
	t.Helper()
	fp := store.Fingerprint("logic", "calc.go", 12, "Divide ignores zero divisor")
	f := store.Finding{
		Fingerprint: fp,
		Title:       "Divide ignores zero divisor",
		Description: "Divide returns 0 for a zero divisor instead of an error.",
		Reasoning:   "Verified: no zero check before the division.",
		Severity:    "high",
		Tier:        2,
		Status:      store.StatusOpen,
		Lens:        "logic",
		File:        "calc.go",
		Line:        12,
		CommitSHA:   "abc123",
		FileHash:    "deadbeef",
	}
	stored, err := st.UpsertFinding(context.Background(), f)
	if err != nil {
		t.Fatalf("seed finding: %v", err)
	}
	return stored
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// --- success path ----------------------------------------------------------

func TestPromoteAll_Success(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	finding := seedFinding(t, st)
	repoDir := newRepoDir(t)
	artifactDir := t.TempDir()

	client := newScriptedClient(planBody(t, goodPlan()))
	// Mock sandbox: a genuine assertion failure (exit 1) with test output.
	sb := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{
		ExitCode: 1,
		Stdout:   "--- FAIL: TestBug\n    bug_test.go:1: Divide(1,0) = 0, want error\nFAIL",
	}})

	r, err := New(client, sb, repoDir, Options{ArtifactDir: artifactDir})
	if err != nil {
		t.Fatal(err)
	}

	summary, err := r.PromoteAll(ctx, st, []store.Finding{finding})
	if err != nil {
		t.Fatalf("PromoteAll: %v", err)
	}

	if summary.Promoted != 1 || summary.Failed != 0 || summary.Attempted != 1 {
		t.Fatalf("summary = %+v, want 1 promoted", summary)
	}

	// Sandbox was asked with network none and the repro file injected.
	calls := sb.Calls()
	if len(calls) != 1 {
		t.Fatalf("sandbox calls = %d, want 1", len(calls))
	}
	spec := calls[0].Spec
	if spec.Network != "none" {
		t.Errorf("network = %q, want none", spec.Network)
	}
	if _, ok := spec.WriteFiles["bug_test.go"]; !ok {
		t.Errorf("repro file not injected: %v", spec.WriteFiles)
	}

	// Artifacts on disk: repro file + run.sh + README.md.
	bundle := filepath.Join(artifactDir, finding.ID)
	for _, name := range []string{"bug_test.go", "run.sh", "README.md"} {
		if _, err := os.Stat(filepath.Join(bundle, name)); err != nil {
			t.Errorf("missing artifact %s: %v", name, err)
		}
	}
	// run.sh is executable and contains the command.
	runSh, err := os.ReadFile(filepath.Join(bundle, "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(runSh), "go test") {
		t.Errorf("run.sh missing command: %s", runSh)
	}
	readme, err := os.ReadFile(filepath.Join(bundle, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readme), finding.Title) {
		t.Errorf("README missing finding title")
	}

	// Finding updated to T1 with repro_path.
	got, err := st.GetFindingByFingerprint(ctx, finding.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if got.Tier != 1 {
		t.Errorf("tier = %d, want 1", got.Tier)
	}
	if got.ReproPath != bundle {
		t.Errorf("repro_path = %q, want %q", got.ReproPath, bundle)
	}
}

// TestPromoteAll_PlanWithoutExpectPromotes proves that a plan omitting the
// descriptive "expect" field still reproduces and promotes. expect is not in
// planSchema's required set, so RunJSON must accept a runnable files+cmd plan
// on the first pass with no repair round-trip. Regression: expect was formerly
// required, so a model that produced a perfectly runnable plan but omitted the
// prose description aborted the whole attempt with a hard "missing required
// field expect" parse error, even though expect is never load-bearing for
// execution (validatePlan never checked it; consumers guard on emptiness).
func TestPromoteAll_PlanWithoutExpectPromotes(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	finding := seedFinding(t, st)
	repoDir := newRepoDir(t)

	// Plan body WITHOUT an "expect" key. planBody(goodPlan()) cannot express
	// this: a Plan always marshals "expect":"", which a present field would trip
	// against the schema's minLength:1. The omission is the case under test.
	body := `{"files":{"bug_test.go":"package bug\n\nimport \"testing\"\n\nfunc TestBug(t *testing.T){ t.Fatal(\"boom\") }\n"},"cmd":["go","test","-run","TestBug","./..."]}`
	client := newScriptedClient(body)
	sb := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{
		ExitCode: 1,
		Stdout:   "--- FAIL: TestBug\n    bug_test.go:1: boom\nFAIL",
	}})

	r, err := New(client, sb, repoDir, Options{ArtifactDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	summary, err := r.PromoteAll(ctx, st, []store.Finding{finding})
	if err != nil {
		t.Fatalf("PromoteAll: %v", err)
	}
	if summary.Promoted != 1 || summary.Failed != 0 {
		t.Fatalf("summary = %+v, want 1 promoted / 0 failed", summary)
	}
	// Exactly one completion: the expect-less plan parsed on the first pass, so
	// no repair round-trip fired. A second request would mean expect was still
	// being enforced as required.
	if n := len(client.allRequests()); n != 1 {
		t.Fatalf("llm completions = %d, want 1 (no repair round-trip)", n)
	}
}

// TestPromoteAll_AbsolutePathPlanRetries proves that a plan injecting an
// ABSOLUTE/escaping file path (e.g. "/tmp/repro_test.cpp") is caught by
// validatePlan and fed back as a recoverable revision — NOT executed and NOT
// surfaced as a hard infrastructure error. Regression: such a path reached the
// sandbox's applyWriteFiles, which rejected it with "path escapes workspace",
// aborting the entire attempt with an `error:` instead of letting the agent
// correct it. The first (bad) plan must never touch the sandbox; the corrected
// plan promotes, and the revision request must carry actionable path guidance.
func TestPromoteAll_AbsolutePathPlanRetries(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	finding := seedFinding(t, st)
	repoDir := newRepoDir(t)

	bad := Plan{
		Files:  map[string]string{"/tmp/repro_test.cpp": "int main(){return 1;}\n"},
		Cmd:    []string{"sh", "-c", "g++ /tmp/repro_test.cpp -o /tmp/r && /tmp/r"},
		Expect: "leak", // non-empty so the plan clears the schema; the path is the defect
	}
	client := newScriptedClient(planBody(t, bad), planBody(t, goodPlan()))
	sb := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{
		ExitCode: 1,
		Stdout:   "--- FAIL: TestBug\nFAIL",
	}})

	r, err := New(client, sb, repoDir, Options{ArtifactDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	summary, err := r.PromoteAll(ctx, st, []store.Finding{finding})
	if err != nil {
		t.Fatalf("PromoteAll returned a hard error for a recoverable bad path: %v", err)
	}
	if summary.Promoted != 1 || summary.Failed != 0 {
		t.Fatalf("summary = %+v, want 1 promoted / 0 failed (retry after bad path)", summary)
	}
	// The absolute-path plan must never reach the sandbox; only the corrected
	// plan executes.
	if n := len(sb.Calls()); n != 1 {
		t.Fatalf("sandbox calls = %d, want 1 (bad-path plan must not execute)", n)
	}
	// Two completions: the rejected plan + the corrected one. The revision
	// request must name the offending path and the workspace-relative rule.
	reqs := client.allRequests()
	if len(reqs) != 2 {
		t.Fatalf("llm completions = %d, want 2 (reject + revise)", len(reqs))
	}
	task := client.taskText(1)
	for _, want := range []string{"/tmp/repro_test.cpp", "workspace-relative"} {
		if !strings.Contains(task, want) {
			t.Errorf("revision task missing %q; got:\n%s", want, task)
		}
	}
}

// TestPromoteAll_UnparseablePlanDoesNotAbort proves the live 2026-06-19 C++
// regression is fixed: when a REVISION round's plan is unparseable / schema-
// violating (observed: a weak model degenerating into a broken tool-call blob
// as its final answer, so the plan omits the required "cmd"), the whole finding
// must NOT be hard-aborted as an `error:` outcome. RunJSON's parse failure is
// recoverable (errors.Is ErrUnparseableOutput), so Attempt feeds it back and
// the finding settles as an honest non-reproduction at Tier-2 — never an
// infrastructure error that discards round 1's already-executed verdict.
func TestPromoteAll_UnparseablePlanDoesNotAbort(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	finding := seedFinding(t, st)
	repoDir := newRepoDir(t)

	// Round 1: a valid, runnable plan that does NOT demonstrate (sandbox exits
	// 0). Round 2 (revision): the model emits garbage for both the run and the
	// repair completion, so RunJSON returns ErrUnparseableOutput.
	client := newScriptedClient(
		planBody(t, goodPlan()),
		"]<]minimax[>[ broken tool call, not json",
		"still not json",
	)
	sb := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{
		ExitCode: 0,
		Stdout:   "ok\nPASS",
	}})

	r, err := New(client, sb, repoDir, Options{ArtifactDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	summary, err := r.PromoteAll(ctx, st, []store.Finding{finding})
	if err != nil {
		t.Fatalf("PromoteAll: %v", err)
	}
	if summary.Promoted != 0 || summary.Failed != 1 {
		t.Fatalf("summary = %+v, want 0 promoted / 1 failed (could not reproduce)", summary)
	}

	o := summary.PerFinding[0]
	// The regression: a bad revision round must settle as a recoverable
	// non-reproduction, NOT a hard infrastructure error.
	if o.Err != nil {
		t.Fatalf("PerFinding.Err = %v, want nil (unparseable plan is recoverable, not infra error)", o.Err)
	}
	if !strings.HasPrefix(o.Reason, "unparseable plan:") {
		t.Errorf("Reason = %q, want an 'unparseable plan:' verdict (not 'error: ...')", o.Reason)
	}
	if o.Attempts != DefaultMaxAttempts {
		t.Errorf("Attempts = %d, want %d (both rounds tried; not aborted on round 2)", o.Attempts, DefaultMaxAttempts)
	}

	// Round 1 executed once; the unparseable round 2 never reached the sandbox.
	if n := len(sb.Calls()); n != 1 {
		t.Errorf("sandbox calls = %d, want 1 (only the valid round-1 plan executes)", n)
	}
	// 3 completions: round-1 plan, round-2 run, round-2 repair.
	if n := len(client.allRequests()); n != 3 {
		t.Errorf("llm completions = %d, want 3 (r1 plan + r2 run + r2 repair)", n)
	}

	// Failure demotes nothing: the finding stays at Tier-2.
	got, err := st.GetFindingByFingerprint(ctx, finding.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if got.Tier != 2 {
		t.Errorf("tier = %d, want 2 (unreproduced finding untouched)", got.Tier)
	}
}

// --- dependency strategy wiring --------------------------------------------

// TestPromoteAll_VendoredSetsGoflags verifies a vendored repo run carries
// GOFLAGS=-mod=vendor and no extra mounts, in the default "off" mode.
func TestPromoteAll_VendoredSetsGoflags(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	finding := seedFinding(t, st)
	repoDir := newRepoDir(t) // has go.mod
	// Make it vendored.
	if err := os.MkdirAll(filepath.Join(repoDir, "vendor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "vendor", "modules.txt"), []byte("# bug\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	client := newScriptedClient(planBody(t, goodPlan()))
	sb := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 1, Stdout: "FAIL"}})

	r, err := New(client, sb, repoDir, Options{ArtifactDir: t.TempDir()}) // DepStrategy empty == off
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.PromoteAll(ctx, st, []store.Finding{finding}); err != nil {
		t.Fatalf("PromoteAll: %v", err)
	}

	calls := sb.Calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	spec := calls[0].Spec
	if !contains(spec.Env, "GOFLAGS=-mod=vendor") {
		t.Errorf("vendored run env = %v, want GOFLAGS=-mod=vendor", spec.Env)
	}
	if len(spec.ROMounts) != 0 {
		t.Errorf("vendored run should have no mounts, got %+v", spec.ROMounts)
	}
}

// TestPromoteAll_HostStrategyMountsCache verifies the host strategy attaches a
// read-only modcache mount and GOMODCACHE/GOPROXY env to the repro Spec.
func TestPromoteAll_HostStrategyMountsCache(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	finding := seedFinding(t, st)
	repoDir := newRepoDir(t) // has go.mod, not vendored
	// Point the host modcache at a real temp dir so resolution is deterministic
	// regardless of the test machine's go env.
	cache := t.TempDir()
	t.Setenv("GOMODCACHE", cache)

	client := newScriptedClient(planBody(t, goodPlan()))
	sb := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 1, Stdout: "FAIL"}})

	r, err := New(client, sb, repoDir, Options{ArtifactDir: t.TempDir(), DepStrategy: sandbox.DepStrategyHost})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.PromoteAll(ctx, st, []store.Finding{finding}); err != nil {
		t.Fatalf("PromoteAll: %v", err)
	}

	spec := sb.Calls()[0].Spec
	if len(spec.ROMounts) != 1 || spec.ROMounts[0].HostPath != cache {
		t.Fatalf("host strategy mount = %+v, want host=%q", spec.ROMounts, cache)
	}
	if spec.ROMounts[0].ContainerPath != "/modcache" {
		t.Errorf("mount container path = %q, want /modcache", spec.ROMounts[0].ContainerPath)
	}
	for _, want := range []string{"GOMODCACHE=/modcache", "GOPROXY=off"} {
		if !contains(spec.Env, want) {
			t.Errorf("host run env missing %q; got %v", want, spec.Env)
		}
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// TestExecute_SetupCmdsPropagate verifies that Resolution.SetupCmds are
// threaded from r.deps into the sandbox Spec by execute().
func TestExecute_SetupCmdsPropagate(t *testing.T) {
	ctx := context.Background()
	repoDir := newRepoDir(t)
	st := openStore(t)
	finding := seedFinding(t, st)

	setupCmds := [][]string{{"npm", "ci", "--offline"}, {"pip", "install", "--no-index"}}
	client := newScriptedClient(planBody(t, goodPlan()))
	sb := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 1, Stdout: "FAIL"}})

	// Construct a Reproducer with a pre-populated deps.SetupCmds to bypass the
	// ecosystem resolver (Go never contributes SetupCmds). Use resolved options
	// so MaxParallel is non-zero (a zero-capacity semaphore deadlocks).
	opts := Options{ArtifactDir: t.TempDir(), MaxAttempts: 1}.resolve()
	r := &Reproducer{
		client:  client,
		sb:      sb,
		repoDir: repoDir,
		opts:    opts,
		deps: sandbox.Resolution{
			SetupCmds: setupCmds,
			Strategy:  sandbox.DepStrategyOff,
		},
	}

	if _, err := r.PromoteAll(ctx, st, []store.Finding{finding}); err != nil {
		t.Fatalf("PromoteAll: %v", err)
	}
	calls := sb.Calls()
	if len(calls) == 0 {
		t.Fatal("sandbox was not called")
	}
	spec := calls[0].Spec
	if len(spec.SetupCmds) != 2 {
		t.Fatalf("SetupCmds len = %d, want 2; got %v", len(spec.SetupCmds), spec.SetupCmds)
	}
	if spec.SetupCmds[0][0] != "npm" || spec.SetupCmds[1][0] != "pip" {
		t.Errorf("SetupCmds content wrong: %v", spec.SetupCmds)
	}
}

// --- zero-exit -> revision -> success --------------------------------------

func TestPromoteAll_ZeroExitThenRevision(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	finding := seedFinding(t, st)
	repoDir := newRepoDir(t)
	artifactDir := t.TempDir()

	// Two scripted plans: the first will be run and exit 0; the second after
	// feedback exits 1.
	plan1 := goodPlan()
	plan1.Files = map[string]string{"bug_test.go": "package bug\nimport \"testing\"\nfunc TestBug(t *testing.T){}\n"}
	plan2 := goodPlan()
	client := newScriptedClient(planBody(t, plan1), planBody(t, plan2))

	sb := sandbox.NewMock(sandbox.MockResponse{})
	sb.EnqueueResponse(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 0, Stdout: "ok\tbug\t0.01s\nPASS"}})
	sb.EnqueueResponse(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 1, Stdout: "--- FAIL: TestBug\nFAIL"}})

	r, err := New(client, sb, repoDir, Options{ArtifactDir: artifactDir})
	if err != nil {
		t.Fatal(err)
	}

	summary, err := r.PromoteAll(ctx, st, []store.Finding{finding})
	if err != nil {
		t.Fatalf("PromoteAll: %v", err)
	}
	if summary.Promoted != 1 {
		t.Fatalf("want promoted after revision, got %+v", summary)
	}
	if got := summary.PerFinding[0].Attempts; got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}

	// The second agent request must carry the corrective feedback about exit 0.
	second := client.taskText(1)
	if !strings.Contains(second, "exited 0") {
		t.Errorf("revision task missing exit-0 feedback:\n%s", second)
	}
	if !strings.Contains(second, "Revision required") {
		t.Errorf("revision task missing revision marker:\n%s", second)
	}
}

// --- compile-error guard ----------------------------------------------------

func TestPromoteAll_CompileErrorGuard(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	finding := seedFinding(t, st)
	repoDir := newRepoDir(t)
	artifactDir := t.TempDir()

	// Both attempts return a build failure (non-zero exit) — must NOT promote.
	client := newScriptedClient(planBody(t, goodPlan()), planBody(t, goodPlan()))
	sb := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{
		ExitCode: 2,
		Stderr:   "# bug [build failed]\n./bug_test.go:4:2: undefined: Divide",
	}})

	r, err := New(client, sb, repoDir, Options{ArtifactDir: artifactDir})
	if err != nil {
		t.Fatal(err)
	}

	summary, err := r.PromoteAll(ctx, st, []store.Finding{finding})
	if err != nil {
		t.Fatalf("PromoteAll: %v", err)
	}
	if summary.Promoted != 0 || summary.Failed != 1 {
		t.Fatalf("build error should not promote: %+v", summary)
	}
	if !strings.Contains(summary.PerFinding[0].Reason, "build_error") {
		t.Errorf("reason = %q, want build_error", summary.PerFinding[0].Reason)
	}

	// Finding stays T2.
	got, err := st.GetFindingByFingerprint(ctx, finding.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if got.Tier != 2 {
		t.Errorf("tier = %d, want unchanged 2", got.Tier)
	}
	if got.ReproPath != "" {
		t.Errorf("repro_path = %q, want empty", got.ReproPath)
	}

	// No artifact directory left behind.
	if _, err := os.Stat(filepath.Join(artifactDir, finding.ID)); !os.IsNotExist(err) {
		t.Errorf("artifact dir should not exist for failed repro: %v", err)
	}

	// The build-error feedback reached the agent's second request.
	if !strings.Contains(client.taskText(1), "BUILD") {
		t.Errorf("revision task missing build feedback:\n%s", client.taskText(1))
	}
}

// --- max-attempts exhaustion ------------------------------------------------

func TestPromoteAll_Exhaustion(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	finding := seedFinding(t, st)
	repoDir := newRepoDir(t)
	artifactDir := t.TempDir()

	client := newScriptedClient(planBody(t, goodPlan()), planBody(t, goodPlan()), planBody(t, goodPlan()))
	// Always exits 0 -> never demonstrates.
	sb := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 0, Stdout: "PASS"}})

	r, err := New(client, sb, repoDir, Options{ArtifactDir: artifactDir, MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}

	summary, err := r.PromoteAll(ctx, st, []store.Finding{finding})
	if err != nil {
		t.Fatalf("PromoteAll: %v", err)
	}
	if summary.Failed != 1 || summary.Promoted != 0 {
		t.Fatalf("exhaustion should fail: %+v", summary)
	}
	if got := summary.PerFinding[0].Attempts; got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
	if sb.CallCount() != 3 {
		t.Errorf("sandbox calls = %d, want 3", sb.CallCount())
	}

	// No artifact dir left behind.
	if _, err := os.Stat(filepath.Join(artifactDir, finding.ID)); !os.IsNotExist(err) {
		t.Errorf("artifact dir should not exist after exhaustion: %v", err)
	}

	// Finding stays T2.
	got, _ := st.GetFindingByFingerprint(ctx, finding.Fingerprint)
	if got.Tier != 2 {
		t.Errorf("tier = %d, want 2", got.Tier)
	}
}

// --- options defaults -------------------------------------------------------

func TestInterpret_EnvironmentFailuresNeverDemonstrate(t *testing.T) {
	// The default command used by these cases: a generic `go test`
	// invocation. Most of the cases below produce Go-style output
	// (build cache refusal, syntax error, --- FAIL), so the
	// detected ecosystem is Go. Cases that should fail regardless
	// of ecosystem (exit 125/126/127, timeout, exit_zero) use the
	// same command because the exit-code short-circuits run before
	// the ecosystem table is consulted.
	goCmd := []string{"go", "test", "./..."}
	cases := []struct {
		name   string
		res    sandbox.Result
		cmd    []string
		reason VerdictReason
	}{
		{"runtime error 125", sandbox.Result{ExitCode: 125, Stderr: "podman: error"}, goCmd, VerdictReasonEnvironmentError},
		{"not executable 126", sandbox.Result{ExitCode: 126, Stderr: "permission denied"}, goCmd, VerdictReasonEnvironmentError},
		{"not found 127", sandbox.Result{ExitCode: 127, Stderr: "sh: gotest: not found"}, goCmd, VerdictReasonEnvironmentError},
		// The real-world case this guard exists for: read-only root broke the
		// Go build cache, exit 1 in 0.13s, and got promoted to Tier 1.
		{"go build cache", sandbox.Result{ExitCode: 1, Stderr: "failed to initialize build cache at /root/.cache/go-build: mkdir /root/.cache: read-only file system"}, goCmd, VerdictReasonEnvironmentError},
		{"read-only fs", sandbox.Result{ExitCode: 1, Stderr: "mkdir /data: Read-only file system"}, goCmd, VerdictReasonEnvironmentError},
		{"disk full", sandbox.Result{ExitCode: 1, Stderr: "write /tmp/x: no space left on device"}, goCmd, VerdictReasonEnvironmentError},
		{"timeout", sandbox.Result{ExitCode: -1, TimedOut: true}, goCmd, VerdictReasonTimeout},
		{"exit zero", sandbox.Result{ExitCode: 0, Stdout: "ok"}, goCmd, VerdictReasonExitZero},
		{"compile error", sandbox.Result{ExitCode: 2, Stderr: "./x_test.go:3:1: syntax error"}, goCmd, VerdictReasonBuildError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := interpret(tc.res, tc.cmd)
			if v.demonstrated {
				t.Fatalf("interpret(%+v) demonstrated=true; must never demonstrate", tc.res)
			}
			if v.reason != tc.reason {
				t.Errorf("reason = %q, want %q", v.reason, tc.reason)
			}
		})
	}

	// A genuine test failure still demonstrates. The Go ecosystem
	// is selected by the command so the --- FAIL marker is matched
	// against the Go ran-evidence list — preserving the legacy
	// "Go verdicts UNCHANGED" guarantee (bugbot-vig acceptance #5).
	v := interpret(sandbox.Result{ExitCode: 1, Stdout: "--- FAIL: TestDivide (0.00s)\n    calc_test.go:6: bug\nFAIL"}, []string{"go", "test", "-run", "TestDivide"})
	if !v.demonstrated {
		t.Fatalf("genuine test failure must demonstrate; got reason=%q", v.reason)
	}
}

// --- bugbot-vig: per-ecosystem positive ran-evidence gate ------------------

// TestInterpret_GoCgoRefusal_NotDemonstrated is the regression test for the
// motivating instance in bugbot-vig: `go test -race` exits 2 with the
// single-line "go: -race requires cgo" toolchain refusal BEFORE compiling
// any tests.  The output contains the Go toolchain marker ("go: ") but
// NONE of the positive ran-evidence markers ("--- FAIL", "FAIL\t",
// "panic:", "WARNING: DATA RACE").  Under the old rule, the bare non-zero
// exit was promoted to a Tier-1 demonstration.  Under the new rule, the
// toolchain refusal is classified toolchain_error and the repro is not
// demonstrated.
func TestInterpret_GoCgoRefusal_NotDemonstrated(t *testing.T) {
	cases := []struct {
		name string
		res  sandbox.Result
	}{
		{
			"cgo refusal (race)",
			sandbox.Result{ExitCode: 2, Stderr: "go: -race requires cgo; enable cgo by setting CGO_ENABLED=1"},
		},
		{
			"go command not found",
			sandbox.Result{ExitCode: 2, Stderr: "go: command not found"},
		},
		{
			"go cannot find main module",
			sandbox.Result{ExitCode: 2, Stderr: "go: cannot find main module; see 'go help modules'"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := interpret(tc.res, []string{"go", "test", "-race", "./..."})
			if v.demonstrated {
				t.Fatalf("toolchain refusal must not demonstrate; got demonstrated=true, reason=%q", v.reason)
			}
			if v.reason != "toolchain_error" {
				t.Errorf("reason = %q, want %q", v.reason, "toolchain_error")
			}
			if v.ecosystem != "go" {
				t.Errorf("ecosystem = %q, want %q", v.ecosystem, "go")
			}
		})
	}
}

// TestInterpret_GoGenuineFailure_Demonstrated confirms acceptance #5:
// existing Go verdicts are UNCHANGED for genuine test failures. The
// classic "--- FAIL: TestX" / "FAIL" line shapes are still positive
// ran-evidence and the test is still demonstrated.
func TestInterpret_GoGenuineFailure_Demonstrated(t *testing.T) {
	cases := []struct {
		name string
		res  sandbox.Result
	}{
		{
			"--- FAIL shape",
			sandbox.Result{ExitCode: 1, Stdout: "--- FAIL: TestDivide (0.00s)\n    calc_test.go:6: bug\nFAIL\nFAIL\tgithub.com/example/bug\t0.123s"},
		},
		{
			"panic shape",
			sandbox.Result{ExitCode: 2, Stderr: "panic: runtime error: integer divide by zero"},
		},
		{
			"data race shape",
			sandbox.Result{ExitCode: 1, Stderr: "WARNING: DATA RACE\nRead at 0x00c0000160a0 by goroutine 7:"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := interpret(tc.res, []string{"go", "test", "-race", "./..."})
			if !v.demonstrated {
				t.Fatalf("genuine Go failure must demonstrate; got reason=%q", v.reason)
			}
		})
	}
}

// TestInterpret_PytestGenuineFailure_Demonstrated confirms acceptance #4:
// adding a second ecosystem (pytest) requires only a table entry plus
// fixture transcripts. Pytest's "FAILED tests/...::test_x" line plus
// "AssertionError" are positive ran-evidence; a non-zero exit is
// demonstrated.
func TestInterpret_PytestGenuineFailure_Demonstrated(t *testing.T) {
	cases := []struct {
		name string
		res  sandbox.Result
		cmd  []string
	}{
		{
			"FAILED + AssertionError",
			sandbox.Result{
				ExitCode: 1,
				Stdout:   "============================= test session starts ==============================\nplatform linux -- Python 3.11.4, pytest-7.4.0\ncollected 1 item\n\ntests/test_calc.py F                                                         [100%]\n\n=================================== FAILURES ===================================\n_______________________________ test_divide_by_zero ______________________________\n\n    def test_divide_by_zero():\n        assert divide(1, 0) is None\n>       assert divide(1, 0) is None\nE       AssertionError: assert 0 is None\nE       assert 0 == None\n\ntests/test_calc.py:5: AssertionError\n=========================== short test summary info ============================\nFAILED tests/test_calc.py::test_divide_by_zero - AssertionError\n",
			},
			[]string{"pytest", "tests/"},
		},
		{
			"FAILED with python -m pytest launcher",
			sandbox.Result{
				ExitCode: 1,
				Stderr:   "FAILED tests/test_x.py::TestX - AssertionError",
				Stdout:   "= 1 failed in 0.12s =",
			},
			[]string{"python", "-m", "pytest", "tests/"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := interpret(tc.res, tc.cmd)
			if !v.demonstrated {
				t.Fatalf("genuine pytest failure must demonstrate; got reason=%q, ecosystem=%q", v.reason, v.ecosystem)
			}
			if v.ecosystem != "python" {
				t.Errorf("ecosystem = %q, want %q", v.ecosystem, "python")
			}
		})
	}
}

// TestInterpret_PytestCollectionError_NotDemonstrated confirms the
// inverse: a non-zero exit WITHOUT pytest's positive ran-evidence is NOT a
// demonstration. ModuleNotFoundError / ImportError at collection time
// never run the test, so they are classified as a build error (collection
// failure) and the repro is not demonstrated.
func TestInterpret_PytestCollectionError_NotDemonstrated(t *testing.T) {
	cases := []struct {
		name   string
		res    sandbox.Result
		reason VerdictReason
	}{
		{
			"ModuleNotFoundError",
			sandbox.Result{
				ExitCode: 4,
				Stderr:   "ERROR tests/test_x.py - ModuleNotFoundError: No module named 'totally_missing_pkg'",
			},
			VerdictReasonBuildError,
		},
		{
			"ImportError collection failure",
			sandbox.Result{
				ExitCode: 2,
				Stderr:   "ImportError: cannot import name 'foo' from 'bug' (unknown location)",
			},
			VerdictReasonBuildError,
		},
		{
			"pytest no tests ran",
			sandbox.Result{
				ExitCode: 5,
				Stderr:   "pytest: error: no tests ran",
			},
			VerdictReasonToolchainError,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := interpret(tc.res, []string{"pytest", "tests/"})
			if v.demonstrated {
				t.Fatalf("collection/import error must not demonstrate; got demonstrated=true, reason=%q", v.reason)
			}
			if v.reason != tc.reason {
				t.Errorf("reason = %q, want %q", v.reason, tc.reason)
			}
		})
	}
}

// TestInterpret_UnknownEcosystem_NotDemonstrated confirms the central
// invariant: an unknown launcher (no Go / pytest / cargo / npm / jest /
// ctest prefix) NEVER demonstrates on a bare non-zero exit. The unknown
// ecosystem still has a small generic ran-marker set (FAIL / FAILED /
// panic), so a transcript that contains those will demonstrate, but a
// bare non-zero exit will not.
func TestInterpret_UnknownEcosystem_NotDemonstrated(t *testing.T) {
	v := interpret(sandbox.Result{ExitCode: 1, Stderr: "make: *** [Makefile:7: test] Error 1"}, []string{"make", "test"})
	if v.demonstrated {
		t.Fatalf("unknown ecosystem bare non-zero must not demonstrate; got reason=%q, ecosystem=%q", v.reason, v.ecosystem)
	}
	if v.ecosystem != "unknown" {
		t.Errorf("ecosystem = %q, want %q", v.ecosystem, "unknown")
	}
}

// TestDetectEcosystem is a focused unit test for the argv-to-ecosystem
// mapping table.
func TestDetectEcosystem(t *testing.T) {
	cases := []struct {
		cmd  []string
		want sandbox.Ecosystem
	}{
		{[]string{"go", "test", "./..."}, sandbox.EcosystemGo},
		{[]string{"go", "test", "-race", "./..."}, sandbox.EcosystemGo},
		{[]string{"go", "build", "./..."}, sandbox.EcosystemGo},
		{[]string{"pytest", "tests/"}, sandbox.EcosystemPython},
		{[]string{"py.test", "tests/"}, sandbox.EcosystemPython},
		{[]string{"python", "-m", "pytest", "tests/"}, sandbox.EcosystemPython},
		{[]string{"python3", "-m", "py.test", "tests/"}, sandbox.EcosystemPython},
		{[]string{"cargo", "test"}, sandbox.EcosystemRust},
		{[]string{"cargo", "build"}, sandbox.EcosystemRust},
		{[]string{"npm", "test"}, sandbox.EcosystemJS},
		{[]string{"yarn", "test"}, sandbox.EcosystemJS},
		{[]string{"pnpm", "test"}, sandbox.EcosystemJS},
		{[]string{"npx", "jest"}, sandbox.EcosystemJS},
		{[]string{"jest", "src/"}, sandbox.EcosystemJS},
		{[]string{"vitest", "run"}, sandbox.EcosystemJS},
		{[]string{"ctest", "--output-on-failure"}, sandbox.EcosystemCpp},
		{[]string{"cmake", "-B", "build", "-S", "."}, sandbox.EcosystemCpp},
		{[]string{"g++", "-std=c++17", "t.cpp", "-o", "t"}, sandbox.EcosystemCpp},
		{[]string{"clang++", "-fsanitize=address", "t.cpp"}, sandbox.EcosystemCpp},
		{[]string{"cc", "-c", "t.c"}, sandbox.EcosystemCpp},
		{[]string{"c++", "t.cpp"}, sandbox.EcosystemCpp},
		{[]string{"bash", "-c", "cmake -B build && ctest"}, sandbox.EcosystemCpp},
		{[]string{"bash", "-lc", "cmake -B build && ctest"}, sandbox.EcosystemCpp},
		{[]string{"sh", "-lc", "g++ t.cpp -o t && ./t"}, sandbox.EcosystemCpp},
		{[]string{"bash", "-c", "go test ./..."}, sandbox.EcosystemGo},
		{[]string{"make", "test"}, sandbox.EcosystemUnknown},
		{[]string{}, sandbox.EcosystemUnknown},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.cmd, " "), func(t *testing.T) {
			eco := detectEcosystem(tc.cmd)
			if eco.name != tc.want {
				t.Errorf("detectEcosystem(%v) = %q, want %q", tc.cmd, eco.name, tc.want)
			}
		})
	}
}

func TestOptionsResolve(t *testing.T) {
	o := Options{}.resolve()
	if o.MaxAttempts != DefaultMaxAttempts {
		t.Errorf("MaxAttempts = %d", o.MaxAttempts)
	}
	if o.MaxParallel != DefaultMaxParallel {
		t.Errorf("MaxParallel = %d", o.MaxParallel)
	}
	if o.Timeout != DefaultTimeout {
		t.Errorf("Timeout = %v", o.Timeout)
	}
	if o.ArtifactDir != DefaultArtifactDir {
		t.Errorf("ArtifactDir = %q", o.ArtifactDir)
	}

	neg := Options{MaxAttempts: -5, MaxParallel: -5, Timeout: time.Second}.resolve()
	if neg.MaxAttempts != 1 || neg.MaxParallel != 1 {
		t.Errorf("negatives not clamped to 1: %+v", neg)
	}
}

func TestNewValidation(t *testing.T) {
	repoDir := newRepoDir(t)
	sb := sandbox.NewMock(sandbox.MockResponse{})
	client := newScriptedClient()

	if _, err := New(nil, sb, repoDir, Options{}); err == nil {
		t.Error("nil client should error")
	}
	if _, err := New(client, nil, repoDir, Options{}); err == nil {
		t.Error("nil sandbox should error")
	}
	if _, err := New(client, sb, "", Options{}); err == nil {
		t.Error("empty repoDir should error")
	}
}

// --- code-nav tool wiring (bugbot-yc8) -------------------------------------

// TestNewRunner_IncludesCodeNavTools asserts that the tool list passed to the
// runner includes both the read-only baseline tools and the code-nav tools
// from agent.CodeNav. Because Runner does not expose its tool set, we verify
// the composition at the source: readOnlyTools gives the baseline 3, and
// r.nav.Tools() gives the code-nav tools, and newRunner appends them.
func TestNewRunner_IncludesCodeNavTools(t *testing.T) {
	repoDir := newRepoDir(t)
	sb := sandbox.NewMock(sandbox.MockResponse{})
	r, err := New(newScriptedClient(), sb, repoDir, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = r.Close() }()

	// Baseline read-only tools.
	baseline, err := readOnlyTools(repoDir)
	if err != nil {
		t.Fatalf("readOnlyTools: %v", err)
	}
	if len(baseline) != 3 {
		t.Errorf("readOnlyTools: want 3 tools, got %d", len(baseline))
	}
	baselineNames := make(map[string]bool)
	for _, tool := range baseline {
		baselineNames[tool.Def().Name] = true
	}
	for _, want := range []string{"read_file", "list_dir", "grep"} {
		if !baselineNames[want] {
			t.Errorf("readOnlyTools missing %q", want)
		}
	}

	// Code-nav tools from r.nav must include the expected nav tool names.
	navTools := r.nav.Tools()
	if len(navTools) == 0 {
		t.Fatal("r.nav.Tools() returned no tools")
	}
	navNames := make(map[string]bool)
	for _, tool := range navTools {
		navNames[tool.Def().Name] = true
	}
	for _, want := range []string{"find_definition", "find_references", "find_implementations", "read_symbol"} {
		if !navNames[want] {
			t.Errorf("nav tools missing %q", want)
		}
	}

	// newRunner must not fail — it will compose baseline + nav tools.
	if _, err := r.newRunner(ingest.LangGo, nil, progress.AgentScope{}); err != nil {
		t.Fatalf("newRunner: %v", err)
	}
}

// TestReproducerClose_Idempotent verifies that Close is safe to call multiple
// times and on a nil receiver.
func TestReproducerClose_Idempotent(t *testing.T) {
	repoDir := newRepoDir(t)
	sb := sandbox.NewMock(sandbox.MockResponse{})
	r, err := New(newScriptedClient(), sb, repoDir, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := r.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Errorf("second Close (idempotent): %v", err)
	}

	// nil receiver must not panic.
	var nilR *Reproducer
	if err := nilR.Close(); err != nil {
		t.Errorf("nil Close: %v", err)
	}
}

// TestPromoteAll_WritesAgentTranscript proves the TranscriptDir option is
// honored end-to-end: when set, the reproducer agent auto-saves its JSONL
// transcript so an operator can later diagnose why a finding did or did not
// reproduce. The mechanism is language-agnostic — the agent runner saves the
// transcript around its own turns, independent of the plan's target ecosystem;
// the plan fixture here is merely a convenient valid plan.
func TestPromoteAll_WritesAgentTranscript(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	finding := seedFinding(t, st)
	repoDir := newRepoDir(t)
	client := newScriptedClient(planBody(t, goodPlan()))
	sb := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{
		ExitCode: 1,
		Stdout:   "--- FAIL: TestBug\nFAIL",
	}})

	trDir := t.TempDir()
	r, err := New(client, sb, repoDir, Options{ArtifactDir: t.TempDir(), TranscriptDir: trDir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.PromoteAll(ctx, st, []store.Finding{finding}); err != nil {
		t.Fatalf("PromoteAll: %v", err)
	}

	entries, err := os.ReadDir(trDir)
	if err != nil {
		t.Fatal(err)
	}
	jsonl := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			jsonl++
		}
	}
	if jsonl == 0 {
		t.Fatalf("TranscriptDir set but no .jsonl transcript written to %s (entries: %v)", trDir, entries)
	}
}
