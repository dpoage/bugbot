package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/store"
)

// fakeReader is a hand-written in-memory implementation of store.StoreReader.
// It exists solely for testing OpenBacklog without a real SQLite database. It
// only implements ListFindings because that is the only method OpenBacklog calls.
type fakeReader struct {
	findings []domain.Finding
}

func (f *fakeReader) GetFinding(_ context.Context, id string) (domain.Finding, error) {
	for _, fi := range f.findings {
		if fi.ID == id {
			return fi, nil
		}
	}
	return domain.Finding{}, store.ErrNotFound
}

func (f *fakeReader) GetFindingByFingerprint(_ context.Context, fp string) (domain.Finding, error) {
	for _, fi := range f.findings {
		if fi.Fingerprint == fp {
			return fi, nil
		}
	}
	return domain.Finding{}, store.ErrNotFound
}

func (f *fakeReader) ListFindings(_ context.Context, filter domain.FindingFilter) ([]domain.Finding, error) {
	var out []domain.Finding
	for _, fi := range f.findings {
		if filter.Status != "" && fi.Status != filter.Status {
			continue
		}
		out = append(out, fi)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// TestOpenBacklog_FakeReader: OpenBacklog filters and sorts correctly using the
// fakeReader double — no real SQLite database is opened.
// ---------------------------------------------------------------------------

func TestOpenBacklog_FakeReader(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	fr := &fakeReader{findings: []domain.Finding{
		// eligible: Tier3/Suspected, no ReproPath, no NeedsHuman — oldest
		{ID: "a", Fingerprint: "fp-a", Status: domain.StatusOpen, Tier: domain.TierSuspected, UpdatedAt: now.Add(-2 * time.Hour)},
		// eligible: Tier2/Verified, no ReproPath, no NeedsHuman — newest
		{ID: "b", Fingerprint: "fp-b", Status: domain.StatusOpen, Tier: domain.TierVerified, UpdatedAt: now.Add(-1 * time.Hour)},
		// ineligible: dismissed
		{ID: "c", Fingerprint: "fp-c", Status: domain.StatusDismissed, Tier: domain.TierSuspected, UpdatedAt: now},
		// ineligible: NeedsHuman
		{ID: "d", Fingerprint: "fp-d", Status: domain.StatusOpen, Tier: domain.TierSuspected, NeedsHuman: true, UpdatedAt: now.Add(-3 * time.Hour)},
		// ineligible: already has ReproPath
		{ID: "e", Fingerprint: "fp-e", Status: domain.StatusOpen, Tier: domain.TierSuspected, ReproPath: "/some/path", UpdatedAt: now.Add(-4 * time.Hour)},
		// ineligible: Tier0 (fix-witnessed) — not T2/T3
		{ID: "f", Fingerprint: "fp-f", Status: domain.StatusOpen, Tier: domain.TierFixWitnessed, UpdatedAt: now.Add(-5 * time.Hour)},
	}}

	got, err := OpenBacklog(context.Background(), fr)
	if err != nil {
		t.Fatalf("OpenBacklog: %v", err)
	}

	// Expect exactly a and b, in oldest-updated-first order (a before b).
	if len(got) != 2 {
		t.Fatalf("want 2 findings, got %d: %v", len(got), idsOf(got))
	}
	if got[0].ID != "a" || got[1].ID != "b" {
		t.Errorf("want [a b], got %v", idsOf(got))
	}
}
