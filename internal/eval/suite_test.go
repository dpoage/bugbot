package eval

import (
	"context"
	"testing"
)

// TestBenchmarkSuite is THE eval command:
//
//	go test ./internal/eval/ -run TestBenchmarkSuite -v
//
// It runs the full built-in scripted suite and asserts the precision invariants
// that define Bugbot. Scripted mode is fully controlled, so these are exact
// regression assertions, not flaky thresholds:
//
//   - No clean-code case may report a false positive (the precision floor).
//   - Aggregate precision must be exactly 1.0 (every reported finding is real).
//
// Recall is reported but not gated: scripted recall is 1.0 by construction here,
// and gating recall would convert a tuning signal into a brittle assertion.
func TestBenchmarkSuite(t *testing.T) {
	ctx := context.Background()
	res, err := RunSuite(ctx, BuiltinCases(), ModeScripted)
	if err != nil {
		t.Fatalf("run suite: %v", err)
	}

	// Always print the table so a failing run shows the full picture.
	t.Log("\n" + res.String())

	// Gate is the shared precision invariant enforced by both this regression
	// test and the `bugbot eval` CLI command, so the two never drift.
	if err := Gate(res); err != nil {
		t.Error(err)
	}

	// Sanity: the seeded cases must actually find their bugs, or the suite isn't
	// testing anything. This guards against a refactor that silently breaks
	// detection.
	if res.TruePositives == 0 {
		t.Fatalf("suite found zero true positives; detection path is broken")
	}
}

// TestBuiltinCases_PerCaseExpectations pins each built-in case's exact scored
// outcome, so a regression in any single scenario is localized rather than
// hidden in the aggregate.
func TestBuiltinCases_PerCaseExpectations(t *testing.T) {
	ctx := context.Background()
	want := map[string]struct {
		tp, fp, fn int
		clean      bool
	}{
		"nil-deref-seeded":     {tp: 1, fp: 0, fn: 0},
		"resource-leak-seeded": {tp: 1, fp: 0, fn: 0},
		"off-by-one-seeded":    {tp: 1, fp: 0, fn: 0},
		"clean-code":           {tp: 0, fp: 0, fn: 0, clean: true},
		"multi-bug":            {tp: 2, fp: 0, fn: 0},
		"suppressed-finding":   {tp: 0, fp: 0, fn: 0, clean: true},
	}

	for _, c := range BuiltinCases() {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			cr, err := RunCase(ctx, c)
			if err != nil {
				t.Fatalf("run case: %v", err)
			}
			exp, ok := want[c.Name]
			if !ok {
				t.Fatalf("no expectation registered for case %q", c.Name)
			}
			if cr.Clean() != exp.clean {
				t.Errorf("clean = %v, want %v", cr.Clean(), exp.clean)
			}
			if cr.TruePositives != exp.tp || cr.FalsePositives != exp.fp || cr.FalseNegatives != exp.fn {
				t.Errorf("tp/fp/fn = %d/%d/%d, want %d/%d/%d",
					cr.TruePositives, cr.FalsePositives, cr.FalseNegatives, exp.tp, exp.fp, exp.fn)
			}
		})
	}
}

// TestSuppressedCase_NeverVerifies confirms the suppressed bug dies in triage
// (stage stats), not in verification — the "where-killed" signal must be
// accurate.
func TestSuppressedCase_NeverVerifies(t *testing.T) {
	ctx := context.Background()
	cr, err := RunCase(ctx, suppressedCase())
	if err != nil {
		t.Fatalf("run case: %v", err)
	}
	if cr.Stats.DroppedSuppressed != 1 {
		t.Errorf("dropped_suppressed = %d, want 1", cr.Stats.DroppedSuppressed)
	}
	if cr.Stats.Verified != 0 {
		t.Errorf("verified = %d, want 0 (suppressed bug must not reach verify)", cr.Stats.Verified)
	}
	if len(cr.Findings) != 0 {
		t.Errorf("findings = %d, want 0", len(cr.Findings))
	}
}
