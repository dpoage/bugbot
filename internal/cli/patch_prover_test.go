package cli

import "testing"

// TestTierLabel_T0 confirms tierLabel returns the correct label for tier 0.
func TestTierLabel_T0(t *testing.T) {
	got := tierLabel(0)
	want := "T0 Fix-witnessed"
	if got != want {
		t.Errorf("tierLabel(0) = %q, want %q", got, want)
	}
}

// TestTierLabel_ExistingTiers confirms existing tier labels are unchanged by
// the addition of tier 0.
func TestTierLabel_ExistingTiers(t *testing.T) {
	cases := []struct {
		tier int
		want string
	}{
		{1, "T1 Reproduced"},
		{2, "T2 Verified"},
		{3, "T3 Suspected"},
	}
	for _, tc := range cases {
		got := tierLabel(tc.tier)
		if got != tc.want {
			t.Errorf("tierLabel(%d) = %q, want %q", tc.tier, got, tc.want)
		}
	}
}
