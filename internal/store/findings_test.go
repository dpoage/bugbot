package store

import (
	"context"
	"errors"
	"testing"
)

func sampleFinding() Finding {
	fp := Fingerprint("race", "internal/x/y.go", 10, "data race on counter")
	return Finding{
		Fingerprint: fp,
		Title:       "data race on counter",
		Description: "counter incremented without a lock",
		Reasoning:   "verifier confirmed concurrent writes",
		Severity:    "high",
		Tier:        2,
		Status:      StatusOpen,
		Lens:        "race",
		File:        "internal/x/y.go",
		Line:        10,
		CommitSHA:   "abc123",
		FileHash:    "hash-v1",
	}
}

func TestUpsertFinding_InsertThenDedupUpdate(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	f := sampleFinding()
	got, err := st.UpsertFinding(ctx, f)
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if got.ID == "" {
		t.Fatal("expected generated id")
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatal("timestamps should be set")
	}
	firstID := got.ID
	firstCreated := got.CreatedAt

	// Re-find the same bug (same fingerprint) with new code anchor + tier.
	f2 := f
	f2.Tier = 1
	f2.CommitSHA = "def456"
	f2.FileHash = "hash-v2"
	f2.ReproPath = "/tmp/repro_test.go"
	got2, err := st.UpsertFinding(ctx, f2)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	// Must update in place: same id and created_at, no duplicate row.
	if got2.ID != firstID {
		t.Fatalf("dedup failed: id changed %s -> %s", firstID, got2.ID)
	}
	if !got2.CreatedAt.Equal(firstCreated) {
		t.Fatalf("created_at should be preserved: %v -> %v", firstCreated, got2.CreatedAt)
	}
	if got2.Tier != 1 || got2.CommitSHA != "def456" || got2.FileHash != "hash-v2" {
		t.Fatalf("mutable fields not updated: %+v", got2)
	}
	if got2.ReproPath != "/tmp/repro_test.go" {
		t.Fatalf("repro_path not stored: %q", got2.ReproPath)
	}

	all, err := st.ListFindings(ctx, FindingFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected exactly 1 finding (deduped), got %d", len(all))
	}
}

func TestUpsertFinding_CorroboratingLensesRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	f := sampleFinding()
	f.CorroboratingLenses = []string{"concurrency", "resource-leaks"}
	f.Reasoning = "Survived adversarial verification.\nCorroborated by lenses: concurrency, resource-leaks"

	stored, err := st.UpsertFinding(ctx, f)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if want := []string{"concurrency", "resource-leaks"}; !equalStrings(stored.CorroboratingLenses, want) {
		t.Fatalf("upsert returned corroborating = %v, want %v", stored.CorroboratingLenses, want)
	}

	// Read back via GetFinding: the column must round-trip.
	got, err := st.GetFinding(ctx, stored.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if want := []string{"concurrency", "resource-leaks"}; !equalStrings(got.CorroboratingLenses, want) {
		t.Errorf("GetFinding corroborating = %v, want %v", got.CorroboratingLenses, want)
	}

	// And via ListFindings (uses the same scan path).
	all, err := st.ListFindings(ctx, FindingFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("want 1 finding, got %d", len(all))
	}
	if want := []string{"concurrency", "resource-leaks"}; !equalStrings(all[0].CorroboratingLenses, want) {
		t.Errorf("ListFindings corroborating = %v, want %v", all[0].CorroboratingLenses, want)
	}

	// Updating to no corroboration must clear it (empty -> nil), confirming the
	// empty-string encode/decode round-trips too.
	f2 := f
	f2.CorroboratingLenses = nil
	if _, err := st.UpsertFinding(ctx, f2); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	got2, err := st.GetFinding(ctx, stored.ID)
	if err != nil {
		t.Fatalf("get after clear: %v", err)
	}
	if len(got2.CorroboratingLenses) != 0 {
		t.Errorf("corroborating after clear = %v, want empty", got2.CorroboratingLenses)
	}
}

