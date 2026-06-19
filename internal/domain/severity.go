package domain

import "strings"

// Severity is the impact class a finder assigns to a candidate bug. It is a
// closed set; convert untrusted or model-supplied strings with ParseSeverity.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityHigh     Severity = "high"
	SeverityMedium   Severity = "medium"
	SeverityLow      Severity = "low"
)

// ParseSeverity converts s to a Severity, reporting whether it was a recognized
// value. Matching is case-insensitive and trims surrounding whitespace.
//
// It deliberately does NOT pick a default for unrecognized input: callers apply
// their own (historically "medium" for ranking/reporting, "high" for the
// sandbox-execution floor), so the policy stays at the call site.
func ParseSeverity(s string) (Severity, bool) {
	switch Severity(strings.ToLower(strings.TrimSpace(s))) {
	case SeverityCritical:
		return SeverityCritical, true
	case SeverityHigh:
		return SeverityHigh, true
	case SeverityMedium:
		return SeverityMedium, true
	case SeverityLow:
		return SeverityLow, true
	default:
		return "", false
	}
}

// Rank is an ordinal where a HIGHER number is MORE severe (critical=4 .. low=1,
// unknown=0). Use it to sort most-severe-first; prefer AtLeast for threshold
// checks so call sites never have to reason about rank direction.
func (s Severity) Rank() int {
	switch s {
	case SeverityCritical:
		return 4
	case SeverityHigh:
		return 3
	case SeverityMedium:
		return 2
	case SeverityLow:
		return 1
	default:
		return 0
	}
}

// AtLeast reports whether s is at least as severe as min.
func (s Severity) AtLeast(min Severity) bool { return s.Rank() >= min.Rank() }

// String returns the canonical lowercase token.
func (s Severity) String() string { return string(s) }
