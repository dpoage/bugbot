package funnel

import "testing"

// TestStats_DuplicateRate pins the definition (bugbot-ezmx.8): the fraction
// of the candidate pool that entered triage this run — Hypothesized (fresh
// finder output) plus Resumed (WAL-replayed pending candidates) — that
// triage identified as a duplicate of another candidate: exact-fingerprint
// drops plus every in-run merge-collapsed non-primary (within-lens,
// cross-lens, root-cause). MergedCrossLensDurable (cross-scan fold against a
// PRIOR run's findings) is deliberately excluded.
func TestStats_DuplicateRate(t *testing.T) {
	cases := []struct {
		name string
		s    Stats
		want float64
	}{
		{"zero pool is zero rate, not NaN/Inf", Stats{}, 0},
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
		{
			name: "resumed candidates widen the denominator, not just the numerator",
			s: Stats{
				Hypothesized: 5, Resumed: 5, // pool = 10
				DroppedDuplicate: 5, // all 5 duplicates came from the resumed WAL replay
			},
			want: 0.5,
		},
		{
			name: "resumed-dedup-heavy run must NOT exceed 1.0",
			// Regression: an earlier definition divided by Hypothesized alone,
			// so a run that resumes a large pending backlog and drops most of
			// it as duplicates could report a rate above 1.0 — nonsensical for
			// a fraction. The pool (Hypothesized+Resumed) bounds it correctly.
			s: Stats{
				Hypothesized: 2, Resumed: 8, // pool = 10
				DroppedDuplicate: 7,
			},
			want: 0.7,
		},
		{
			name: "durable cross-lens fold against a PRIOR run is excluded",
			s: Stats{
				Hypothesized:           10,
				MergedCrossLensDurable: 6, // cross-scan reconciliation, not in-run dedup
			},
			want: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.s.DuplicateRate(); got != c.want {
				t.Errorf("DuplicateRate() = %v, want %v", got, c.want)
			}
			if got := c.s.DuplicateRate(); got > 1.0 {
				t.Errorf("DuplicateRate() = %v, must never exceed 1.0", got)
			}
		})
	}
}
