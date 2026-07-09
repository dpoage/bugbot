package store

import (
	"context"
	"fmt"
	"testing"

	"github.com/dpoage/bugbot/internal/domain"
)

// TestSuppressionFlow is the headline behaviour: a dismissed fingerprint must
// never resurface as open, even if a finder re-discovers it on a later scan.
func TestSuppressionFlow_DismissedNeverResurfaces(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	f := sampleFinding()
	if _, err := st.UpsertFinding(ctx, f); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Maintainer dismisses it via UpdateStatus, which records the suppression.
	if err := st.UpdateStatus(ctx, f.Fingerprint, domain.StatusDismissed, "false positive"); err != nil {
		t.Fatalf("dismiss: %v", err)
	}

	sup, err := st.IsSuppressed(ctx, f.Fingerprint, "")
	if err != nil {
		t.Fatalf("IsSuppressed: %v", err)
	}
	if !sup {
		t.Fatal("fingerprint should be suppressed after dismissal")
	}

	// A later scan re-discovers the same bug and upserts it as OPEN.
	f2 := f
	f2.Status = domain.StatusOpen
	f2.CommitSHA = "newcommit"
	got, err := st.UpsertFinding(ctx, f2)
	if err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	// Suppression memory must force it back to dismissed.
	if got.Status != domain.StatusDismissed {
		t.Fatalf("re-discovered suppressed finding resurfaced as %q; want dismissed", got.Status)
	}

	open, err := st.ListFindings(ctx, domain.FindingFilter{Status: domain.StatusOpen})
	if err != nil {
		t.Fatalf("list open: %v", err)
	}
	if len(open) != 0 {
		t.Fatalf("expected no open findings, got %d", len(open))
	}
}

func TestAddSuppression_FlipsExistingFindingAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	f := sampleFinding()
	if _, err := st.UpsertFinding(ctx, f); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	if err := st.AddSuppression(ctx, f.Fingerprint, "wontfix"); err != nil {
		t.Fatalf("add suppression: %v", err)
	}
	got, err := st.GetFindingByFingerprint(ctx, f.Fingerprint)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != domain.StatusDismissed {
		t.Fatalf("AddSuppression should flip finding to dismissed, got %q", got.Status)
	}

	// Re-suppress with a new reason: idempotent, updates reason, single row.
	if err := st.AddSuppression(ctx, f.Fingerprint, "still wontfix"); err != nil {
		t.Fatalf("re-suppress: %v", err)
	}
	sups, err := st.ListSuppressions(ctx)
	if err != nil {
		t.Fatalf("list suppressions: %v", err)
	}
	if len(sups) != 1 {
		t.Fatalf("expected 1 suppression row, got %d", len(sups))
	}
	if sups[0].Reason != "still wontfix" {
		t.Fatalf("reason not updated: %q", sups[0].Reason)
	}
}

