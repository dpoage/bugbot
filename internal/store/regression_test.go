package store

import (
	"context"
	"fmt"
	"testing"

	"github.com/dpoage/bugbot/internal/domain"
)

// sampleFindingAt builds a finding at (file, line) with the given status,
// distinct from sampleFinding()/sampleFindingSwept() so file-window tests can
// place several findings at controlled offsets without lens/title collisions.
func sampleFindingAt(file string, line int, lens, title string, status domain.Status) domain.Finding {
	fp := domain.Fingerprint(lens, file, fmt.Sprintf("%d|%s", line, title))
	return domain.Finding{
		Fingerprint: fp,
		Title:       title,
		Description: "buffer bounds check is missing before the write at this offset",
		Severity:    "high",
		Tier:        domain.TierVerified,
		Status:      status,
		Lens:        lens,
		File:        file,
		Line:        line,
		CommitSHA:   "abc123",
		FileHash:    "hash-v1",
	}
}

// TestFindingsByFileWindow_StatusAndRangeFilter proves the widened lookup
// query triage's durable cross-lens fold now uses: it returns only rows in
// the requested file, within the line window, whose status is in the
// requested set — mirroring the same-file, same-window breadth
// funnel.SimilarFinding applies, but letting the caller choose which
// lifecycle states participate.
func TestFindingsByFileWindow_StatusAndRangeFilter(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	const file = "internal/x/buf.go"
	inWindowOpen := sampleFindingAt(file, 100, "boundary-conditions", "off by one A", domain.StatusOpen)
	inWindowFixed := sampleFindingAt(file, 105, "boundary-conditions", "off by one B", domain.StatusFixed)
	inWindowDismissed := sampleFindingAt(file, 95, "boundary-conditions", "off by one C", domain.StatusDismissed)
	outOfWindow := sampleFindingAt(file, 500, "boundary-conditions", "off by one D", domain.StatusOpen)
	otherFile := sampleFindingAt("internal/x/other.go", 100, "boundary-conditions", "off by one E", domain.StatusOpen)
	wrongStatus := sampleFindingAt(file, 102, "boundary-conditions", "off by one F", domain.StatusSuperseded)

	for _, f := range []domain.Finding{inWindowOpen, inWindowFixed, inWindowDismissed, outOfWindow, otherFile, wrongStatus} {
		if _, err := st.UpsertFinding(ctx, f); err != nil {
			t.Fatalf("seed upsert %q: %v", f.Title, err)
		}
	}

	got, err := st.FindingsByFileWindow(ctx, file, 100, 10, /* mirrors funnel.DefaultMergeWindow */
		[]domain.Status{domain.StatusOpen, domain.StatusFixed, domain.StatusDismissed})
	if err != nil {
		t.Fatalf("FindingsByFileWindow: %v", err)
	}
	wantFPs := map[string]bool{
		inWindowOpen.Fingerprint:      true,
		inWindowFixed.Fingerprint:     true,
		inWindowDismissed.Fingerprint: true,
	}
	if len(got) != len(wantFPs) {
		t.Fatalf("FindingsByFileWindow returned %d rows, want %d: %+v", len(got), len(wantFPs), got)
	}
	for _, f := range got {
		if !wantFPs[f.Fingerprint] {
			t.Errorf("unexpected row in window result: %q (status=%s, line=%d)", f.Title, f.Status, f.Line)
		}
	}

	// Narrowing the status set narrows the result.
	openOnly, err := st.FindingsByFileWindow(ctx, file, 100, 10 /* mirrors funnel.DefaultMergeWindow */, []domain.Status{domain.StatusOpen})
	if err != nil {
		t.Fatalf("FindingsByFileWindow open-only: %v", err)
	}
	if len(openOnly) != 1 || openOnly[0].Fingerprint != inWindowOpen.Fingerprint {
		t.Fatalf("open-only window result = %+v, want just %q", openOnly, inWindowOpen.Title)
	}

	// Empty statuses is a no-op, not an error.
	none, err := st.FindingsByFileWindow(ctx, file, 100, 10 /* mirrors funnel.DefaultMergeWindow */, nil)
	if err != nil {
		t.Fatalf("FindingsByFileWindow empty statuses: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("empty statuses should return no rows, got %d", len(none))
	}
}

// TestReopenAsRegression_PreservesIdentityNoNewRow proves the fixed-match
// regression path: reopening a fixed finding flips it back to open IN PLACE —
// same id, same fingerprint, same tier/repro history — rather than minting a
// second row for the same defect.
func TestReopenAsRegression_PreservesIdentityNoNewRow(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	f := sampleFindingAt("internal/x/buf.go", 42, "boundary-conditions", "heap overflow on resize", domain.StatusFixed)
	f.Tier = domain.TierReproduced
	f.ReproPath = "repro/heap-overflow.sh"
	seeded, err := st.UpsertFinding(ctx, f)
	if err != nil {
		t.Fatalf("seed upsert: %v", err)
	}

	if err := st.ReopenAsRegression(ctx, f.Fingerprint); err != nil {
		t.Fatalf("ReopenAsRegression: %v", err)
	}

	got, err := st.GetFindingByFingerprint(ctx, f.Fingerprint)
	if err != nil {
		t.Fatalf("GetFindingByFingerprint: %v", err)
	}
	if got.Status != domain.StatusOpen {
		t.Fatalf("status after reopen = %q, want open", got.Status)
	}
	if got.ID != seeded.ID {
		t.Fatalf("ReopenAsRegression changed the row id: got %q, want %q (identity must be preserved)", got.ID, seeded.ID)
	}
	if got.Fingerprint != f.Fingerprint {
		t.Fatalf("fingerprint changed: got %q, want %q", got.Fingerprint, f.Fingerprint)
	}
	if got.Tier != domain.TierReproduced {
		t.Fatalf("tier changed on reopen: got %v, want %v (history must be preserved)", got.Tier, domain.TierReproduced)
	}
	if got.ReproPath != f.ReproPath {
		t.Fatalf("ReproPath changed on reopen: got %q, want %q", got.ReproPath, f.ReproPath)
	}

	all, err := st.ListFindings(ctx, domain.FindingFilter{})
	if err != nil {
		t.Fatalf("ListFindings: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected exactly 1 row for the fingerprint (no new row minted), got %d: %+v", len(all), all)
	}
}

// TestReopenAsRegression_NotFound proves the ErrNotFound contract for a
// fingerprint with no backing row.
func TestReopenAsRegression_NotFound(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	if err := st.ReopenAsRegression(ctx, "does-not-exist"); err == nil {
		t.Fatal("expected an error reopening a nonexistent fingerprint")
	}
}
