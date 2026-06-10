package report

import (
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/store"
)

// TestTierName_T0 confirms tierName returns the correct label for tier 0.
func TestTierName_T0(t *testing.T) {
	got := tierName(0)
	want := "T0 Fix-witnessed"
	if got != want {
		t.Errorf("tierName(0) = %q, want %q", got, want)
	}
}

// TestTierName_ExistingTiers confirms existing tier labels are unchanged.
func TestTierName_ExistingTiers(t *testing.T) {
	cases := []struct {
		tier int
		want string
	}{
		{1, "T1 Reproduced"},
		{2, "T2 Verified"},
		{3, "T3 Suspected"},
	}
	for _, tc := range cases {
		got := tierName(tc.tier)
		if got != tc.want {
			t.Errorf("tierName(%d) = %q, want %q", tc.tier, got, tc.want)
		}
	}
}

// TestMarkdown_T0FindingRendered confirms a Tier-0 finding is rendered with the
// "T0 Fix-witnessed" tier label and appears in the by-tier summary.
func TestMarkdown_T0FindingRendered(t *testing.T) {
	f := store.Finding{
		ID:          "t0-finding-id",
		Fingerprint: "fp-t0-fix",
		Title:       "fix-witnessed finding",
		Description: "A bug with a witnessed fix.",
		Reasoning:   "Patch-prover confirmed the fix.",
		Severity:    "high",
		Tier:        0,
		Status:      store.StatusOpen,
		File:        "pkg/x.go",
		Line:        10,
		CommitSHA:   "abc123",
	}
	r := New([]store.Finding{f}, fixtureMeta())
	got := Markdown(r)

	for _, want := range []string{
		"T0 Fix-witnessed",
		"fix-witnessed finding",
		"pkg/x.go:10",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("markdown missing %q\n--- got ---\n%s", want, got)
		}
	}
}

// TestMarkdown_FixPatchRendered confirms the candidate-fix diff block is
// rendered when FixPatch is non-empty.
func TestMarkdown_FixPatchRendered(t *testing.T) {
	diff := "--- a/calc.go\n+++ b/calc.go\n@@ -1 +1 @@\n-return 0\n+return 1\n"
	f := store.Finding{
		ID:          "fp-fix-patch",
		Fingerprint: "fp-fix-patch",
		Title:       "Divide ignores zero divisor",
		Severity:    "high",
		Tier:        0,
		Status:      store.StatusOpen,
		File:        "calc.go",
		Line:        5,
		ReproPath:   ".bugbot/repro/fp-fix-patch",
		FixPatch:    diff,
	}
	r := New([]store.Finding{f}, fixtureMeta())
	got := Markdown(r)

	for _, want := range []string{
		"Candidate fix",
		"witness",
		"NOT reviewed",
		"```diff",
		diff,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("markdown missing %q\n--- got ---\n%s", want, got)
		}
	}
}

// TestMarkdown_NeedsHumanRendered confirms the needs-human meta line appears
// when NeedsHuman is true.
func TestMarkdown_NeedsHumanRendered(t *testing.T) {
	f := store.Finding{
		ID:          "fp-needs-human",
		Fingerprint: "fp-needs-human",
		Title:       "Complex concurrency bug",
		Severity:    "high",
		Tier:        1,
		Status:      store.StatusOpen,
		File:        "queue.go",
		Line:        42,
		NeedsHuman:  true,
	}
	r := New([]store.Finding{f}, fixtureMeta())
	got := Markdown(r)

	want := "Needs human review"
	if !strings.Contains(got, want) {
		t.Errorf("markdown missing %q\n--- got ---\n%s", want, got)
	}
}

// TestMarkdown_NoFixPatch confirms the fix-patch block is absent when
// FixPatch is empty.
func TestMarkdown_NoFixPatch(t *testing.T) {
	f := store.Finding{
		ID:          "fp-no-patch",
		Fingerprint: "fp-no-patch",
		Title:       "ordinary finding",
		Severity:    "medium",
		Tier:        2,
		Status:      store.StatusOpen,
		File:        "x.go",
		Line:        1,
	}
	r := New([]store.Finding{f}, fixtureMeta())
	got := Markdown(r)

	if strings.Contains(got, "Candidate fix") {
		t.Errorf("markdown should not contain 'Candidate fix' when FixPatch is empty")
	}
	if strings.Contains(got, "Needs human review") {
		t.Errorf("markdown should not contain 'Needs human review' when NeedsHuman is false")
	}
}

// TestMarkdown_T0InSummaryCount confirms tier-0 count appears in the by-tier
// summary.
func TestMarkdown_T0InSummaryCount(t *testing.T) {
	findings := []store.Finding{
		{Fingerprint: "fp-a", Title: "a", Tier: 0, Status: store.StatusOpen, File: "a.go", Line: 1},
		{Fingerprint: "fp-b", Title: "b", Tier: 1, Status: store.StatusOpen, File: "b.go", Line: 1},
	}
	r := New(findings, fixtureMeta())
	got := Markdown(r)

	if !strings.Contains(got, "T0 Fix-witnessed: 1") {
		t.Errorf("summary missing 'T0 Fix-witnessed: 1':\n%s", got)
	}
	if !strings.Contains(got, "T1 Reproduced: 1") {
		t.Errorf("summary missing 'T1 Reproduced: 1':\n%s", got)
	}
}
