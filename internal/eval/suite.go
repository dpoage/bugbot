package eval

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
)

// SuiteResult aggregates the scored results of a whole case suite.
type SuiteResult struct {
	// Mode is the mode the suite ran in.
	Mode Mode
	// Cases are the per-case results, in run order.
	Cases []*CaseResult

	// Aggregate counts across all cases.
	TruePositives  int
	FalsePositives int
	FalseNegatives int

	// PerLensTP / PerLensFP are the lens breakdowns summed across cases.
	PerLensTP map[string]int
	PerLensFP map[string]int
}

// Precision is the suite-wide TP / (TP + FP); 1.0 when nothing was reported.
func (s *SuiteResult) Precision() float64 {
	denom := s.TruePositives + s.FalsePositives
	if denom == 0 {
		return 1.0
	}
	return float64(s.TruePositives) / float64(denom)
}

// Recall is the suite-wide TP / (TP + FN); 1.0 when nothing was seeded.
func (s *SuiteResult) Recall() float64 {
	denom := s.TruePositives + s.FalseNegatives
	if denom == 0 {
		return 1.0
	}
	return float64(s.TruePositives) / float64(denom)
}

// CleanFalsePositives returns the total false positives across clean-code
// cases. The benchmark gate requires this to be zero: any finding in code with
// no seeded bug is a precision failure.
func (s *SuiteResult) CleanFalsePositives() int {
	total := 0
	for _, c := range s.Cases {
		if c.Clean() {
			total += c.FalsePositives
		}
	}
	return total
}

// Gate enforces the benchmark's precision invariants on a scored suite. It is
// the single source of truth shared by the eval CLI command and the
// TestBenchmarkSuite regression test, so the build gate and the command never
// drift apart. It returns nil when the suite passes, or a descriptive error
// naming every violation otherwise.
//
// The invariants (see internal/eval/README.md):
//
//   - No clean-code case may report a false positive (the precision floor).
//   - Aggregate precision must be exactly 1.0 (every reported finding is real).
//
// These are exact assertions, not flaky thresholds, because scripted mode is
// fully controlled. Recall is intentionally NOT gated: gating a tuning signal
// would convert it into a brittle assertion.
func Gate(s *SuiteResult) error {
	var problems []string

	if fp := s.CleanFalsePositives(); fp != 0 {
		problems = append(problems, fmt.Sprintf("clean-code cases produced %d false positive(s); want 0", fp))
		for _, c := range s.Cases {
			if c.Clean() && c.FalsePositives > 0 {
				for _, f := range c.UnmatchedFindings {
					problems = append(problems, fmt.Sprintf("  FP in %q: %s:%d %q (lens=%s)", c.Name, f.File, f.Line, f.Title, f.Lens))
				}
			}
		}
	}

	if p := s.Precision(); p < 1.0 {
		problems = append(problems, fmt.Sprintf("aggregate precision = %.3f; want exactly 1.0", p))
		for _, c := range s.Cases {
			if c.FalsePositives > 0 {
				problems = append(problems, fmt.Sprintf("  case %q has %d FP", c.Name, c.FalsePositives))
			}
		}
	}

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("eval gate failed:\n%s", strings.Join(problems, "\n"))
}

// RunSuite runs every case in mode, scores each, and aggregates. It returns an
// error only on a harness failure for some case (fixture/store/funnel wiring);
// detection results — even bad ones — are returned in the SuiteResult, not as
// errors, so callers can assert on them.
func RunSuite(ctx context.Context, cases []Case, mode Mode) (*SuiteResult, error) {
	out := &SuiteResult{
		Mode:      mode,
		PerLensTP: map[string]int{},
		PerLensFP: map[string]int{},
	}
	for _, c := range cases {
		var (
			cr  *CaseResult
			err error
		)
		switch mode {
		case ModeRecorded:
			cr, err = runCaseRecorded(ctx, c)
		default:
			cr, err = runCaseScripted(ctx, c)
		}
		if err != nil {
			return nil, fmt.Errorf("eval: case %q: %w", c.Name, err)
		}
		out.Cases = append(out.Cases, cr)
		out.TruePositives += cr.TruePositives
		out.FalsePositives += cr.FalsePositives
		out.FalseNegatives += cr.FalseNegatives
		for lens, n := range cr.PerLensTP {
			out.PerLensTP[lens] += n
		}
		for lens, n := range cr.PerLensFP {
			out.PerLensFP[lens] += n
		}
	}
	return out, nil
}

