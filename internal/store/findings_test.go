package store

import (
	"context"
	"errors"
	"testing"

	"github.com/dpoage/bugbot/internal/domain"
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

	mk := func(lens, file string, line int, tier domain.Tier, status Status, commit string) {
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

// TestUpsertFinding_PreservesPromotionOnRescan verifies that an implicit re-scan
// upsert (tier=2, repro_path="") never regresses a finding that was already
// promoted to tier=1 with a repro artifact. The freshness fields (reasoning,
// severity, updated_at) must still update.
func TestUpsertFinding_PreservesPromotionOnRescan(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	// 1. Initial upsert: tier=2 (verified), no repro.
	f := sampleFinding()
	stored, err := st.UpsertFinding(ctx, f)
	if err != nil {
		t.Fatalf("initial upsert: %v", err)
	}

	// 2. Promote: simulate what promoteFinding does — read current row, set tier=1
	// and repro_path, then upsert.
	promoted := stored
	promoted.Tier = 1
	promoted.ReproPath = "/artifacts/repro_test.go"
	promoted.NeedsHuman = false
	after, err := st.UpsertFinding(ctx, promoted)
	if err != nil {
		t.Fatalf("promotion upsert: %v", err)
	}
	if after.Tier != 1 {
		t.Fatalf("after promotion: tier=%d, want 1", after.Tier)
	}
	if after.ReproPath != "/artifacts/repro_test.go" {
		t.Fatalf("after promotion: repro_path=%q, want /artifacts/repro_test.go", after.ReproPath)
	}

	// 3. Re-scan: a fresh T2 finding with empty ReproPath and updated freshness
	// fields arrives for the same fingerprint.
	rescan := Finding{
		Fingerprint: f.Fingerprint,
		Title:       f.Title,
		Description: f.Description,
		Reasoning:   "updated reasoning from re-scan",
		Severity:    "critical", // changed
		Tier:        2,
		Status:      StatusOpen,
		Lens:        f.Lens,
		File:        f.File,
		Line:        f.Line,
		CommitSHA:   "newcommit",
		FileHash:    "hash-v3",
		ReproPath:   "", // empty — must NOT clobber stored /artifacts/repro_test.go
	}
	rescanned, err := st.UpsertFinding(ctx, rescan)
	if err != nil {
		t.Fatalf("rescan upsert: %v", err)
	}

	// Promotion state must be preserved.
	if rescanned.Tier != 1 {
		t.Errorf("rescan DEMOTED tier: got %d, want 1 (promotion must be preserved)", rescanned.Tier)
	}
	if rescanned.ReproPath != "/artifacts/repro_test.go" {
		t.Errorf("rescan CLEARED repro_path: got %q, want /artifacts/repro_test.go", rescanned.ReproPath)
	}

	// Freshness fields must have updated.
	if rescanned.Reasoning != "updated reasoning from re-scan" {
		t.Errorf("reasoning not updated: %q", rescanned.Reasoning)
	}
	if rescanned.Severity != "critical" {
		t.Errorf("severity not updated: %q", rescanned.Severity)
	}

	// Read back from DB to confirm it matches the returned struct.
	dbRow, err := st.GetFindingByFingerprint(ctx, f.Fingerprint)
	if err != nil {
		t.Fatalf("GetFindingByFingerprint: %v", err)
	}
	if dbRow.Tier != 1 {
		t.Errorf("DB tier=%d after rescan, want 1", dbRow.Tier)
	}
	if dbRow.ReproPath != "/artifacts/repro_test.go" {
		t.Errorf("DB repro_path=%q after rescan, want /artifacts/repro_test.go", dbRow.ReproPath)
	}
	if dbRow.Reasoning != "updated reasoning from re-scan" {
		t.Errorf("DB reasoning not updated: %q", dbRow.Reasoning)
	}
}

// TestUpsertFinding_ExplicitDemotionStillWorks verifies that MarkFixed and
// UpdateStatus (the explicit mutation paths) still work correctly — they do NOT
// route through UpsertFinding's promotion-preserving UPDATE, so they are
// unaffected by the guard.
func TestUpsertFinding_ExplicitDemotionStillWorks(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	f := sampleFinding()
	if _, err := st.UpsertFinding(ctx, f); err != nil {
		t.Fatalf("initial upsert: %v", err)
	}

	// MarkFixed changes status without touching tier.
	if err := st.MarkFixed(ctx, f.Fingerprint); err != nil {
		t.Fatalf("MarkFixed: %v", err)
	}
	got, err := st.GetFindingByFingerprint(ctx, f.Fingerprint)
	if err != nil {
		t.Fatalf("GetFindingByFingerprint: %v", err)
	}
	if got.Status != StatusFixed {
		t.Errorf("after MarkFixed: status=%q, want %q", got.Status, StatusFixed)
	}
	// Tier is unchanged by MarkFixed — it operates only on status.
	if got.Tier != f.Tier {
		t.Errorf("MarkFixed must not change tier: got %d, want %d", got.Tier, f.Tier)
	}

	// UpdateStatus to dismissed also works.
	if err := st.UpdateStatus(ctx, f.Fingerprint, StatusDismissed, "test dismissal"); err != nil {
		t.Fatalf("UpdateStatus dismissed: %v", err)
	}
	got2, err := st.GetFindingByFingerprint(ctx, f.Fingerprint)
	if err != nil {
		t.Fatalf("GetFindingByFingerprint after dismiss: %v", err)
	}
	if got2.Status != StatusDismissed {
		t.Errorf("after UpdateStatus dismissed: status=%q, want %q", got2.Status, StatusDismissed)
	}
}

// TestUpsertFinding_TierEdgeCases covers the tier-direction edge cases:
//   - incoming T1 on stored T2 → promotes to T1 (MIN in correct direction)
//   - incoming non-empty repro_path on stored non-empty → replaces (genuine re-repro)
//   - needs_human set then cleared by re-scan → preserved
func TestUpsertFinding_TierEdgeCases(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	// Seed a T2 verified finding.
	f := sampleFinding()
	if _, err := st.UpsertFinding(ctx, f); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Edge 1: incoming T1 on stored T2 → should promote to T1.
	promote := f
	promote.Tier = 1
	promote.ReproPath = "/artifacts/first.go"
	got, err := st.UpsertFinding(ctx, promote)
	if err != nil {
		t.Fatalf("T1 upsert: %v", err)
	}
	if got.Tier != 1 {
		t.Errorf("T1 promote: tier=%d, want 1", got.Tier)
	}

	// Edge 2: incoming non-empty repro_path replaces stored non-empty (re-repro).
	rerepro := f
	rerepro.Tier = 1
	rerepro.ReproPath = "/artifacts/second.go"
	got2, err := st.UpsertFinding(ctx, rerepro)
	if err != nil {
		t.Fatalf("re-repro upsert: %v", err)
	}
	if got2.ReproPath != "/artifacts/second.go" {
		t.Errorf("re-repro: repro_path=%q, want /artifacts/second.go", got2.ReproPath)
	}

	// Edge 3: needs_human set, then re-scan with needs_human=false → must stay true.
	setNH := f
	setNH.Tier = 1
	setNH.ReproPath = "/artifacts/second.go"
	setNH.NeedsHuman = true
	if _, err := st.UpsertFinding(ctx, setNH); err != nil {
		t.Fatalf("set needs_human: %v", err)
	}
	clearNH := f
	clearNH.Tier = 2
	clearNH.ReproPath = ""
	clearNH.NeedsHuman = false // re-scan would produce false
	got3, err := st.UpsertFinding(ctx, clearNH)
	if err != nil {
		t.Fatalf("rescan clear needs_human: %v", err)
	}
	if !got3.NeedsHuman {
		t.Errorf("needs_human cleared by re-scan; must be preserved once set")
	}
	// Verify DB also reflects preserved needs_human.
	dbRow, err := st.GetFindingByFingerprint(ctx, f.Fingerprint)
	if err != nil {
		t.Fatalf("GetFindingByFingerprint: %v", err)
	}
	if !dbRow.NeedsHuman {
		t.Errorf("DB needs_human cleared; must be preserved")
	}
}

// TestCountFindings covers the status-pane tally aggregation.
func TestCountFindings(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	seed := func(file string, line int, tier domain.Tier, status Status, needsHuman bool) {
		t.Helper()
		f := Finding{
			Fingerprint: Fingerprint("l", file, line, "t"),
			Title:       "t", Severity: "high", Tier: tier, Status: status,
			Lens: "l", File: file, Line: line, NeedsHuman: needsHuman,
		}
		if _, err := st.UpsertFinding(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	seed("a.go", 1, 0, StatusOpen, false)
	seed("a.go", 2, 1, StatusOpen, true)
	seed("a.go", 3, 2, StatusOpen, false)
	seed("a.go", 4, 2, StatusOpen, false)
	seed("b.go", 1, 2, StatusFixed, false)
	seed("b.go", 2, 3, StatusDismissed, false)

	got, err := st.CountFindings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.OpenByTier[0] != 1 || got.OpenByTier[1] != 1 || got.OpenByTier[2] != 2 {
		t.Errorf("OpenByTier = %v", got.OpenByTier)
	}
	if got.NeedsHuman != 1 {
		t.Errorf("NeedsHuman = %d, want 1", got.NeedsHuman)
	}
	if got.Fixed != 1 || got.Dismissed != 1 {
		t.Errorf("fixed=%d dismissed=%d, want 1/1", got.Fixed, got.Dismissed)
	}
}

// TestFindingConfidence_Monotonic verifies the monotonicity and boundedness
// properties of findingConfidence:
//   - higher tier number (weaker evidence) => lower confidence
//   - more corroborating lenses => higher confidence
//   - all values in [0,1]
func TestFindingConfidence_Monotonic(t *testing.T) {
	// Tier monotonicity: tier 1 > tier 2 > tier 3.
	c1 := findingConfidence(domain.TierReproduced, "high", 0)
	c2 := findingConfidence(domain.TierVerified, "high", 0)
	c3 := findingConfidence(domain.TierSuspected, "high", 0)
	if !(c1 > c2 && c2 > c3) {
		t.Errorf("tier monotonicity violated: c1=%v c2=%v c3=%v", c1, c2, c3)
	}

	// Corroboration monotonicity: more lenses => higher confidence (same tier+severity).
	c0 := findingConfidence(domain.TierVerified, "medium", 0)
	cA := findingConfidence(domain.TierVerified, "medium", 1)
	cB := findingConfidence(domain.TierVerified, "medium", 2)
	if !(cB > cA && cA > c0) {
		t.Errorf("corroboration monotonicity violated: c0=%v cA=%v cB=%v", c0, cA, cB)
	}

	// Bounded [0,1] across a range of inputs.
	for _, tier := range []domain.Tier{0, 1, 2, 3, 99} {
		for _, corrob := range []int{0, 1, 5, 100} {
			v := findingConfidence(tier, "critical", corrob)
			if v < 0 || v > 1 {
				t.Errorf("out of range: findingConfidence(%d, critical, %d) = %v", tier, corrob, v)
			}
		}
	}
}

// TestFindingConfidence_FixWitnessedStrongest pins the tier-0 correction: a
// fix-witnessed finding must score higher than an otherwise-identical reproduced finding.
func TestFindingConfidence_FixWitnessedStrongest(t *testing.T) {
	c0 := findingConfidence(domain.TierFixWitnessed, "high", 0)
	c1 := findingConfidence(domain.TierReproduced, "high", 0)
	if c0 <= c1 {
		t.Errorf("TierFixWitnessed confidence (%v) should be > TierReproduced (%v)", c0, c1)
	}
}

// TestFindingConfidence_CorroborationIncreases verifies that otherwise-identical
// findings have higher confidence when corroborating_lenses is non-empty.
func TestFindingConfidence_CorroborationIncreases(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	base := sampleFinding()
	base.CorroboratingLenses = nil
	stored0, err := st.UpsertFinding(ctx, base)
	if err != nil {
		t.Fatalf("UpsertFinding (no corrob): %v", err)
	}

	// Same fingerprint but with corroborating lenses — triggers UPDATE path.
	updated := base
	updated.CorroboratingLenses = []string{"concurrency", "resource-leaks"}
	stored2, err := st.UpsertFinding(ctx, updated)
	if err != nil {
		t.Fatalf("UpsertFinding (2 corrob): %v", err)
	}

	if stored2.Confidence <= stored0.Confidence {
		t.Errorf("confidence did not increase with corroboration: before=%v after=%v",
			stored0.Confidence, stored2.Confidence)
	}
}

// TestFindingConfidence_RoundTrip verifies that Confidence is persisted and
// returned correctly from all read paths.
func TestFindingConfidence_RoundTrip(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	f := sampleFinding()
	f.Tier = 1
	f.Severity = "critical"
	f.CorroboratingLenses = []string{"concurrency"}

	stored, err := st.UpsertFinding(ctx, f)
	if err != nil {
		t.Fatalf("UpsertFinding: %v", err)
	}
	if stored.Confidence <= 0 {
		t.Errorf("stored confidence should be > 0, got %v", stored.Confidence)
	}

	// Round-trip via GetFinding.
	got, err := st.GetFinding(ctx, stored.ID)
	if err != nil {
		t.Fatalf("GetFinding: %v", err)
	}
	if got.Confidence != stored.Confidence {
		t.Errorf("GetFinding confidence = %v, want %v", got.Confidence, stored.Confidence)
	}

	// Round-trip via GetFindingByFingerprint.
	gotFP, err := st.GetFindingByFingerprint(ctx, stored.Fingerprint)
	if err != nil {
		t.Fatalf("GetFindingByFingerprint: %v", err)
	}
	if gotFP.Confidence != stored.Confidence {
		t.Errorf("GetFindingByFingerprint confidence = %v, want %v", gotFP.Confidence, stored.Confidence)
	}

	// Round-trip via ListFindings.
	list, err := st.ListFindings(ctx, FindingFilter{})
	if err != nil {
		t.Fatalf("ListFindings: %v", err)
	}
	if len(list) != 1 || list[0].Confidence != stored.Confidence {
		t.Errorf("ListFindings confidence = %v, want %v", list[0].Confidence, stored.Confidence)
	}
}
