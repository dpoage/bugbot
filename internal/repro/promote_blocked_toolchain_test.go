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
	plan := Plan{
		Files:  map[string]string{"app.test.ts": "test('pagination', () => { throw new Error('boom'); });\n"},
		Cmd:    []string{"npx", "vitest", "run", "app.test.ts"},
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

// TestGateEcosystem_NeverGatesUngatedEcosystems verifies Go/C++ findings and
// findings with a nil CapabilitySet are never blocked, matching
// ecosystem.BaseMode's documented ungated set.
func TestGateEcosystem_NeverGatesUngatedEcosystems(t *testing.T) {
	goFinding := domain.Finding{File: "main.go"}
	if _, blocked := gateEcosystem(goFinding, nodeUnavailableCaps()); blocked {
		t.Error("a Go finding must never be gated (no base-presence probe mode)")
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
		{File: "c.go"}, // ungated: must not appear
	}
	counts := r.SummarizeBlocked(findings)
	if counts["js"] != 2 {
		t.Errorf("counts[js] = %d, want 2", counts["js"])
	}
	if _, ok := counts["go"]; ok {
		t.Errorf("go must never be gated, got %v", counts)
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
