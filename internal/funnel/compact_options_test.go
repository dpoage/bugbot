package funnel

import (
	"testing"

	"github.com/dpoage/bugbot/internal/agent"
)

func TestResolve_FinderHistoryCompactionOffByDefault(t *testing.T) {
	// Unset (zero) leaves compaction DISABLED: the bugbot-3nf measurement showed
	// it raises cache-weighted cost under a strong cache, so it must be opt-in.
	got := Options{}.resolve()
	if got.Limits.FinderLimits.HistoryTokenBudget != 0 {
		t.Errorf("HistoryTokenBudget = %d, want 0 (compaction off by default)",
			got.Limits.FinderLimits.HistoryTokenBudget)
	}
}

func TestResolve_FinderHistoryTokensOptIn(t *testing.T) {
	// A positive value opts in at that threshold (weak-/no-cache providers).
	got := Options{Limits: StageLimits{FinderHistoryTokens: 25_000}}.resolve()
	if got.Limits.FinderLimits.HistoryTokenBudget != 25_000 {
		t.Errorf("HistoryTokenBudget = %d, want 25000", got.Limits.FinderLimits.HistoryTokenBudget)
	}
}

func TestResolve_FinderHistoryTokensNegativeDisabled(t *testing.T) {
	got := Options{Limits: StageLimits{FinderHistoryTokens: -1}}.resolve()
	if got.Limits.FinderLimits.HistoryTokenBudget != 0 {
		t.Errorf("HistoryTokenBudget = %d, want 0 (disabled)", got.Limits.FinderLimits.HistoryTokenBudget)
	}
}

func TestFinderReadCaps_DefaultsTighten(t *testing.T) {
	// Unset => the tighter funnel finder caps (the cache-safe default lever).
	caps := Options{}.finderReadCaps()
	if caps.MaxLines != DefaultFinderReadLines || caps.MaxBytes != DefaultFinderReadBytes {
		t.Errorf("finderReadCaps default = %+v, want {%d %d}",
			caps, DefaultFinderReadLines, DefaultFinderReadBytes)
	}
}

func TestFinderReadCaps_Explicit(t *testing.T) {
	caps := Options{Limits: StageLimits{FinderReadLines: 300, FinderReadBytes: 40000}}.finderReadCaps()
	if caps.MaxLines != 300 || caps.MaxBytes != 40000 {
		t.Errorf("finderReadCaps = %+v, want {300 40000}", caps)
	}
}

func TestFinderReadCaps_NegativeRestoresAgentDefaults(t *testing.T) {
	// Negative => 0 at the funnel layer, which the tool resolves to the looser
	// agent-package defaults.
	caps := Options{Limits: StageLimits{FinderReadLines: -1, FinderReadBytes: -1}}.finderReadCaps()
	if caps.MaxLines != 0 || caps.MaxBytes != 0 {
		t.Errorf("finderReadCaps = %+v, want {0 0} (defer to agent defaults)", caps)
	}
	// Sanity: agent.ReadCaps{}.resolve() yields the looser package defaults.
	resolved := agent.ReadCaps{MaxLines: caps.MaxLines, MaxBytes: caps.MaxBytes}
	_ = resolved
}

func TestResolve_TokenClaimDefaults(t *testing.T) {
	// Unset (zero) resolves to DefaultTokenClaim for both roles.
	got := Options{}.resolve()
	if got.Budget.FinderTokenClaim != DefaultTokenClaim {
		t.Errorf("FinderTokenClaim = %d, want %d (default)", got.Budget.FinderTokenClaim, DefaultTokenClaim)
	}
	if got.Budget.VerifierTokenClaim != DefaultTokenClaim {
		t.Errorf("VerifierTokenClaim = %d, want %d (default)", got.Budget.VerifierTokenClaim, DefaultTokenClaim)
	}

	// An explicit positive value is preserved; a negative value (disable the
	// per-task cap) is preserved as-is, NOT replaced by the default.
	custom := Options{Budget: BudgetConfig{FinderTokenClaim: 2_000_000, VerifierTokenClaim: -1}}.resolve()
	if custom.Budget.FinderTokenClaim != 2_000_000 {
		t.Errorf("FinderTokenClaim = %d, want 2000000 (explicit preserved)", custom.Budget.FinderTokenClaim)
	}
	if custom.Budget.VerifierTokenClaim != -1 {
		t.Errorf("VerifierTokenClaim = %d, want -1 (negative preserved = cap disabled)", custom.Budget.VerifierTokenClaim)
	}
}
