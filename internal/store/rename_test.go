package store

import (
	"context"
	"fmt"
	"testing"

	"github.com/dpoage/bugbot/internal/domain"
)

// fixedLocusResolver returns a resolve func that ignores the file argument and
// always yields the same locus for a given line, mirroring how a pure rename
// (no content change) resolves identically at the enclosing symbol regardless
// of path.
func fixedLocusResolver(locusByLine map[int]string) func(file string, line int) string {
	return func(_ string, line int) string {
		if l, ok := locusByLine[line]; ok {
			return l
		}
		return fmt.Sprintf("L:%d", line)
	}
}

// TestRenameFindingIdentity_RewritesOpenFindingAndSuppression is the headline
// behaviour: renaming a file carrying an open finding and a suppression must
// rewrite both onto the new path's identity, with no duplicate finding and the
// suppression still honored on a subsequent re-discovery at the new path.
func TestRenameFindingIdentity_RewritesOpenFindingAndSuppression(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	const oldFile, newFile = "internal/old/handler.go", "internal/new/handler.go"
	locus := "S:func\x00Handle"
	oldFP := domain.Fingerprint("nil-safety", oldFile, locus)

	seeded, err := st.UpsertFinding(ctx, domain.Finding{
		Fingerprint: oldFP,
		LocusKey:    domain.LocusKey(oldFile, locus),
		Title:       "possible nil deref",
		Severity:    "high",
		Tier:        domain.TierSuspected,
		Lens:        "nil-safety",
		File:        oldFile,
		Line:        42,
	})
	if err != nil {
		t.Fatalf("seed finding: %v", err)
	}
	if err := st.UpdateStatus(ctx, oldFP, domain.StatusDismissed, "false positive"); err != nil {
		t.Fatalf("dismiss: %v", err)
	}

	resolve := fixedLocusResolver(map[int]string{42: locus})
	n, err := st.RenameFindingIdentity(ctx, oldFile, newFile, resolve)
	if err != nil {
		t.Fatalf("RenameFindingIdentity: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 finding rewritten, got %d", n)
	}

	newFP := domain.Fingerprint("nil-safety", newFile, locus)
	if newFP == oldFP {
		t.Fatalf("test setup invalid: old and new fingerprints must differ")
	}

	// Old identity must be gone: no row, no suppression.
	if _, err := st.GetFindingByFingerprint(ctx, oldFP); err != ErrNotFound {
		t.Fatalf("old fingerprint should be gone, got err=%v", err)
	}
	if sup, _ := st.IsSuppressed(ctx, oldFP); sup {
		t.Fatal("old fingerprint should no longer be marked suppressed")
	}

	// New identity carries the row and the dismissal forward.
	got, err := st.GetFindingByFingerprint(ctx, newFP)
	if err != nil {
		t.Fatalf("GetFindingByFingerprint(new): %v", err)
	}
	if got.ID != seeded.ID {
		t.Fatalf("rewrite should preserve finding id: want %q got %q", seeded.ID, got.ID)
	}
	if got.File != newFile {
		t.Fatalf("file not rewritten: got %q", got.File)
	}
	if got.Status != domain.StatusDismissed {
		t.Fatalf("dismissed status should survive rename, got %q", got.Status)
	}
	sup, err := st.IsSuppressed(ctx, newFP)
	if err != nil {
		t.Fatalf("IsSuppressed(new): %v", err)
	}
	if !sup {
		t.Fatal("suppression should carry forward to the new fingerprint")
	}

	// A "rescan" at the new path must not resurrect it as a duplicate open
	// finding: suppression memory forces it back to dismissed.
	rescanned, err := st.UpsertFinding(ctx, domain.Finding{
		Fingerprint: newFP,
		LocusKey:    domain.LocusKey(newFile, locus),
		Title:       "possible nil deref",
		Severity:    "high",
		Tier:        domain.TierSuspected,
		Lens:        "nil-safety",
		File:        newFile,
		Line:        42,
		Status:      domain.StatusOpen,
	})
	if err != nil {
		t.Fatalf("rescan upsert: %v", err)
	}
	if rescanned.ID != seeded.ID {
		t.Fatalf("rescan should fold onto the same finding id, got a new one: %q vs %q", rescanned.ID, seeded.ID)
	}
	if rescanned.Status != domain.StatusDismissed {
		t.Fatalf("rescanned finding must stay dismissed, got %q", rescanned.Status)
	}
	all, err := st.ListFindings(ctx, domain.FindingFilter{})
	if err != nil {
		t.Fatalf("ListFindings: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected exactly 1 finding after rename+rescan, got %d", len(all))
	}
}

// TestRenameFindingIdentity_IdempotentOnReplay covers the crash-replay
// contract (bugbot-r4x3): applying the same rename twice must not double-hash,
// error, or produce a second row.
func TestRenameFindingIdentity_IdempotentOnReplay(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	const oldFile, newFile = "pkg/a.go", "pkg/b.go"
	locus := "L:10"
	oldFP := domain.Fingerprint("race", oldFile, locus)
	seeded, err := st.UpsertFinding(ctx, domain.Finding{
		Fingerprint: oldFP,
		LocusKey:    domain.LocusKey(oldFile, locus),
		Title:       "data race",
		Severity:    "medium",
		Tier:        domain.TierSuspected,
		Lens:        "race",
		File:        oldFile,
		Line:        10,
	})
	if err != nil {
		t.Fatalf("seed finding: %v", err)
	}

	resolve := fixedLocusResolver(map[int]string{10: locus})

	n1, err := st.RenameFindingIdentity(ctx, oldFile, newFile, resolve)
	if err != nil {
		t.Fatalf("first RenameFindingIdentity: %v", err)
	}
	if n1 != 1 {
		t.Fatalf("expected 1 rewrite on first pass, got %d", n1)
	}

	// Replay the exact same rename over the exact same range: no matching row
	// remains at oldFile, so this must be a clean no-op, not an error.
	n2, err := st.RenameFindingIdentity(ctx, oldFile, newFile, resolve)
	if err != nil {
		t.Fatalf("replayed RenameFindingIdentity: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("expected 0 rewrites on replay, got %d", n2)
	}

	newFP := domain.Fingerprint("race", newFile, locus)
	got, err := st.GetFindingByFingerprint(ctx, newFP)
	if err != nil {
		t.Fatalf("GetFindingByFingerprint(new): %v", err)
	}
	if got.ID != seeded.ID {
		t.Fatalf("id drifted across replay: want %q got %q", seeded.ID, got.ID)
	}
	all, err := st.ListFindings(ctx, domain.FindingFilter{})
	if err != nil {
		t.Fatalf("ListFindings: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("replay must not create a second row, got %d findings", len(all))
	}
}

// TestRenameFindingIdentity_NoMatchIsNoop covers the boundary cases: empty
// paths, identical paths, and a rename with no findings at the old path all
// return zero rewrites without error.
func TestRenameFindingIdentity_NoMatchIsNoop(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)
	resolve := fixedLocusResolver(nil)

	cases := []struct{ old, new string }{
		{"", "a.go"},
		{"a.go", ""},
		{"a.go", "a.go"},
		{"unrelated/old.go", "unrelated/new.go"},
	}
	for _, c := range cases {
		n, err := st.RenameFindingIdentity(ctx, c.old, c.new, resolve)
		if err != nil {
			t.Fatalf("RenameFindingIdentity(%q, %q): %v", c.old, c.new, err)
		}
		if n != 0 {
			t.Fatalf("RenameFindingIdentity(%q, %q) = %d, want 0", c.old, c.new, n)
		}
	}
}
