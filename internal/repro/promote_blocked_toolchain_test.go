package repro

import (
	"context"
	"fmt"
	"testing"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/sandbox"
	"github.com/dpoage/bugbot/internal/store"
)

// seedTSFinding inserts a Tier-2 verified finding whose file is TypeScript,
// so ecosystem.InferFromExtension resolves it to the "js" ecosystem — the
// production incident this issue describes (a bazel-only image reproducing a
// TypeScript finding).
func seedTSFinding(t *testing.T, st *store.Store) domain.Finding {
	t.Helper()
	fp := domain.Fingerprint("logic", "src/app.ts", fmt.Sprintf("%d|%s", 7, "off-by-one in pagination"))
	f := domain.Finding{
		Fingerprint: fp,
		Title:       "Off-by-one in pagination",
		Description: "The page offset is computed one index too high.",
		Reasoning:   "Verified: loop bound uses <= instead of <.",
		Severity:    "medium",
		Tier:        2,
		Status:      domain.StatusOpen,
		Lens:        "logic",
		File:        "src/app.ts",
		Line:        7,
		CommitSHA:   "abc123",
		FileHash:    "deadbeef",
	}
	stored, err := st.UpsertFinding(context.Background(), f)
	if err != nil {
		t.Fatalf("seed TS finding: %v", err)
	}
	return stored
}

// nodeUnavailableCaps mirrors what ProbeCapabilities would return for a
// bazel-only image: every ecosystem probed, none of them have node.
func nodeUnavailableCaps() sandbox.CapabilitySet {
	return sandbox.CapabilitySet{
		"js": {"node": false, "node_test": false},
	}
}

// nodeAvailableCaps mirrors what ProbeCapabilities would return once a host
// node toolchain is mounted (bugbot-14g0 fix A).
func nodeAvailableCaps() sandbox.CapabilitySet {
	return sandbox.CapabilitySet{
		"js": {"node": true, "node_test": true},
	}
}

// TestPromoteAll_BlockedToolchain_NoAttemptAggregateReported is regression
// 6(a): a Node-less image + TS finding must be skipped as blocked_toolchain,
// with zero sandbox attempts and the block reflected in Summary's aggregate.
func TestPromoteAll_BlockedToolchain_NoAttemptAggregateReported(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	finding := seedTSFinding(t, st)
	repoDir := newRepoDir(t)

	// No scripted response queued: if the reproducer ever calls the LLM this
	// panics/fails loudly — the gate must never let the plan-agent run.
	client := newScriptedClient()
	sb := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 1}})

	r, err := New(client, sb, repoDir, Options{
		ArtifactDir:  t.TempDir(),
		Capabilities: nodeUnavailableCaps(),
	})
	if err != nil {
		t.Fatal(err)
	}

	summary, err := r.PromoteAll(ctx, st, []domain.Finding{finding})
	if err != nil {
		t.Fatalf("PromoteAll: %v", err)
	}

	if summary.BlockedToolchain != 1 {
		t.Errorf("summary.BlockedToolchain = %d, want 1", summary.BlockedToolchain)
	}
	if summary.Attempted != 0 || summary.Failed != 0 || summary.Promoted != 0 {
		t.Errorf("summary = %+v, want zero attempted/failed/promoted", summary)
	}
	if got := summary.BlockedByEcosystem["js"]; got != 1 {
		t.Errorf("summary.BlockedByEcosystem[js] = %d, want 1", got)
	}
	if len(summary.PerFinding) != 1 || !summary.PerFinding[0].BlockedToolchain {
		t.Fatalf("PerFinding[0] = %+v, want BlockedToolchain=true", summary.PerFinding)
	}
	if summary.PerFinding[0].MissingEcosystem != "js" {
		t.Errorf("MissingEcosystem = %q, want js", summary.PerFinding[0].MissingEcosystem)
	}

	// No sandbox run happened at all (not even a probe re-run — the gate is a
	// zero-container decision against the already-cached CapabilitySet).
	if n := sb.CallCount(); n != 0 {
		t.Errorf("sandbox CallCount = %d, want 0 (blocked findings must not launch the sandbox)", n)
	}

	// The queue row reflects the distinct, retryable state — no attempt was
	// ever claimed.
	ra, err := st.GetReproAttempt(ctx, finding.Fingerprint)
	if err != nil {
		t.Fatalf("GetReproAttempt: %v", err)
	}
	if ra.State != store.ReproStateBlockedToolchain {
		t.Errorf("queue state = %s, want blocked_toolchain", ra.State)
	}
	if ra.BlockedEcosystem != "js" {
		t.Errorf("queue blocked_ecosystem = %q, want js", ra.BlockedEcosystem)
	}
	if ra.AttemptCount != 0 {
		t.Errorf("queue attempt_count = %d, want 0 (blocking is not an attempt)", ra.AttemptCount)
	}
}

