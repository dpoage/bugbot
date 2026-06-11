package repro

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	cases := []struct {
		name   string
		res    sandbox.Result
		reason string
	}{
		{"runtime error 125", sandbox.Result{ExitCode: 125, Stderr: "podman: error"}, "environment_error"},
		{"not executable 126", sandbox.Result{ExitCode: 126, Stderr: "permission denied"}, "environment_error"},
		{"not found 127", sandbox.Result{ExitCode: 127, Stderr: "sh: gotest: not found"}, "environment_error"},
		// The real-world case this guard exists for: read-only root broke the
		// Go build cache, exit 1 in 0.13s, and got promoted to Tier 1.
		{"go build cache", sandbox.Result{ExitCode: 1, Stderr: "failed to initialize build cache at /root/.cache/go-build: mkdir /root/.cache: read-only file system"}, "environment_error"},
		{"read-only fs", sandbox.Result{ExitCode: 1, Stderr: "mkdir /data: Read-only file system"}, "environment_error"},
		{"disk full", sandbox.Result{ExitCode: 1, Stderr: "write /tmp/x: no space left on device"}, "environment_error"},
		{"timeout", sandbox.Result{ExitCode: -1, TimedOut: true}, "timeout"},
		{"exit zero", sandbox.Result{ExitCode: 0, Stdout: "ok"}, "exit_zero"},
		{"compile error", sandbox.Result{ExitCode: 2, Stderr: "./x_test.go:3:1: syntax error"}, "build_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := interpret(tc.res)
			if v.demonstrated {
				t.Fatalf("interpret(%+v) demonstrated=true; must never demonstrate", tc.res)
			}
			if v.reason != tc.reason {
				t.Errorf("reason = %q, want %q", v.reason, tc.reason)
			}
		})
	}

	// A genuine test failure still demonstrates.
	v := interpret(sandbox.Result{ExitCode: 1, Stdout: "--- FAIL: TestDivide (0.00s)\n    calc_test.go:6: bug\nFAIL"})
	if !v.demonstrated {
		t.Fatalf("genuine test failure must demonstrate; got reason=%q", v.reason)
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
