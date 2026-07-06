package repro

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/domain"
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
		Cmd:    []string{"go", "test", "-timeout", "60s", "-run", "TestBug", "./..."},
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
func seedFinding(t *testing.T, st *store.Store) domain.Finding {
	t.Helper()
	fp := domain.Fingerprint("logic", "calc.go", fmt.Sprintf("%d|%s", 12, "Divide ignores zero divisor"))
	f := domain.Finding{
		Fingerprint: fp,
		Title:       "Divide ignores zero divisor",
		Description: "Divide returns 0 for a zero divisor instead of an error.",
		Reasoning:   "Verified: no zero check before the division.",
		Severity:    "high",
		Tier:        2,
		Status:      domain.StatusOpen,
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

	summary, err := r.PromoteAll(ctx, st, []domain.Finding{finding})
	if err != nil {
		t.Fatalf("PromoteAll: %v", err)
	}

	if summary.Promoted != 1 || summary.Failed != 0 || summary.Attempted != 1 {
		t.Fatalf("summary = %+v, want 1 promoted", summary)
	}

	// Repro must NOT force a network: it inherits the sandbox's configured
	// default (sandbox.network) so a repo whose build fetches deps at configure
	// time can resolve them. The mock applies no default, so the recorded spec
	// carries the empty "inherit" value.
	calls := sb.Calls()
	if len(calls) != 1 {
		t.Fatalf("sandbox calls = %d, want 1", len(calls))
	}
	spec := calls[0].Spec
	if spec.Network != "" {
		t.Errorf("network = %q, want empty (inherit sandbox default)", spec.Network)
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
	body := `{"files":{"bug_test.go":"package bug\n\nimport \"testing\"\n\nfunc TestBug(t *testing.T){ t.Fatal(\"boom\") }\n"},"cmd":["go","test","-timeout","60s","-run","TestBug","./..."]}`
	client := newScriptedClient(body)
	sb := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{
		ExitCode: 1,
		Stdout:   "--- FAIL: TestBug\n    bug_test.go:1: boom\nFAIL",
	}})

	r, err := New(client, sb, repoDir, Options{ArtifactDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	summary, err := r.PromoteAll(ctx, st, []domain.Finding{finding})
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

	summary, err := r.PromoteAll(ctx, st, []domain.Finding{finding})
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

	summary, err := r.PromoteAll(ctx, st, []domain.Finding{finding})
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
	if _, err := r.PromoteAll(ctx, st, []domain.Finding{finding}); err != nil {
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
	if _, err := r.PromoteAll(ctx, st, []domain.Finding{finding}); err != nil {
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

	if _, err := r.PromoteAll(ctx, st, []domain.Finding{finding}); err != nil {
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

	summary, err := r.PromoteAll(ctx, st, []domain.Finding{finding})
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

	summary, err := r.PromoteAll(ctx, st, []domain.Finding{finding})
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

	summary, err := r.PromoteAll(ctx, st, []domain.Finding{finding})
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
	if _, err := r.newRunner(context.Background(), ingest.LangGo, nil, progress.AgentScope{}, &iterationWorkspace{}); err != nil {
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
	if _, err := r.PromoteAll(ctx, st, []domain.Finding{finding}); err != nil {
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

// TestBuildTask_IncludesPackageSummary verifies the reproducer task carries the
// finding's package summary (the cartographer handoff) only when one is present,
// labeled with the package directory.
func TestBuildTask_IncludesPackageSummary(t *testing.T) {
	f := domain.Finding{Title: "T", File: "engine/common/memory/X.hpp", Line: 5, Description: "d"}

	with := buildTask(f, "MEMORY-PKG-SUMMARY", "")
	if !strings.Contains(with, "MEMORY-PKG-SUMMARY") {
		t.Error("task must include the pushed package summary")
	}
	if !strings.Contains(with, "engine/common/memory") {
		t.Error("task must label the package context with the package directory")
	}

	if without := buildTask(f, "", ""); strings.Contains(without, "Package context") {
		t.Error("empty summary must not add a package context section")
	}
}

// TestPromoteAll_PushesPackageSummary verifies the provider is consulted for the
// finding's package directory and that the summary reaches the model's task
// prompt through Attempt -> planFor -> buildTask.
func TestPromoteAll_PushesPackageSummary(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	f, err := st.UpsertFinding(ctx, domain.Finding{
		Fingerprint: domain.Fingerprint("logic", "pkg/calc.go", fmt.Sprintf("%d|%s", 12, "bug")),
		Title:       "bug",
		Severity:    "high",
		Tier:        2,
		Status:      domain.StatusOpen,
		Lens:        "logic",
		File:        "pkg/calc.go",
		Line:        12,
		CommitSHA:   "abc",
		FileHash:    "def",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	repoDir := newRepoDir(t)

	var gotPkg string
	client := newScriptedClient(planBody(t, goodPlan()))
	sb := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{
		ExitCode: 1,
		Stdout:   "--- FAIL: TestBug\nFAIL",
	}})
	r, err := New(client, sb, repoDir, Options{
		ArtifactDir: t.TempDir(),
		PackageSummary: func(_ context.Context, pkg string) (string, bool) {
			gotPkg = pkg
			return "CALC-PKG-SUMMARY", true
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.PromoteAll(ctx, st, []domain.Finding{f}); err != nil {
		t.Fatalf("PromoteAll: %v", err)
	}
	if gotPkg != "pkg" {
		t.Errorf("provider queried pkg %q, want %q (dir of finding.File)", gotPkg, "pkg")
	}
	if task := client.taskText(0); !strings.Contains(task, "CALC-PKG-SUMMARY") {
		t.Errorf("model task prompt missing pushed package summary; got:\n%s", task)
	}

	// The get_package_context tool and its guidance must also be wired when a
	// provider is set (covers newRunner's pkgSummary != nil branch). The first
	// recorded request (the plan turn, before any forced finalization) carries
	// both the system prompt and the tool defs.
	reqs := client.allRequests()
	if len(reqs) == 0 {
		t.Fatal("no requests recorded")
	}
	if !strings.Contains(reqs[0].System, "get_package_context") {
		t.Error("system prompt missing package-context guidance")
	}
	hasTool := false
	for _, td := range reqs[0].Tools {
		if td.Name == "get_package_context" {
			hasTool = true
			break
		}
	}
	if !hasTool {
		t.Error("get_package_context tool not registered in the runner")
	}
	if !strings.Contains(reqs[0].System, "bash -c") {
		t.Error("system prompt missing sandbox/command-hygiene guidance")
	}
}

// TestPromoteAll_WiresTimeout verifies Options.Timeout reaches the sandbox Spec
// (RC4). A regression dropping the field silently reverts to the 90s default.
func TestPromoteAll_WiresTimeout(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	finding := seedFinding(t, st)
	repoDir := newRepoDir(t)

	client := newScriptedClient(planBody(t, goodPlan()))
	sb := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{
		ExitCode: 1,
		Stdout:   "--- FAIL: TestBug\nFAIL",
	}})
	want := 1234 * time.Second
	r, err := New(client, sb, repoDir, Options{ArtifactDir: t.TempDir(), Timeout: want})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.PromoteAll(ctx, st, []domain.Finding{finding}); err != nil {
		t.Fatalf("PromoteAll: %v", err)
	}
	calls := sb.Calls()
	if len(calls) != 1 {
		t.Fatalf("sandbox calls = %d, want 1", len(calls))
	}
	if got := calls[0].Spec.Timeout; got != want {
		t.Errorf("Spec.Timeout = %v, want %v", got, want)
	}
}

// TestReproSandboxGuidance covers the always-appended sandbox-environment and
// command-hygiene section: the hygiene rules are present, configured bind mounts
// are listed by container path, and no mount list appears when there are none.
func TestReproSandboxGuidance(t *testing.T) {
	withMount := reproSandboxGuidance([]sandbox.ROMount{{HostPath: "/h/vendor", ContainerPath: "/vendor"}})
	if !strings.Contains(withMount, "bash -c") {
		t.Error("guidance missing command-hygiene rules")
	}
	if !strings.Contains(withMount, "/vendor") {
		t.Error("guidance must list bind-mount container paths")
	}
	if strings.Contains(reproSandboxGuidance(nil), "bind-mounted") {
		t.Error("no mounts must produce no mount list")
	}
}

// TestPromoteAll_SurfacesMountsInPrompt verifies an operator bind mount threads
// through New -> newRunner -> reproSandboxGuidance(r.deps.ROMounts) into the
// model's system prompt, so the agent is told the mount's container path. Guards
// against passing the wrong slice (e.g. r.opts.LocalMounts) or dropping the call.
func TestPromoteAll_SurfacesMountsInPrompt(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	finding := seedFinding(t, st)
	repoDir := newRepoDir(t)

	client := newScriptedClient(planBody(t, goodPlan()))
	sb := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{
		ExitCode: 1,
		Stdout:   "--- FAIL: TestBug\nFAIL",
	}})
	r, err := New(client, sb, repoDir, Options{
		ArtifactDir: t.TempDir(),
		LocalMounts: []sandbox.ROMount{{HostPath: t.TempDir(), ContainerPath: "/vendor-xyz", Shared: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.PromoteAll(ctx, st, []domain.Finding{finding}); err != nil {
		t.Fatalf("PromoteAll: %v", err)
	}
	reqs := client.allRequests()
	if len(reqs) == 0 {
		t.Fatal("no requests recorded")
	}
	if !strings.Contains(reqs[0].System, "/vendor-xyz") {
		t.Errorf("system prompt missing bind-mount container path; got:\n%s", reqs[0].System)
	}
}

// TestValidatePlan_ShellOps covers the structural gate that prevents the
// reproducer agent from emitting shell syntax the sandbox cannot interpret.
// Bare shell control operators as standalone argv elements (e.g. "&&", "|",
// "cd") are passed as literal arguments to the preceding command by the
// raw-argv sandbox runner, which both wastes a sandbox execution AND hides
// the real syntax problem behind a confusing "Unknown argument" error.
// validatePlan must reject these so the agent is fed corrective guidance
// instead. A correctly bash-wrapped plan (operators live INSIDE one quoted
// argv element) must NOT be flagged — we match whole argv elements only.
func TestValidatePlan_ShellOps(t *testing.T) {
	files := map[string]string{"repro_test.go": "package bug\n"}

	// --- accepted: well-formed plans must NOT be flagged ---------------
	accepted := []struct {
		name string
		cmd  []string
	}{
		{"plain go test", []string{"go", "test", "-timeout", "60s", "./..."}},
		{"plain go test with -run", []string{"go", "test", "-timeout", "60s", "-run", "TestX", "./..."}},
		{"plain ctest", []string{"ctest", "--output-on-failure"}},
		{"plain cargo", []string{"cargo", "test"}},
		// The shell-op example from the bug report, correctly wrapped: the
		// operators live inside ONE quoted argv element, so the whole plan
		// is just ["bash","-c","<... && ...>"] — no bare operators.
		{"bash -c with && and cd", []string{
			"bash", "-c",
			"cmake -B build -S . && cmake --build build && cd build && ./tests/x",
		}},
		{"bash -lc with operators inside string", []string{
			"bash", "-lc", "cmake -B build | tee log && cmake --build build",
		}},
		// Operators appearing inside a longer string argument that is NOT a
		// bare operator itself must not trip the check.
		{"arg contains && but is not bare", []string{"echo", "foo && bar"}},
		{"arg contains ; but is not bare", []string{"echo", "a;b"}},
		{"arg contains cd as substring", []string{"echo", "cd-build"}},
	}
	for _, tc := range accepted {
		t.Run("accept/"+tc.name, func(t *testing.T) {
			p := &Plan{Files: files, Cmd: tc.cmd, Expect: "x"}
			if err := validatePlan(p, ""); err != nil {
				t.Errorf("validatePlan(%v) = %v, want nil (a correctly formed plan must not be rejected)", tc.cmd, err)
			}
		})
	}

	// --- rejected: each spec-listed bare operator must be flagged --------
	rejected := []struct {
		name      string
		cmd       []string
		wantOp    string
		wantGuide string
	}{
		{
			// The exact reproducer plan from the bug report.
			name: "cmake && cmake --build && cd build && ./tests",
			cmd: []string{
				"cmake", "-B", "build", "-S", ".",
				"&&", "cmake", "--build", "build",
				"&&", "cd", "build",
				"&&", "./tests/x",
			},
			wantOp:    "&&",
			wantGuide: "bash",
		},
		{"bare &&", []string{"echo", "a", "&&", "echo", "b"}, "&&", "bash"},
		{"bare ||", []string{"false", "||", "true"}, "||", "bash"},
		{"bare |", []string{"echo", "a", "|", "cat"}, "|", "bash"},
		{"bare ;", []string{"echo", "a", ";", "echo", "b"}, ";", "bash"},
		{"bare 2>&1", []string{"echo", "a", "2>&1", "cat"}, "2>&1", "bash"},
		{"bare >", []string{"echo", "a", ">", "out.txt"}, ">", "bash"},
		{"bare >>", []string{"echo", "a", ">>", "out.txt"}, ">>", "bash"},
		{"bare <", []string{"cat", "<", "in.txt"}, "<", "bash"},
		{"bare &", []string{"echo", "a", "&", "echo", "b"}, "&", "bash"},
		{"bare cd", []string{"cd", "build", "&&", "ctest"}, "cd", "bash"},
	}
	for _, tc := range rejected {
		t.Run("reject/"+tc.name, func(t *testing.T) {
			p := &Plan{Files: files, Cmd: tc.cmd, Expect: "x"}
			err := validatePlan(p, "")
			if err == nil {
				t.Fatalf("validatePlan(%v) = nil, want error containing %q", tc.cmd, tc.wantOp)
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.wantOp) {
				t.Errorf("error %q does not name offending bare operator %q", msg, tc.wantOp)
			}
			if !strings.Contains(msg, tc.wantGuide) {
				t.Errorf("error %q does not include wrapping guidance (%q)", msg, tc.wantGuide)
			}
			// The fix must be concrete: show the ["bash","-c","<...>"]
			// argv shape the agent must emit.
			if !strings.Contains(msg, `"bash"`) || !strings.Contains(msg, `"-c"`) {
				t.Errorf("error %q does not show the bash -c argv shape", msg)
			}
		})
	}
}

// TestValidatePlan_GoTestRequiresTimeout covers the bugbot-opq guardrail: a
// go test repro must carry a -timeout flag so a hung test self-terminates
// before the sandbox idle watchdog kills it. The rule is scoped to go test;
// other commands (and the bash -c wrapped form) are handled too.
func TestValidatePlan_GoTestRequiresTimeout(t *testing.T) {
	files := map[string]string{"bug_test.go": "package bug\n"}

	accepted := []struct {
		name string
		cmd  []string
	}{
		{"go test with -timeout", []string{"go", "test", "-timeout", "60s", "-run", "TestX", "./..."}},
		{"go test with -timeout=val", []string{"go", "test", "-timeout=30s", "./..."}},
		{"bash -c wrapped go test with -timeout", []string{"bash", "-c", "go test -timeout 60s -run TestX ./..."}},
		{"non-go pytest needs no -timeout", []string{"pytest", "-k", "test_x"}},
		{"bare binary needs no -timeout", []string{"./repro"}},
		{"go build is not a test run", []string{"go", "build", "./..."}},
	}
	for _, tc := range accepted {
		t.Run("accept/"+tc.name, func(t *testing.T) {
			p := &Plan{Files: files, Cmd: tc.cmd, Expect: "x"}
			if err := validatePlan(p, ""); err != nil {
				t.Errorf("validatePlan(%v) = %v, want nil", tc.cmd, err)
			}
		})
	}

	rejected := []struct {
		name string
		cmd  []string
	}{
		{"go test missing -timeout", []string{"go", "test", "-run", "TestX", "./..."}},
		{"plain go test missing -timeout", []string{"go", "test", "./..."}},
		{"bash -c wrapped go test missing -timeout", []string{"bash", "-c", "go test -run TestX ./..."}},
	}
	for _, tc := range rejected {
		t.Run("reject/"+tc.name, func(t *testing.T) {
			p := &Plan{Files: files, Cmd: tc.cmd, Expect: "x"}
			err := validatePlan(p, "")
			if err == nil {
				t.Fatalf("validatePlan(%v) = nil, want a -timeout error", tc.cmd)
			}
			if !strings.Contains(err.Error(), "-timeout") {
				t.Errorf("error %q does not mention -timeout", err.Error())
			}
		})
	}
}

// TestPromoteAll_BareShellOpPlanRetries proves the recoverable-revision flow:
// an agent that emits ["cmake", ..., "&&", "cmake", "--build", ..., "&&",
// "cd", "build", "&&", "./tests/x"] is caught by validatePlan and fed back
// to the model as actionable bash-wrapping guidance. The bad plan must never
// reach the sandbox; the corrected plan promotes. Regression: such a plan
// previously burned a sandbox execution on the first cmake and surfaced a
// confusing "Unknown argument" error instead of a revision request.
func TestPromoteAll_BareShellOpPlanRetries(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	finding := seedFinding(t, st)
	repoDir := newRepoDir(t)

	bad := Plan{
		Files: map[string]string{"repro_test.go": "package bug\n"},
		// Reproduces the JSON-finding command shape from the bug report.
		Cmd: []string{
			"cmake", "-B", "build", "-S", ".",
			"&&", "cmake", "--build", "build",
			"&&", "cd", "build",
			"&&", "./tests/x",
		},
		Expect: "leak", // non-empty so the plan clears the schema; the cmd is the defect
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

	summary, err := r.PromoteAll(ctx, st, []domain.Finding{finding})
	if err != nil {
		t.Fatalf("PromoteAll returned a hard error for a recoverable bad cmd: %v", err)
	}
	if summary.Promoted != 1 || summary.Failed != 0 {
		t.Fatalf("summary = %+v, want 1 promoted / 0 failed (retry after bare shell op)", summary)
	}
	// The bare-shell-op plan must never reach the sandbox; only the corrected
	// (bash-wrapped) plan executes. Load-bearing: it proves the bad plan does
	// not waste a sandbox run.
	if n := len(sb.Calls()); n != 1 {
		t.Fatalf("sandbox calls = %d, want 1 (bare-shell-op plan must not execute)", n)
	}
	// Two completions: the rejected plan + the corrected one. The revision
	// request must name the offending operator and the bash-wrapping fix.
	reqs := client.allRequests()
	if len(reqs) != 2 {
		t.Fatalf("llm completions = %d, want 2 (reject + revise)", len(reqs))
	}
	task := client.taskText(1)
	for _, want := range []string{"&&", "bash", "-c"} {
		if !strings.Contains(task, want) {
			t.Errorf("revision task missing %q; got:\n%s", want, task)
		}
	}
}

// TestValidatePlan_FilesCollisionCheck verifies the bugbot-ndlw guard: a plan
// whose Files key matches an existing file in repoDir is rejected before any
// sandbox run with corrective feedback naming the colliding path. Plans that
// only add new files are unaffected.
func TestValidatePlan_FilesCollisionCheck(t *testing.T) {
	// Set up a real directory that mimics a repo with a few existing files.
	repoDir := t.TempDir()
	existingFiles := []string{
		"go.mod",
		"pkg/calc.go",
		"internal/helper/util.go",
	}
	for _, f := range existingFiles {
		full := filepath.Join(repoDir, f)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte("// existing\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}

	validCmd := []string{"go", "test", "-timeout", "60s", "-run", "TestBug", "./..."}

	t.Run("reject/overwrites go.mod", func(t *testing.T) {
		p := &Plan{
			Files: map[string]string{"go.mod": "module evil\n"},
			Cmd:   validCmd,
		}
		err := validatePlan(p, repoDir)
		if err == nil {
			t.Fatal("validatePlan must reject a plan that overwrites go.mod")
		}
		if !strings.Contains(err.Error(), "go.mod") {
			t.Errorf("error %q must name the colliding path 'go.mod'", err.Error())
		}
		if !strings.Contains(err.Error(), "NEW files only") {
			t.Errorf("error %q must say 'NEW files only'", err.Error())
		}
	})

	t.Run("reject/overwrites existing source", func(t *testing.T) {
		p := &Plan{
			Files: map[string]string{
				"repro_test.go":           "package bug\n",
				"internal/helper/util.go": "// overwrite\n",
			},
			Cmd: validCmd,
		}
		err := validatePlan(p, repoDir)
		if err == nil {
			t.Fatal("validatePlan must reject a plan overwriting an existing source file")
		}
		if !strings.Contains(err.Error(), "internal/helper/util.go") {
			t.Errorf("error %q must name the colliding path", err.Error())
		}
	})

	t.Run("accept/all new files", func(t *testing.T) {
		p := &Plan{
			Files: map[string]string{
				"repro_test.go":    "package bug\n",
				"testdata/in.json": `{"x":1}`,
			},
			Cmd: validCmd,
		}
		if err := validatePlan(p, repoDir); err != nil {
			t.Errorf("validatePlan rejected a plan with only new files: %v", err)
		}
	})

	t.Run("accept/empty repoDir skips check", func(t *testing.T) {
		// When repoDir is empty the collision check must be skipped entirely,
		// so existing unit tests that pass "" are unaffected.
		p := &Plan{
			Files: map[string]string{"go.mod": "module x\n"},
			Cmd:   validCmd,
		}
		if err := validatePlan(p, ""); err != nil {
			t.Errorf("validatePlan with empty repoDir must not check collisions: %v", err)
		}
	})
}