// TestPromoteAll_BlockedToolchain_ProceedsOnceCapabilityRestored is regression
// 6(b): the SAME finding, once the capability set reports node available
// (simulating a host-toolchain mount), must proceed to a genuine sandbox
// attempt instead of blocking again.
func TestPromoteAll_BlockedToolchain_ProceedsOnceCapabilityRestored(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	finding := seedTSFinding(t, st)
	repoDir := newRepoDir(t)

	// First cycle: node unavailable, blocks.
	blockedClient := newScriptedClient()
	blockedSb := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 1}})
	r1, err := New(blockedClient, blockedSb, repoDir, Options{
		ArtifactDir:  t.TempDir(),
		Capabilities: nodeUnavailableCaps(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r1.PromoteAll(ctx, st, []domain.Finding{finding}); err != nil {
		t.Fatalf("PromoteAll (blocked cycle): %v", err)
	}
	pre, err := st.GetReproAttempt(ctx, finding.Fingerprint)
	if err != nil {
		t.Fatalf("GetReproAttempt: %v", err)
	}
	if pre.State != store.ReproStateBlockedToolchain {
		t.Fatalf("precondition: queue state = %s, want blocked_toolchain", pre.State)
	}
	// The test file imports the target module (src/app.ts) so the plan passes
	// the bugbot-qb4r static executable-edge gate (ClassifyTargetExecution):
	// this test is about the CAPABILITY gate, and the plan must be otherwise
	// legitimate so the recovered cycle reaches a genuine sandbox attempt.
	plan := Plan{
		Files:  map[string]string{"src/app.test.ts": "import { paginate } from './app';\ntest('pagination', () => { expect(paginate([1, 2], 1)).toEqual([2]); });\n"},
		Cmd:    []string{"npx", "vitest", "run", "src/app.test.ts"},
		Expect: "the off-by-one causes the test to fail",
	}
	client := newScriptedClient(planBody(t, plan))
	sb := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{
		ExitCode: 1,
		Stdout:   " × app.test.ts > pagination (3 ms)\n⎯⎯⎯⎯⎯⎯⎯ Failed Tests 1 ⎯⎯⎯⎯⎯⎯⎯\n FAIL  app.test.ts > pagination\nAssertionError: expected 2 to equal 1\n",
	}})
	r2, err := New(client, sb, repoDir, Options{
		ArtifactDir:  t.TempDir(),
		Capabilities: nodeAvailableCaps(),
	})
	if err != nil {
		t.Fatal(err)
	}

	summary, err := r2.PromoteAll(ctx, st, []domain.Finding{finding})
	if err != nil {
		t.Fatalf("PromoteAll (recovered cycle): %v", err)
	}

	if summary.BlockedToolchain != 0 {
		t.Errorf("summary.BlockedToolchain = %d, want 0 once capability is available", summary.BlockedToolchain)
	}
	if summary.Attempted != 1 || summary.Promoted != 1 {
		t.Fatalf("summary = %+v, want 1 attempted and promoted", summary)
	}
	if n := sb.CallCount(); n == 0 {
		t.Error("sandbox CallCount = 0, want at least 1 — the recovered cycle must actually run the repro")
	}

	got, err := st.GetReproAttempt(ctx, finding.Fingerprint)
	if err != nil {
		t.Fatalf("GetReproAttempt: %v", err)
	}
	if got.State != store.ReproStateDone {
		t.Errorf("queue state = %s, want done", got.State)
	}
	if got.AttemptCount != 1 {
		t.Errorf("queue attempt_count = %d, want 1 (the recovered claim is the first real attempt)", got.AttemptCount)
	}
}

