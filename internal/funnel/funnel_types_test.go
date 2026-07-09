package funnel

import "testing"

// TestStats_DuplicateRate pins the definition (bugbot-ezmx.8): the fraction
// of hypothesized candidates triage identified as a duplicate of another
// candidate this run — exact-fingerprint drops plus every merge-collapsed
// non-primary (within-lens, cross-lens, root-cause) — over Hypothesized.
func TestStats_DuplicateRate(t *testing.T) {
	cases := []struct {
		name string
		s    Stats
		want float64
	}{
		{"zero hypothesized is zero rate, not NaN/Inf", Stats{}, 0},
		{
			name: "no duplicates",
			s:    Stats{Hypothesized: 10, Triaged: 10},
			want: 0,
		},
		{
			name: "exact-fingerprint duplicates only",
			s:    Stats{Hypothesized: 10, DroppedDuplicate: 3},
			want: 0.3,
		},
		{
			name: "merges count too, exact-dup and merges combine",
			s: Stats{
				Hypothesized: 20, DroppedDuplicate: 2,
				MergedWithinLens: 3, MergedCrossLens: 1, MergedRootCause: 4,
			},
			want: 10.0 / 20.0,
		},
		{
			name: "every candidate a duplicate is rate 1.0",
			s:    Stats{Hypothesized: 4, DroppedDuplicate: 4},
			want: 1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.s.DuplicateRate(); got != c.want {
				t.Errorf("DuplicateRate() = %v, want %v", got, c.want)
			}
		})
	}
}