// A lens name containing a comma must not split into phantom entries on
// read-back; encode sanitizes the comma to a semicolon so the list length is
// preserved and the corruption is visible rather than silent.
func TestEncodeLenses_CommaSanitized(t *testing.T) {
	got := decodeLenses(encodeLenses([]string{"type-safety,bounds", "concurrency"}))
	want := []string{"type-safety;bounds", "concurrency"}
	if !equalStrings(got, want) {
		t.Errorf("round-trip = %v, want %v", got, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestUpsertFinding_RequiresFingerprint(t *testing.T) {
	st := openTemp(t)
	_, err := st.UpsertFinding(context.Background(), Finding{Title: "x"})
	if err == nil {
		t.Fatal("expected error for missing fingerprint")
	}
}

func TestGetFinding_NotFound(t *testing.T) {
	st := openTemp(t)
	_, err := st.GetFinding(context.Background(), "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestListFindings_Filters(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	mk := func(lens, file string, line, tier int, status Status, commit string) {
		f := Finding{
			Fingerprint: Fingerprint(lens, file, line, "t"),
			Title:       "t",
			Tier:        tier,
			Status:      status,
			Lens:        lens,
			File:        file,
			Line:        line,
			CommitSHA:   commit,
		}
		if _, err := st.UpsertFinding(ctx, f); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}
	mk("a", "f1.go", 1, 1, StatusOpen, "c1")
	mk("a", "f2.go", 2, 2, StatusOpen, "c1")
	mk("b", "f3.go", 3, 2, StatusFixed, "c2")

	open, err := st.ListFindings(ctx, FindingFilter{Status: StatusOpen})
	if err != nil {
		t.Fatalf("list open: %v", err)
	}
	if len(open) != 2 {
		t.Fatalf("expected 2 open, got %d", len(open))
	}

	tier2, err := st.ListFindings(ctx, FindingFilter{Tier: 2})
	if err != nil {
		t.Fatalf("list tier2: %v", err)
	}
	if len(tier2) != 2 {
		t.Fatalf("expected 2 tier-2, got %d", len(tier2))
	}

	// Code-version scoping: only findings anchored to commit c1.
	c1, err := st.ListFindings(ctx, FindingFilter{CommitSHA: "c1"})
	if err != nil {
		t.Fatalf("list c1: %v", err)
	}
	if len(c1) != 2 {
		t.Fatalf("expected 2 findings at commit c1, got %d", len(c1))
	}

	lensB, err := st.ListFindings(ctx, FindingFilter{Lens: "b"})
	if err != nil {
		t.Fatalf("list lens b: %v", err)
	}
	if len(lensB) != 1 {
		t.Fatalf("expected 1 finding for lens b, got %d", len(lensB))
	}
}

func TestMarkFixed(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	f := sampleFinding()
	if _, err := st.UpsertFinding(ctx, f); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := st.MarkFixed(ctx, f.Fingerprint); err != nil {
		t.Fatalf("mark fixed: %v", err)
	}
	got, err := st.GetFindingByFingerprint(ctx, f.Fingerprint)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != StatusFixed {
		t.Fatalf("expected fixed, got %q", got.Status)
	}
}

func TestUpdateStatus_NotFound(t *testing.T) {
	st := openTemp(t)
	err := st.UpdateStatus(context.Background(), "missing", StatusFixed, "")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestFixPatchNeedsHuman_RoundTrip verifies that FixPatch and NeedsHuman
// persist through insert, update, and all read paths.
func TestFixPatchNeedsHuman_RoundTrip(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	f := sampleFinding()
	f.FixPatch = "--- a/calc.go\n+++ b/calc.go\n@@ -1 +1 @@\n-return 0\n+return 1\n"
	f.NeedsHuman = false

	stored, err := st.UpsertFinding(ctx, f)
	if err != nil {
		t.Fatalf("upsert with fix_patch: %v", err)
	}
	if stored.FixPatch != f.FixPatch {
		t.Errorf("upsert returned fix_patch = %q, want %q", stored.FixPatch, f.FixPatch)
	}
	if stored.NeedsHuman {
		t.Errorf("upsert returned needs_human = true, want false")
	}

	// Read back via GetFinding.
	got, err := st.GetFinding(ctx, stored.ID)
	if err != nil {
		t.Fatalf("GetFinding: %v", err)
	}
	if got.FixPatch != f.FixPatch {
		t.Errorf("GetFinding fix_patch = %q, want %q", got.FixPatch, f.FixPatch)
	}
	if got.NeedsHuman {
		t.Errorf("GetFinding needs_human = true, want false")
	}

	// Update: set NeedsHuman=true, clear FixPatch.
	f2 := stored
	f2.NeedsHuman = true
	f2.FixPatch = ""
	updated, err := st.UpsertFinding(ctx, f2)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if !updated.NeedsHuman {
		t.Errorf("updated needs_human = false, want true")
	}
	if updated.FixPatch != "" {
		t.Errorf("updated fix_patch = %q, want empty", updated.FixPatch)
	}

	// Read back the updated values via GetFindingByFingerprint.
	got2, err := st.GetFindingByFingerprint(ctx, f.Fingerprint)
	if err != nil {
		t.Fatalf("GetFindingByFingerprint: %v", err)
	}
	if !got2.NeedsHuman {
		t.Errorf("GetFindingByFingerprint needs_human = false, want true")
	}
	if got2.FixPatch != "" {
		t.Errorf("GetFindingByFingerprint fix_patch = %q, want empty", got2.FixPatch)
	}

	// ListFindings must also return the updated values.
	all, err := st.ListFindings(ctx, FindingFilter{})
	if err != nil {
		t.Fatalf("ListFindings: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("want 1 finding, got %d", len(all))
	}
	if !all[0].NeedsHuman {
		t.Errorf("ListFindings needs_human = false, want true")
	}

	// Tier-0 round-trip: the tier column stores 0 correctly.
	f3 := stored
	f3.Tier = 0
	f3.FixPatch = "--- a/x.go\n+++ b/x.go\n@@ -1 +1 @@\n-bad\n+good\n"
	if _, err := st.UpsertFinding(ctx, f3); err != nil {
		t.Fatalf("tier-0 upsert: %v", err)
	}
	got3, err := st.GetFindingByFingerprint(ctx, f.Fingerprint)
	if err != nil {
		t.Fatalf("tier-0 get: %v", err)
	}
	if got3.Tier != 0 {
		t.Errorf("tier = %d, want 0", got3.Tier)
	}
	if got3.FixPatch != f3.FixPatch {
		t.Errorf("tier-0 fix_patch = %q, want %q", got3.FixPatch, f3.FixPatch)
	}
}

// Document the re-verification flow as an executable example: after a commit
// changes a file, the daemon finds open findings whose stored file_hash differs
// from the file's current hash and re-checks only those.
func TestReVerificationFlow_DetectsChangedFindings(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	f := sampleFinding() // FileHash "hash-v1", File internal/x/y.go
	if _, err := st.UpsertFinding(ctx, f); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Daemon's incremental scan computes current hashes for files on disk.
	currentHashes := map[string]string{"internal/x/y.go": "hash-v2"}

	open, err := st.ListFindings(ctx, FindingFilter{Status: StatusOpen})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var needsReverify []Finding
	for _, fnd := range open {
		if h, ok := currentHashes[fnd.File]; ok && h != fnd.FileHash {
			needsReverify = append(needsReverify, fnd)
		}
	}
	if len(needsReverify) != 1 {
		t.Fatalf("expected 1 finding needing re-verification, got %d", len(needsReverify))
	}
}