// TestGateEcosystem_NeverGatesUngatedEcosystems verifies a C++ finding, a Go
// finding whose CapabilitySet has no "go" probe data (bugbot-bslx's
// degradation rule — see goAvailable), and findings with a nil
// CapabilitySet are never blocked.
func TestGateEcosystem_NeverGatesUngatedEcosystems(t *testing.T) {
	goFinding := domain.Finding{File: "main.go"}
	if _, blocked := gateEcosystem(goFinding, nodeUnavailableCaps()); blocked {
		t.Error("a Go finding must not be gated when the CapabilitySet has no go probe data (degrade to pre-bslx ungated behavior)")
	}
	cppFinding := domain.Finding{File: "main.cpp"}
	if _, blocked := gateEcosystem(cppFinding, nodeUnavailableCaps()); blocked {
		t.Error("a C++ finding must never be gated (no base-presence probe mode)")
	}
	tsFinding := domain.Finding{File: "app.ts"}
	if _, blocked := gateEcosystem(tsFinding, nil); blocked {
		t.Error("a nil CapabilitySet must never gate (no probe available)")
	}
}

// TestSummarizeBlocked_GroupsByEcosystemZeroContainer verifies SummarizeBlocked
// tallies blocked findings by ecosystem without launching the sandbox or
// touching the store (bugbot-14g0 acceptance 2's zero-container preview).
func TestSummarizeBlocked_GroupsByEcosystemZeroContainer(t *testing.T) {
	repoDir := newRepoDir(t)
	sb := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 1}})
	r, err := New(newScriptedClient(), sb, repoDir, Options{
		ArtifactDir:  t.TempDir(),
		Capabilities: nodeUnavailableCaps(),
	})
	if err != nil {
		t.Fatal(err)
	}

	findings := []domain.Finding{
		{File: "a.ts"},
		{File: "b.tsx"},
		{File: "c.go"}, // no go probe data in nodeUnavailableCaps(): degrades to ungated
	}
	counts := r.SummarizeBlocked(findings)
	if counts["js"] != 2 {
		t.Errorf("counts[js] = %d, want 2", counts["js"])
	}
	if _, ok := counts["go"]; ok {
		t.Errorf("go must not be counted when the CapabilitySet has no go probe data, got %v", counts)
	}
	if n := sb.CallCount(); n != 0 {
		t.Errorf("SummarizeBlocked must not touch the sandbox, got %d calls", n)
	}
}

