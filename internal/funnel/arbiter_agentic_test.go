package funnel

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/agent"
)

// TestArbiterRunnerLimits_LargerThanRefuter pins bugbot-mi5.17 AC1: on a split
// the arbiter runs with a DISTINCT budget materially larger than a refuter's. It
// draws from the same verify pool but is capped at arbiterClaim (~5x verifyClaim)
// and gets DefaultArbiterMaxIterations model turns (vs the refuter default), so a
// single arbiter can drive the split to ground instead of being clipped to a
// one-shot read on a refuter's allowance.
func TestArbiterRunnerLimits_LargerThanRefuter(t *testing.T) {
	st, _ := openFixture(t)
	rec := &spendRecorder{ctx: context.Background(), store: st}
	b := newBudgetState(20_000_000, rec, 1.0)
	b.verifyClaim = DefaultTokenClaim         // 1M
	b.arbiterClaim = DefaultArbiterTokenClaim // 5M
	b.reserveForDownstream(0.5)               // finder 10M, verify 10M (both above the arbiter claim)

	limits := StageLimits{}.resolve()
	verifyTok := b.verifyRunnerLimits(limits.VerifierLimits).TokenBudget
	arb := b.arbiterRunnerLimits(limits.ArbiterLimits)

	if arb.TokenBudget <= verifyTok {
		t.Errorf("arbiter per-run token budget = %d, want strictly > refuter %d (materially larger)", arb.TokenBudget, verifyTok)
	}
	if arb.TokenBudget != DefaultArbiterTokenClaim {
		t.Errorf("arbiter per-run token budget = %d, want %d (claim cap, pool remainder is larger)", arb.TokenBudget, DefaultArbiterTokenClaim)
	}
	if arb.MaxIterations != DefaultArbiterMaxIterations {
		t.Errorf("arbiter MaxIterations = %d, want %d", arb.MaxIterations, DefaultArbiterMaxIterations)
	}
	if arb.MaxIterations <= agent.DefaultMaxIterations {
		t.Errorf("arbiter MaxIterations = %d, want strictly > the refuter default %d", arb.MaxIterations, agent.DefaultMaxIterations)
	}
}

// TestArbiterClaimNeverTouchesRefuter pins AC5 budget isolation: configuring the
// arbiter claim must not change the refuter (verify) per-run budget.
func TestArbiterClaimNeverTouchesRefuter(t *testing.T) {
	st, _ := openFixture(t)
	rec := &spendRecorder{ctx: context.Background(), store: st}
	b := newBudgetState(20_000_000, rec, 1.0)
	b.verifyClaim = DefaultTokenClaim
	b.arbiterClaim = DefaultArbiterTokenClaim
	b.reserveForDownstream(0.5)
	if got := b.verifyRunnerLimits(agent.Limits{}).TokenBudget; got != DefaultTokenClaim {
		t.Errorf("refuter per-run budget = %d, want %d (arbiter claim must not affect it)", got, DefaultTokenClaim)
	}
}

// TestArbiterSchema_RequiresEvidence pins bugbot-mi5.17 AC2: the arbiter MUST
// cite concrete evidence, enforced as a REQUIRED structured field. The refuter
// schema has no evidence field — refuters do not carry the arbiter's grounding
// obligation, and refutationSchema's additionalProperties:false would reject a
// response that included one.
func TestArbiterSchema_RequiresEvidence(t *testing.T) {
	type schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	var arb schema
	if err := json.Unmarshal(arbiterSchema, &arb); err != nil {
		t.Fatalf("arbiterSchema is not valid JSON: %v", err)
	}
	if _, ok := arb.Properties["evidence"]; !ok {
		t.Error("arbiterSchema must declare an 'evidence' property")
	}
	if !slices.Contains(arb.Required, "evidence") {
		t.Errorf("arbiterSchema must REQUIRE 'evidence'; required = %v", arb.Required)
	}

	var ref schema
	if err := json.Unmarshal(refutationSchema, &ref); err != nil {
		t.Fatalf("refutationSchema is not valid JSON: %v", err)
	}
	if slices.Contains(ref.Required, "evidence") {
		t.Error("refutationSchema must NOT require 'evidence' (refuters do not ground like the arbiter)")
	}
	if _, ok := ref.Properties["evidence"]; ok {
		t.Error("refutationSchema must NOT declare 'evidence'")
	}
}

// TestArbiterPrompt_DrivesToGroundNoDebate pins bugbot-mi5.17 AC2/AC3: the
// arbiter prompt directs an AGENTIC drive-to-ground with MANDATORY verification
// of the decisive claim plus evidence citation, advertises its broader read
// reach, and does NOT place the seats in a multi-agent debate.
func TestArbiterPrompt_DrivesToGroundNoDebate(t *testing.T) {
	p := arbiterSystemPrompt("senior Go engineer", false, true)
	for _, must := range []string{"ACTIVE GROUNDING", "DRIVE", "MANDATORY", "evidence", "BROADER READ REACH"} {
		if !strings.Contains(p, must) {
			t.Errorf("arbiter prompt missing required directive %q (bugbot-mi5.17 AC2)", must)
		}
	}
	if strings.Contains(strings.ToLower(p), "debate") {
		t.Error("arbiter prompt must NOT instruct a debate — refuter independence is preserved (bugbot-mi5.17 AC3)")
	}
}

// TestRefuterIndependence_PanelVerdictsArbiterOnly pins bugbot-mi5.17 AC3: a
// refuter's task never includes other seats' verdicts, so no seat sees another
// seat's reasoning; only the arbiter receives the PANEL VERDICTS block.
func TestRefuterIndependence_PanelVerdictsArbiterOnly(t *testing.T) {
	c := Candidate{Lens: "l", File: "f.go", Line: 1, Title: "t", Description: "d", Evidence: "e"}
	if got := verifierTask(c); strings.Contains(got, "PANEL VERDICTS") {
		t.Error("refuter task must NOT contain PANEL VERDICTS — refuters never see other seats (independence)")
	}
	verdicts := []refutation{
		{Refuted: true, Reasoning: "guarded by caller", Confidence: "high"},
		{Refuted: false, Reasoning: "path is reachable", Confidence: "high"},
	}
	at := arbiterTask(c, verdicts, []string{"reachability", "semantics"})
	if !strings.Contains(at, "PANEL VERDICTS") {
		t.Error("arbiter task must contain PANEL VERDICTS — the arbiter is the only cross-seat reader")
	}
}
