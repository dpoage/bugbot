package engine

import (
	"testing"

	"github.com/dpoage/bugbot/internal/domain"
)

// TestApplyReranked refreshes matched findings' severity and verdict detail from
// the post-sweep set and leaves unmatched findings untouched. This is the
// oracle #3 fix: with the terminal Stage F removed from run(), a scan Result
// carries PRE-sweep severities, and the oneshot summary must reflect the
// re-ranked store state instead.
func TestApplyReranked(t *testing.T) {
	findings := []domain.Finding{
		{ID: "a", Severity: "high", VerdictDetail: "pre-a"},
		{ID: "b", Severity: "medium", VerdictDetail: "pre-b"},
		{ID: "c", Severity: "low", VerdictDetail: "untouched"},
	}
	reranked := map[string]domain.Finding{
		"a": {ID: "a", Severity: "low", VerdictDetail: "downranked: zero non-test callers"},
		"b": {ID: "b", Severity: "critical", VerdictDetail: "escalated"},
		// "c" intentionally absent: nothing re-ranked it this pass.
		"z": {ID: "z", Severity: "high", VerdictDetail: "not in the scan's result set"},
	}

	applyReranked(findings, reranked)

	if findings[0].Severity != "low" || findings[0].VerdictDetail != "downranked: zero non-test callers" {
		t.Errorf("finding a not refreshed from re-ranked set: %+v", findings[0])
	}
	if findings[1].Severity != "critical" || findings[1].VerdictDetail != "escalated" {
		t.Errorf("finding b not refreshed from re-ranked set: %+v", findings[1])
	}
	if findings[2].Severity != "low" || findings[2].VerdictDetail != "untouched" {
		t.Errorf("finding c (absent from re-ranked set) must be left untouched: %+v", findings[2])
	}
}