// TestSummarizeBlocked_NilWhenNothingBlocked verifies the nil (not empty-map)
// zero value when every finding's ecosystem is available or ungated.
func TestSummarizeBlocked_NilWhenNothingBlocked(t *testing.T) {
	repoDir := newRepoDir(t)
	sb := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 1}})
	r, err := New(newScriptedClient(), sb, repoDir, Options{ArtifactDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if counts := r.SummarizeBlocked([]domain.Finding{{File: "a.go"}}); counts != nil {
		t.Errorf("counts = %v, want nil", counts)
	}
}

// seedGoFinding inserts a Tier-2 verified finding whose file is Go, so
// ecosystem.InferFromExtension resolves it to the "go" ecosystem — the
// bugbot-bslx production incident (a host_toolchains image lacking go
// burning a full reproducer attempt on exit 127 "go: not found").
func seedGoFinding(t *testing.T, st *store.Store) domain.Finding {
	t.Helper()
	fp := domain.Fingerprint("logic", "internal/paginate.go", fmt.Sprintf("%d|%s", 12, "off-by-one in pagination"))
	f := domain.Finding{
		Fingerprint: fp,
		Title:       "Off-by-one in pagination",
		Description: "The page offset is computed one index too high.",
		Reasoning:   "Verified: loop bound uses <= instead of <.",
		Severity:    "medium",
		Tier:        2,
		Status:      domain.StatusOpen,
		Lens:        "logic",
		File:        "internal/paginate.go",
		Line:        12,
		CommitSHA:   "abc123",
		FileHash:    "deadbeef",
	}
	stored, err := st.UpsertFinding(context.Background(), f)
	if err != nil {
		t.Fatalf("seed Go finding: %v", err)
	}
	return stored
}

// goUnavailableCaps mirrors what ProbeCapabilities would return for a
// host_toolchains image whose probe RAN and explicitly found no go binary
// (bugbot-bslx's positive-negative signal — the only case that blocks).
func goUnavailableCaps() sandbox.CapabilitySet {
	return sandbox.CapabilitySet{
		"go": {"present": false, "race": false},
	}
}

// goAvailableCaps mirrors what ProbeCapabilities would return once a host go
// toolchain is confirmed present.
func goAvailableCaps() sandbox.CapabilitySet {
	return sandbox.CapabilitySet{
		"go": {"present": true, "race": false},
	}
}

// TestPromoteAll_BlockedToolchain_Go_NoAttemptAggregateReported is the
// bugbot-bslx regression: a go-less image + Go finding must be skipped as
// blocked_toolchain, with zero agent/sandbox activity, mirroring
// TestPromoteAll_BlockedToolchain_NoAttemptAggregateReported for js/node.
func TestPromoteAll_BlockedToolchain_Go_NoAttemptAggregateReported(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	finding := seedGoFinding(t, st)
	repoDir := newRepoDir(t)

	// No scripted response queued: if the reproducer ever calls the LLM this
	// panics/fails loudly — the gate must never let the plan-agent run.
	client := newScriptedClient()
	sb := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 1}})

	r, err := New(client, sb, repoDir, Options{
		ArtifactDir:  t.TempDir(),
		Capabilities: goUnavailableCaps(),
	})
	if err != nil {
		t.Fatal(err)
	}

	summary, err := r.PromoteAll(ctx, st, []domain.Finding{finding})
	if err != nil {
		t.Fatalf("PromoteAll: %v", err)
	}

	if summary.BlockedToolchain != 1 {
		t.Errorf("summary.BlockedToolchain = %d, want 1", summary.BlockedToolchain)
	}
	if summary.Attempted != 0 || summary.Failed != 0 || summary.Promoted != 0 {
		t.Errorf("summary = %+v, want zero attempted/failed/promoted", summary)
	}
	if got := summary.BlockedByEcosystem["go"]; got != 1 {
		t.Errorf("summary.BlockedByEcosystem[go] = %d, want 1", got)
	}
	if len(summary.PerFinding) != 1 || !summary.PerFinding[0].BlockedToolchain {
		t.Fatalf("PerFinding[0] = %+v, want BlockedToolchain=true", summary.PerFinding)
	}
	if summary.PerFinding[0].MissingEcosystem != "go" {
		t.Errorf("MissingEcosystem = %q, want go", summary.PerFinding[0].MissingEcosystem)
	}

	// No sandbox run happened at all — the gate is a zero-container decision
	// against the already-cached CapabilitySet.
	if n := sb.CallCount(); n != 0 {
		t.Errorf("sandbox CallCount = %d, want 0 (blocked findings must not launch the sandbox)", n)
	}

	ra, err := st.GetReproAttempt(ctx, finding.Fingerprint)
	if err != nil {
		t.Fatalf("GetReproAttempt: %v", err)
	}
	if ra.State != store.ReproStateBlockedToolchain {
		t.Errorf("queue state = %s, want blocked_toolchain", ra.State)
	}
	if ra.BlockedEcosystem != "go" {
		t.Errorf("queue blocked_ecosystem = %q, want go", ra.BlockedEcosystem)
	}
	if ra.AttemptCount != 0 {
		t.Errorf("queue attempt_count = %d, want 0 (blocking is not an attempt)", ra.AttemptCount)
	}
}

