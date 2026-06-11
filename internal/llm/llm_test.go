package llm

import "testing"

func TestUsage_ChargeableTokens(t *testing.T) {
	u := Usage{InputTokens: 10000, OutputTokens: 500, CacheReadInputTokens: 8000}
	// weight 1.0 = raw behavior: 10000 + 500
	if got := u.ChargeableTokens(1.0); got != 10500 {
		t.Errorf("weight 1.0 = %d, want 10500", got)
	}
	// weight 0.1: uncached 2000 + 8000*0.1=800 + out 500 = 3300
	if got := u.ChargeableTokens(0.1); got != 3300 {
		t.Errorf("weight 0.1 = %d, want 3300", got)
	}
	// weight 0: all cache reads free: 2000 + 500
	if got := u.ChargeableTokens(0); got != 2500 {
		t.Errorf("weight 0 = %d, want 2500", got)
	}
	// no cache: weight irrelevant
	plain := Usage{InputTokens: 1000, OutputTokens: 100}
	if got := plain.ChargeableTokens(0.1); got != 1100 {
		t.Errorf("no-cache = %d, want 1100", got)
	}
	// defensive: cache reads exceeding input (shouldn't happen) clamp uncached to 0
	weird := Usage{InputTokens: 100, OutputTokens: 0, CacheReadInputTokens: 500}
	if got := weird.ChargeableTokens(0.1); got != 50 {
		t.Errorf("clamp = %d, want 50 (0 uncached + 500*0.1)", got)
	}
}
