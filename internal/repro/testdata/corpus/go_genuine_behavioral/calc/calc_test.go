package calc

import "testing"

// TestDivideByZeroReturnsError is the acceptance-(d) counterpart to the four
// non-behavioral fixtures in this corpus: it actually calls the target
// package's Divide function (same-directory colocation is itself an
// executable edge for Go), so it must NOT be flagged by
// ClassifyTargetExecution.
func TestDivideByZeroReturnsError(t *testing.T) {
	if _, err := Divide(1, 0); err == nil {
		t.Fatal("expected an error dividing by zero, got nil")
	}
}