// TestPromoteAll_BlockedToolchain_Go_ProbeAbsentDegradesToUnblocked pins the
// bugbot-bslx CRITICAL degradation rule: a CapabilitySet that carries no
// "go" entry at all (probe never ran / no probe data — the only kind of
// CapabilitySet that existed before this change) must leave Go findings
// exactly as ungated as they were pre-bslx. Only an explicit "go
// unavailable" probe result (goUnavailableCaps, tested above) may block.
func TestPromoteAll_BlockedToolchain_Go_ProbeAbsentDegradesToUnblocked(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	finding := seedGoFinding(t, st)
	repoDir := newRepoDir(t)

	plan := Plan{
		Files:  map[string]string{"internal/paginate_test.go": "package internal\n\nimport \"testing\"\n\nfunc TestPaginate(t *testing.T) {\n\tif got := paginate([]int{1, 2}, 1); len(got) != 1 {\n\t\tt.Fatalf(\"got %v\", got)\n\t}\n}\n"},
		Cmd:    []string{"go", "test", "-timeout", "60s", "./internal/..."},
		Expect: "the off-by-one causes the test to fail",
	}
	client := newScriptedClient(planBody(t, plan))
	sb := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{
		ExitCode: 1,
		Stdout:   "--- FAIL: TestPaginate (0.00s)\nFAIL\n",
	}})

	r, err := New(client, sb, repoDir, Options{
		ArtifactDir: t.TempDir(),
		// nodeUnavailableCaps has entries for js but none for "go" — this is
		// exactly the "probe never ran for go" shape the degradation rule
		// must tolerate.
		Capabilities: nodeUnavailableCaps(),
	})
	if err != nil {
		t.Fatal(err)
	}

	summary, err := r.PromoteAll(ctx, st, []domain.Finding{finding})
	if err != nil {
		t.Fatalf("PromoteAll: %v", err)
	}

	if summary.BlockedToolchain != 0 {
		t.Errorf("summary.BlockedToolchain = %d, want 0 (no go probe data must degrade to ungated)", summary.BlockedToolchain)
	}
	if summary.Attempted != 1 {
		t.Errorf("summary.Attempted = %d, want 1 (the claim must proceed to a genuine sandbox attempt)", summary.Attempted)
	}
	if n := sb.CallCount(); n == 0 {
		t.Error("sandbox CallCount = 0, want at least 1 — a Go finding with no go probe data must not be blocked")
	}
}

// TestPromoteAll_BlockedToolchain_Go_AvailableProceeds pins the happy path:
// a probe that RAN and confirmed go present must leave the finding fully
// ungated — the claim proceeds to a genuine sandbox attempt.
func TestPromoteAll_BlockedToolchain_Go_AvailableProceeds(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	finding := seedGoFinding(t, st)
	repoDir := newRepoDir(t)

	plan := Plan{
		Files:  map[string]string{"internal/paginate_test.go": "package internal\n\nimport \"testing\"\n\nfunc TestPaginate(t *testing.T) {\n\tif got := paginate([]int{1, 2}, 1); len(got) != 1 {\n\t\tt.Fatalf(\"got %v\", got)\n\t}\n}\n"},
		Cmd:    []string{"go", "test", "-timeout", "60s", "./internal/..."},
		Expect: "the off-by-one causes the test to fail",
	}
	client := newScriptedClient(planBody(t, plan))
	sb := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{
		ExitCode: 1,
		Stdout:   "--- FAIL: TestPaginate (0.00s)\nFAIL\n",
	}})

	r, err := New(client, sb, repoDir, Options{
		ArtifactDir:  t.TempDir(),
		Capabilities: goAvailableCaps(),
	})
	if err != nil {
		t.Fatal(err)
	}

	summary, err := r.PromoteAll(ctx, st, []domain.Finding{finding})
	if err != nil {
		t.Fatalf("PromoteAll: %v", err)
	}

	if summary.BlockedToolchain != 0 {
		t.Errorf("summary.BlockedToolchain = %d, want 0 (go confirmed present must not block)", summary.BlockedToolchain)
	}
	if summary.Attempted != 1 {
		t.Errorf("summary.Attempted = %d, want 1 (available go must proceed to a genuine attempt)", summary.Attempted)
	}
	if n := sb.CallCount(); n == 0 {
		t.Error("sandbox CallCount = 0, want at least 1 — an available-go finding must reach the sandbox")
	}
}
