package report

import (
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/store"
)

// contradictedFinding returns a Finding with ReproContradicted=true for use in
// rendering tests. All other fields are minimal to keep the test focused.
func contradictedFinding() store.Finding {
	return store.Finding{
		ID:                "dddd000011112222",
		Fingerprint:       "fp-contradicted",
		Title:             "heap use after free in allocator",
		Description:       "allocator reuses freed memory under concurrent load.",
		Reasoning:         "verifier confirmed; repro ran twice, bug did not manifest.",
		Severity:          "high",
		Tier:              2,
		Status:            store.StatusOpen,
		Lens:              "uaf",
		File:              "internal/alloc/alloc.go",
		Line:              77,
		CommitSHA:         "deadbeef",
		FileHash:          "h-uaf",
		ReproContradicted: true,
	}
}

// TestMarkdown_ContradictedSignalPresent pins that the Markdown renderer
// includes the repro-contradicted notice when Finding.ReproContradicted is true.
func TestMarkdown_ContradictedSignalPresent(t *testing.T) {
	r := New([]store.Finding{contradictedFinding()}, fixtureMeta())
	got := Markdown(r)

	// The notice must reference the threshold so it tracks the constant.
	if !strings.Contains(got, "Repro-contradicted") {
		t.Error("markdown missing Repro-contradicted notice for contradicted finding")
	}
	if !strings.Contains(got, "2") { // ReproContradictionThreshold = 2
		t.Error("markdown Repro-contradicted notice must include the threshold count")
	}
	if !strings.Contains(got, "independent attempts") {
		t.Error("markdown Repro-contradicted notice must describe independent attempts")
	}
}

// TestMarkdown_NoContradictedSignalWhenFalse pins that the notice is absent
// when ReproContradicted is false (the common case for most findings).
func TestMarkdown_NoContradictedSignalWhenFalse(t *testing.T) {
	f := contradictedFinding()
	f.ReproContradicted = false
	r := New([]store.Finding{f}, fixtureMeta())
	got := Markdown(r)

	if strings.Contains(got, "Repro-contradicted") {
		t.Error("markdown must NOT include Repro-contradicted notice when ReproContradicted=false")
	}
}
