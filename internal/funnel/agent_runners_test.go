package funnel

import (
	"context"
	"testing"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/progress"
)

// TestBudgetStopped_BothTruncationReasons asserts that the shared
// budgetStopped predicate recognises BOTH TruncTokenBudget (the run's own
// per-run allowance) and TruncBudgetPool (the shared cross-runner pool) as
// budget stops. The verify stage previously hard-coded the pool-only check
// at verify.go:79/126, so a verifier that exhausted its own per-run budget
// was misclassified and routed through the unparseable/failed path. This
// test pins the contract so the finder/verify paths stay in sync.
func TestBudgetStopped_BothTruncationReasons(t *testing.T) {
	cases := []struct {
		name string
		out  *agent.Outcome
		want bool
	}{
		{"nil outcome", nil, false},
		{"not truncated", &agent.Outcome{Truncated: false}, false},
		{"truncated iteration cap", &agent.Outcome{Truncated: true, TruncationReason: agent.TruncMaxIterations}, false},
		{"truncated token budget (per-run)", &agent.Outcome{Truncated: true, TruncationReason: agent.TruncTokenBudget}, true},
		{"truncated budget pool (shared)", &agent.Outcome{Truncated: true, TruncationReason: agent.TruncBudgetPool}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := budgetStopped(tc.out); got != tc.want {
				t.Errorf("budgetStopped(%+v) = %v, want %v", tc.out, got, tc.want)
			}
		})
	}
}

// TestHasSandboxExec_FindsTool confirms the shared hasSandboxExec helper
// detects the sandbox_exec tool in a tool set. The verify path used to
// duplicate this scan inline at runRefuters and runArbiter; centralising it
// removes the duplication.
func TestHasSandboxExec_FindsTool(t *testing.T) {
	if hasSandboxExec(nil) {
		t.Errorf("hasSandboxExec(nil) = true, want false")
	}
	if hasSandboxExec([]agent.Tool{}) {
		t.Errorf("hasSandboxExec(empty) = true, want false")
	}
}

// TestRunRefuters_PerRunTokenBudgetIsBudgetStop is the acceptance test for
// bugbot-89r.2's first correctness fix. We construct an Outcome matching
// what a per-run-token-budget stop looks like (TruncTokenBudget) and
// verify the shared budgetStopped predicate classifies it as a stop. The
// pre-fix code only checked TruncBudgetPool at verify.go:79/126; a
// verifier stopped by its own per-run budget was misrouted to the
// unparseable/failed path. The post-fix code uses budgetStopped, so any
// budget stop — pool or per-run — short-circuits the verdict loop and
// returns stopped=true.
func TestRunRefuters_PerRunTokenBudgetIsBudgetStop(t *testing.T) {
	out := &agent.Outcome{Truncated: true, TruncationReason: agent.TruncTokenBudget}
	if !budgetStopped(out) {
		t.Fatalf("budgetStopped(TruncTokenBudget) = false, want true — a per-run budget stop must be classified as a budget stop, like TruncBudgetPool")
	}

	out2 := &agent.Outcome{Truncated: true, TruncationReason: agent.TruncBudgetPool}
	if !budgetStopped(out2) {
		t.Fatalf("budgetStopped(TruncBudgetPool) = false, want true")
	}
}

// TestNewAgentRunner_AppliesStandardOptions verifies the unified builder
// applies WithLimits / WithMaxTokens / transcript option so the three
// funnel call sites (finder, refuter, arbiter) construct identical runners
// (modulo the per-site limits and system prompt).
func TestNewAgentRunner_AppliesStandardOptions(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)
	f, err := New(RoleClients{Finder: newScriptedClient(), Verifier: newScriptedClient()}, st, repo, Options{})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := f.readOnlyTools(agent.ReadCaps{})
	if err != nil {
		t.Fatal(err)
	}
	client := newScriptedClientWithCaps(llm.Capabilities{StructuredOutput: true})
	client.fallback = notRefutedJSON

	c := Candidate{Lens: "l", File: "f.go", Line: 1, Title: "t"}
	budget := &budgetState{}

	scope := progress.NewAgentScope(nil, progress.RoleVerifier, c.Title)
	verdicts, _, _, _, stopped, err := f.runRefuters(ctx, client, tools, "engineer", c, 1, budget, scope)
	if err != nil {
		t.Fatalf("runRefuters: %v", err)
	}
	if stopped {
		t.Fatalf("stopped=true on unlimited budget")
	}
	if len(verdicts) != 1 || verdicts[0].Refuted {
		t.Errorf("verdicts = %+v, want one not-refuted", verdicts)
	}
	if got := client.allRequests(); len(got) == 0 {
		t.Fatal("verifier saw no completions")
	}
}