func TestAddSuppression_PreemptiveBeforeFindingExists(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	fp := domain.Fingerprint("x", "a.go", fmt.Sprintf("%d|%s", 1, "preempt"))
	// Suppress before any finding exists.
	if err := st.AddSuppression(ctx, fp, "known noise"); err != nil {
		t.Fatalf("preemptive suppress: %v", err)
	}

	// A scan later discovers it as open; it must land dismissed.
	got, err := st.UpsertFinding(ctx, domain.Finding{
		Fingerprint: fp, Title: "preempt", Tier: 3, Status: domain.StatusOpen,
		Lens: "x", File: "a.go", Line: 1,
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if got.Status != domain.StatusDismissed {
		t.Fatalf("pre-suppressed finding should be dismissed, got %q", got.Status)
	}
}

func TestIsSuppressed_Unknown(t *testing.T) {
	st := openTemp(t)
	sup, err := st.IsSuppressed(context.Background(), "never-seen", "")
	if err != nil {
		t.Fatalf("IsSuppressed: %v", err)
	}
	if sup {
		t.Fatal("unknown fingerprint should not be suppressed")
	}
}

// TestIsSuppressed_LegacyLocusFallback_PreservesV2SuppressionCoverage pins the
// v2->v3 migration contract (bugbot-ezmx.1): a suppression row that predates
// Fingerprint v3 (defect_kind/subject did not exist yet, so it was recorded
// under a v2-scheme fingerprint keyed by (lens, file, locus)) must still
// suppress the SAME locus after a fresh scan mints a v3 fingerprint for it —
// even though the two fingerprint strings are completely different hashes.
//
// This seeds a "legacy" suppression row directly via SQL exactly as the
// 020_defect_identity_v3 migration's backfill would have produced it: legacy=1
// and locus_key populated from a v2-era finding sharing the old fingerprint.
// It does NOT re-run the migration's ALTER/UPDATE statements (those already
// run unconditionally on every openTemp() in this package; a SQL mistake
// there would fail every store test at Open, not just this one) — it proves
// the RUNTIME fallback IsSuppressed relies on to honor a backfilled row.
func TestIsSuppressed_LegacyLocusFallback_PreservesV2SuppressionCoverage(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	const legacyLocusKey = "legacy-locus-key-nil-deref-greeting"
	v2FP := domain.Fingerprint("nil-safety", "bug.go", "S:function\x00Greeting")

	// Simulate the migration backfill: a pre-v3 suppression row, marked legacy,
	// with locus_key populated from the finding it once matched.
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO suppressions (fingerprint, reason, created_at, locus_key, legacy) VALUES (?, ?, ?, ?, 1)`,
		v2FP, "v2-era: known noise", "2025-01-01T00:00:00Z", legacyLocusKey,
	); err != nil {
		t.Fatalf("seed legacy suppression: %v", err)
	}

	// A fresh scan re-discovers the same defect and mints a v3 fingerprint —
	// necessarily DIFFERENT from the legacy v2 fingerprint (different scheme,
	// different inputs).
	v3FP := domain.FingerprintV3("bug.go", "S:function\x00Greeting", domain.DefectNilDeref, "Greeting")
	if v3FP == v2FP {
		t.Fatalf("test setup invariant violated: v3 fingerprint must differ from the v2 one")
	}

	suppressed, err := st.IsSuppressed(ctx, v3FP, legacyLocusKey)
	if err != nil {
		t.Fatalf("IsSuppressed: %v", err)
	}
	if !suppressed {
		t.Fatal("a v2-era suppression must still suppress a post-migration v3 fingerprint at the same locus")
	}

	// Sanity: the legacy fallback is locus-scoped, not global — an unrelated
	// locus_key must NOT be suppressed by this row.
	unrelated, err := st.IsSuppressed(ctx, v3FP, "some-other-locus-entirely")
	if err != nil {
		t.Fatalf("IsSuppressed: %v", err)
	}
	if unrelated {
		t.Fatal("legacy suppression fallback must be scoped to its own locus_key, not suppress everything")
	}
}

// TestListSuppressions_DeterministicOrderUnderTiedCreatedAt verifies the
// rowid tiebreak added in 89r.5: when many suppressions share a created_at
// (e.g. one round of triage-dismissal fired within the same wall-clock tick),
// ListSuppressions still returns them in a stable order across calls. Without
// the secondary sort, two calls could legally return tied rows in different
// orders and tests that snapshot the list would flake.
func TestListSuppressions_DeterministicOrderUnderTiedCreatedAt(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	// Stamp a fixed wall-clock time on every insert so all created_at collide.
	// We can't pin nowUTC() from this test (it would be a global hook), so
	// instead insert via direct SQL and force a known, identical timestamp.
	fps := []string{"fp-a", "fp-b", "fp-c", "fp-d", "fp-e"}
	fixed := "2025-01-01T00:00:00Z"
	for _, fp := range fps {
		if _, err := st.DB().ExecContext(ctx,
			`INSERT INTO suppressions (fingerprint, reason, created_at) VALUES (?, ?, ?)`,
			fp, "tied", fixed); err != nil {
			t.Fatalf("insert %q: %v", fp, err)
		}
	}

	first, err := st.ListSuppressions(ctx)
	if err != nil {
		t.Fatalf("list 1: %v", err)
	}
	if len(first) != len(fps) {
		t.Fatalf("first call: got %d rows, want %d", len(first), len(fps))
	}

	second, err := st.ListSuppressions(ctx)
	if err != nil {
		t.Fatalf("list 2: %v", err)
	}
	if len(second) != len(fps) {
		t.Fatalf("second call: got %d rows, want %d", len(second), len(fps))
	}

	// Order must be stable across calls.
	for i := range first {
		if first[i].Fingerprint != second[i].Fingerprint {
			t.Errorf("position %d: first=%q second=%q (unstable under tied created_at)",
				i, first[i].Fingerprint, second[i].Fingerprint)
		}
	}

	// And it must be deterministic per insertion order: the rowid tiebreak
	// is DESC to match the primary created_at DESC, so the last-inserted
	// fingerprint (fp-e) must come back first.
	want := []string{"fp-e", "fp-d", "fp-c", "fp-b", "fp-a"}
	for i, w := range want {
		if first[i].Fingerprint != w {
			t.Errorf("position %d: got %q, want %q (rowid tiebreak broken)", i, first[i].Fingerprint, w)
		}
	}
}
