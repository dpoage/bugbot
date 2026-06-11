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
