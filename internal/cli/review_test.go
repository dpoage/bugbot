package cli

import (
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/funnel"
)

func cfgReview(failOn, suspected string) config.Review {
	return config.Review{FailOn: failOn, Suspected: suspected}
}

// TestReviewGate_NewVerifiedFails: a new T2 finding under fail_on=verified errors.
func TestReviewGate_NewVerifiedFails(t *testing.T) {
	run := reviewRun{
		result:           &funnel.Result{Stats: funnel.Stats{FinderRuns: 5}},
		newVerifiedCount: 1,
	}
	if err := reviewGateError(run, "verified", 3); err == nil {
		t.Fatal("a new verified finding should fail the gate")
	}
}

// TestReviewGate_OnlyPreexistingPasses: zero NEW verified findings (all re-posts)
// passes even though findings were surfaced.
func TestReviewGate_OnlyPreexistingPasses(t *testing.T) {
	run := reviewRun{
		result:           &funnel.Result{Stats: funnel.Stats{FinderRuns: 5}},
		newVerifiedCount: 0,
	}
	if err := reviewGateError(run, "verified", 3); err != nil {
		t.Fatalf("only pre-existing findings must not fail the gate: %v", err)
	}
}

// TestReviewGate_FailOnNone: fail_on=none never fails on findings.
func TestReviewGate_FailOnNone(t *testing.T) {
	run := reviewRun{
		result:           &funnel.Result{Stats: funnel.Stats{FinderRuns: 5}},
		newVerifiedCount: 3,
	}
	if err := reviewGateError(run, "none", 3); err != nil {
		t.Fatalf("fail_on=none must not fail the gate: %v", err)
	}
}

// TestReviewGate_ReliabilityPrecedence: an unreliable run errors regardless of
// fail_on, and the reliability error takes precedence over the findings gate.
func TestReviewGate_ReliabilityPrecedence(t *testing.T) {
	run := reviewRun{
		result:           &funnel.Result{Stats: funnel.Stats{FinderRuns: 6, FinderFailures: 4}},
		newVerifiedCount: 0,
	}
	err := reviewGateError(run, "none", 3)
	if err == nil {
		t.Fatal("most-finders-failed must error even with fail_on=none")
	}
	if !strings.Contains(err.Error(), "unreliable") {
		t.Errorf("expected reliability error, got %v", err)
	}
}

// TestResolveReviewConfig_FlagOverridesConfig: flags win over config; empties
// fall back to config then to documented defaults.
func TestResolveReviewConfig_FlagOverridesConfig(t *testing.T) {
	// Flags override config.
	rc := resolveReviewConfig(cfgReview("verified", "summary"), "none", "withhold")
	if rc.failOn != "none" || rc.suspected != "withhold" {
		t.Errorf("flags should override config: %+v", rc)
	}
	// Empty flags fall back to config.
	rc = resolveReviewConfig(cfgReview("none", "withhold"), "", "")
	if rc.failOn != "none" || rc.suspected != "withhold" {
		t.Errorf("empty flags should fall back to config: %+v", rc)
	}
	// Empty config + empty flags default.
	rc = resolveReviewConfig(cfgReview("", ""), "", "")
	if rc.failOn != "verified" || rc.suspected != "summary" {
		t.Errorf("empty config + flags should default: %+v", rc)
	}
}

// TestValidateReviewFlags rejects unknown values.
func TestValidateReviewFlags(t *testing.T) {
	if err := validateReviewFlags(reviewConfig{failOn: "bogus", suspected: "summary"}); err == nil {
		t.Error("bad fail_on should be rejected")
	}
	if err := validateReviewFlags(reviewConfig{failOn: "verified", suspected: "bogus"}); err == nil {
		t.Error("bad suspected should be rejected")
	}
	if err := validateReviewFlags(reviewConfig{failOn: "none", suspected: "withhold"}); err != nil {
		t.Errorf("valid values should pass: %v", err)
	}
}
