package cli

import (
	"testing"

	"github.com/dpoage/bugbot/internal/domain"
)

// TestTierLabel_T0 confirms domain.Tier.Label() returns the correct label for tier 0.
func TestTierLabel_T0(t *testing.T) {
	got := domain.TierFixWitnessed.Label()
	want := "T0 Fix-witnessed"
	if got != want {
		t.Errorf("TierFixWitnessed.Label() = %q, want %q", got, want)
	}
}

// TestTierLabel_ExistingTiers confirms existing tier labels are unchanged.
func TestTierLabel_ExistingTiers(t *testing.T) {
	cases := []struct {
		tier domain.Tier
		want string
	}{
		{domain.TierReproduced, "T1 Reproduced"},
		{domain.TierVerified, "T2 Verified"},
		{domain.TierSuspected, "T3 Suspected"},
	}
	for _, tc := range cases {
		got := tc.tier.Label()
		if got != tc.want {
			t.Errorf("Tier(%d).Label() = %q, want %q", tc.tier, got, tc.want)
		}
	}
}