// runCaseScripted runs one case in scripted mode (its Recorded field, if any,
// is ignored). It forces the case into scripted mode so buildClients uses the
// scripted path regardless of the case's original kind.
func runCaseScripted(ctx context.Context, c Case) (*CaseResult, error) {
	c.kind = CaseKindScripted
	c.Recorded = nil
	return RunCase(ctx, c)
}

// runCaseRecorded runs one case in recorded mode. It errors if the case carries
// no recordings, so a misconfigured suite fails loudly rather than silently
// scoring an all-empty run.
func runCaseRecorded(ctx context.Context, c Case) (*CaseResult, error) {
	if c.Recorded == nil {
		return nil, fmt.Errorf("recorded mode requested but case has no recordings")
	}
	c.kind = CaseKindRecorded
	c.Scripted = nil
	return RunCase(ctx, c)
}

// String renders the suite as a table plus an aggregate line. Columns: case,
// clean flag, true/false positives, false negatives, precision, recall, and a
// "where-killed" note that surfaces where seeded bugs were lost in the funnel.
func (s *SuiteResult) String() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Bugbot eval suite (mode=%s)\n\n", s.Mode)

	tw := tabwriter.NewWriter(&sb, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "CASE\tKIND\tTP\tFP\tFN\tPREC\tRECALL\tWHERE-KILLED")
	for _, c := range s.Cases {
		kind := "seeded"
		if c.Clean() {
			kind = "clean"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%.2f\t%.2f\t%s\n",
			c.Name, kind,
			c.TruePositives, c.FalsePositives, c.FalseNegatives,
			c.Precision(), c.Recall(),
			whereKilled(c),
		)
	}
	_ = tw.Flush()

	fmt.Fprintf(&sb, "\nAGGREGATE  tp=%d fp=%d fn=%d  precision=%.3f recall=%.3f  clean-fp=%d\n",
		s.TruePositives, s.FalsePositives, s.FalseNegatives,
		s.Precision(), s.Recall(), s.CleanFalsePositives())

	if len(s.PerLensTP) > 0 || len(s.PerLensFP) > 0 {
		fmt.Fprintf(&sb, "\nPer-lens (tp/fp):\n")
		for _, lens := range lensKeys(s.PerLensTP, s.PerLensFP) {
			fmt.Fprintf(&sb, "  %-28s %d/%d\n", lens, s.PerLensTP[lens], s.PerLensFP[lens])
		}
	}
	return sb.String()
}

// whereKilled produces a short note explaining where a case's seeded bugs were
// lost in the funnel, or "-" when nothing was lost. It reads the per-stage stats
// passthrough: a seeded bug can die in triage (low-confidence / out-of-scope /
// duplicate / suppressed) or in verification (killed by refuters).
func whereKilled(c *CaseResult) string {
	if c.FalseNegatives == 0 && c.FalsePositives == 0 {
		return "-"
	}
	st := c.Stats
	var parts []string
	if st.DroppedSuppressed > 0 {
		parts = append(parts, fmt.Sprintf("suppressed=%d", st.DroppedSuppressed))
	}
	if st.DroppedLowConfidence > 0 {
		parts = append(parts, fmt.Sprintf("low-conf=%d", st.DroppedLowConfidence))
	}
	if st.DroppedOutOfScope > 0 {
		parts = append(parts, fmt.Sprintf("out-of-scope=%d", st.DroppedOutOfScope))
	}
	if st.DroppedDuplicate > 0 {
		parts = append(parts, fmt.Sprintf("dup=%d", st.DroppedDuplicate))
	}
	if st.MergedWithinLens > 0 || st.MergedCrossLens > 0 || st.MergedRootCause > 0 {
		parts = append(parts, fmt.Sprintf("merged=%d/%d/%d(within/cross/root)", st.MergedWithinLens, st.MergedCrossLens, st.MergedRootCause))
	}
	if st.Killed > 0 {
		parts = append(parts, fmt.Sprintf("refuted=%d", st.Killed))
	}
	if len(parts) == 0 {
		// Lost without a stage explanation (e.g. finder never hypothesized it, or
		// an FP slipped all the way through). Surface the funnel counts so it's
		// still legible.
		return fmt.Sprintf("hyp=%d→tri=%d→ver=%d", st.Hypothesized, st.Triaged, st.Verified)
	}
	return strings.Join(parts, " ")
}

func lensKeys(maps ...map[string]int) []string {
	seen := map[string]bool{}
	var keys []string
	for _, m := range maps {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}
	sort.Strings(keys)
	return keys
}
