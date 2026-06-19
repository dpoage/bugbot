package agent

import (
	"testing"

	"github.com/dpoage/bugbot/internal/llm"
)

func TestOverBudget_CacheReadWeighted(t *testing.T) {
	r := &Runner{limits: Limits{TokenBudget: 2000, CacheReadWeight: 0.1}}
	// 5000 input (4500 cached) + 100 out -> chargeable 500 + 450 + 100 = 1050 < 2000.
	u := llm.Usage{InputTokens: 5000, OutputTokens: 100, CacheReadInputTokens: 4500}
	if r.overBudget(u) {
		t.Error("weighted usage 1050 should be under budget 2000")
	}
	// Same usage at raw weight 1.0 -> 5100 > 2000.
	rRaw := &Runner{limits: Limits{TokenBudget: 2000, CacheReadWeight: 1.0}}
	if !rRaw.overBudget(u) {
		t.Error("raw usage 5100 should exceed budget 2000")
	}
}

// TestOutcome_Validate enforces the Truncated→TruncationReason invariant.
func TestOutcome_Validate(t *testing.T) {
	// Clean outcome: no truncation.
	if err := (&Outcome{}).Validate(); err != nil {
		t.Errorf("zero Outcome should be valid, got %v", err)
	}
	// Valid truncation: both fields set.
	if err := (&Outcome{Truncated: true, TruncationReason: TruncMaxIterations}).Validate(); err != nil {
		t.Errorf("Truncated+reason should be valid, got %v", err)
	}
	// Invariant violation: Truncated without reason.
	if err := (&Outcome{Truncated: true}).Validate(); err == nil {
		t.Error("Truncated with empty TruncationReason should fail Validate")
	}
}
