package domain

import "strings"

// Confidence is a finder's or verifier's self-reported confidence in a
// candidate. Closed set; convert untrusted input with ParseConfidence.
type Confidence string

const (
	ConfidenceHigh   Confidence = "high"
	ConfidenceMedium Confidence = "medium"
	ConfidenceLow    Confidence = "low"
)

// ParseConfidence converts s to a Confidence, reporting whether it was a
// recognized value (case-insensitive, whitespace-trimmed). Unrecognized model
// output is treated conservatively by callers (historically coerced to low so
// ambiguous candidates are dropped in triage), so no default is baked in here.
func ParseConfidence(s string) (Confidence, bool) {
	switch Confidence(strings.ToLower(strings.TrimSpace(s))) {
	case ConfidenceHigh:
		return ConfidenceHigh, true
	case ConfidenceMedium:
		return ConfidenceMedium, true
	case ConfidenceLow:
		return ConfidenceLow, true
	default:
		return "", false
	}
}

// Rank is an ordinal where a higher number is more confident (high=3 .. low=1,
// unknown=0).
func (c Confidence) Rank() int {
	switch c {
	case ConfidenceHigh:
		return 3
	case ConfidenceMedium:
		return 2
	case ConfidenceLow:
		return 1
	default:
		return 0
	}
}

// AtLeast reports whether c is at least as confident as min.
func (c Confidence) AtLeast(min Confidence) bool { return c.Rank() >= min.Rank() }

// String returns the canonical lowercase token.
func (c Confidence) String() string { return string(c) }
