package store

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/dpoage/bugbot/internal/domain"
)

func sampleFinding() domain.Finding {
	fp := domain.Fingerprint("race", "internal/x/y.go", fmt.Sprintf("%d|%s", 10, "data race on counter"))
	return domain.Finding{
		Fingerprint: fp,
		Title:       "data race on counter",
		Description: "counter incremented without a lock",
		Reasoning:   "verifier confirmed concurrent writes",
		Severity:    "high",
		Tier:        2,
		Status:      domain.StatusOpen,
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

	all, err := st.ListFindings(ctx, domain.FindingFilter{})
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
	all, err := st.ListFindings(ctx, domain.FindingFilter{})
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

// JSON encoding preserves lens names verbatim, including commas — they no longer
// need sanitization. Verify round-trip fidelity for a lens containing a comma.
func TestEncodeLenses_JSONRoundTrip(t *testing.T) {
	input := []string{"type-safety,bounds", "concurrency"}
	got := decodeLenses(encodeLenses(input))
	if !equalStrings(got, input) {
		t.Errorf("JSON round-trip = %v, want %v", got, input)
	}
}

// decodeLenses must fall back to comma-split for legacy rows (no leading '[').
func TestDecodeLenses_LegacyCommaFallback(t *testing.T) {
	legacy := "alpha,beta,gamma"
	got := decodeLenses(legacy)
	want := []string{"alpha", "beta", "gamma"}
	if !equalStrings(got, want) {
		t.Errorf("legacy decode = %v, want %v", got, want)
	}
}

// decodeLenses must parse JSON-encoded rows produced by the new encoder.
func TestDecodeLenses_JSONEncoded(t *testing.T) {
	encoded := encodeLenses([]string{"foo", "bar,baz"})
	got := decodeLenses(encoded)
	want := []string{"foo", "bar,baz"}
	if !equalStrings(got, want) {
		t.Errorf("JSON decode = %v, want %v", got, want)
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
	_, err := st.UpsertFinding(context.Background(), domain.Finding{Title: "x"})
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

	mk := func(lens, file string, line int, tier domain.Tier, status domain.Status, commit string) {
		f := domain.Finding{
			Fingerprint: domain.Fingerprint(lens, file, fmt.Sprintf("%d|%s", line, "t")),
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
	mk("a", "f1.go", 1, 2, domain.StatusOpen, "c1") // was tier=1; tests filter/sort, not tier
	mk("a", "f2.go", 2, 2, domain.StatusOpen, "c1")
	mk("b", "f3.go", 3, 2, domain.StatusFixed, "c2")

	open, err := st.ListFindings(ctx, domain.FindingFilter{Status: domain.StatusOpen})
	if err != nil {
		t.Fatalf("list open: %v", err)
	}
	if len(open) != 2 {
		t.Fatalf("expected 2 open, got %d", len(open))
	}

	tier2, err := st.ListFindings(ctx, domain.FindingFilter{HasTier: true, Tier: 2})
	if err != nil {
		t.Fatalf("list tier2: %v", err)
	}
	if len(tier2) != 3 {
		t.Fatalf("expected 3 tier-2, got %d", len(tier2))
	}

	// Code-version scoping: only findings anchored to commit c1.
	c1, err := st.ListFindings(ctx, domain.FindingFilter{CommitSHA: "c1"})
	if err != nil {
		t.Fatalf("list c1: %v", err)
	}
	if len(c1) != 2 {
		t.Fatalf("expected 2 findings at commit c1, got %d", len(c1))
	}

	lensB, err := st.ListFindings(ctx, domain.FindingFilter{Lens: "b"})
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
	if got.Status != domain.StatusFixed {
		t.Fatalf("expected fixed, got %q", got.Status)
	}
}

func TestUpdateStatus_NotFound(t *testing.T) {
	st := openTemp(t)
	err := st.UpdateStatus(context.Background(), "missing", domain.StatusFixed, "")
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
	f2.NeedsHumanReason = domain.NeedsHumanReasonBelowQuorum
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
	all, err := st.ListFindings(ctx, domain.FindingFilter{})
	if err != nil {
		t.Fatalf("ListFindings: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("want 1 finding, got %d", len(all))
	}
	if !all[0].NeedsHuman {
		t.Errorf("ListFindings needs_human = false, want true")
	}

	// Tier-0 round-trip: use a fresh finding (no NeedsHuman history, since the
	// stored row above has NeedsHuman=true which the UPDATE path preserves, and
	// T0+NeedsHuman is an illegal state).
	f3 := sampleFinding()
	f3.Fingerprint = domain.Fingerprint("race", "internal/x/z.go", "10|t0-test")
	f3.Tier = 0
	f3.ReproPath = "/artifacts/fix.go"
	f3.FixPatch = "--- a/x.go\n+++ b/x.go\n@@ -1 +1 @@\n-bad\n+good\n"
	if _, err := st.UpsertFinding(ctx, f3); err != nil {
		t.Fatalf("tier-0 upsert: %v", err)
	}
	got3, err := st.GetFindingByFingerprint(ctx, f3.Fingerprint)
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

	open, err := st.ListFindings(ctx, domain.FindingFilter{Status: domain.StatusOpen})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var needsReverify []domain.Finding
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
	rescan := domain.Finding{
		Fingerprint: f.Fingerprint,
		Title:       f.Title,
		Description: f.Description,
		Reasoning:   "updated reasoning from re-scan",
		Severity:    "critical", // changed
		Tier:        2,
		Status:      domain.StatusOpen,
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
	if got.Status != domain.StatusFixed {
		t.Errorf("after MarkFixed: status=%q, want %q", got.Status, domain.StatusFixed)
	}
	// Tier is unchanged by MarkFixed — it operates only on status.
	if got.Tier != f.Tier {
		t.Errorf("MarkFixed must not change tier: got %d, want %d", got.Tier, f.Tier)
	}

	// UpdateStatus to dismissed also works.
	if err := st.UpdateStatus(ctx, f.Fingerprint, domain.StatusDismissed, "test dismissal"); err != nil {
		t.Fatalf("UpdateStatus dismissed: %v", err)
	}
	got2, err := st.GetFindingByFingerprint(ctx, f.Fingerprint)
	if err != nil {
		t.Fatalf("GetFindingByFingerprint after dismiss: %v", err)
	}
	if got2.Status != domain.StatusDismissed {
		t.Errorf("after UpdateStatus dismissed: status=%q, want %q", got2.Status, domain.StatusDismissed)
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
	setNH.NeedsHumanReason = domain.NeedsHumanReasonProverExhausted
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

	seed := func(file string, line int, tier domain.Tier, status domain.Status, needsHuman bool) {
		t.Helper()
		f := domain.Finding{
			Fingerprint: domain.Fingerprint("l", file, fmt.Sprintf("%d|%s", line, "t")),
			Title:       "t", Severity: "high", Tier: tier, Status: status,
			Lens: "l", File: file, Line: line, NeedsHuman: needsHuman,
		}
		if needsHuman {
			f.NeedsHumanReason = domain.NeedsHumanReasonBelowQuorum
		}
		// T0/T1 require ReproPath; supply a placeholder so FSM guard passes.
		if tier <= 1 {
			f.ReproPath = "/artifacts/placeholder"
		}
		if _, err := st.UpsertFinding(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	seed("a.go", 1, 0, domain.StatusOpen, false)
	seed("a.go", 2, 2, domain.StatusOpen, true) // was tier=1; NeedsHuman+T2 = below-quorum
	seed("a.go", 3, 2, domain.StatusOpen, false)
	seed("a.go", 4, 2, domain.StatusOpen, false)
	seed("b.go", 1, 2, domain.StatusFixed, false)
	seed("b.go", 2, 3, domain.StatusDismissed, false)

	got, err := st.CountFindings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.OpenByTier[0] != 1 || got.OpenByTier[1] != 0 || got.OpenByTier[2] != 3 {
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
	f.ReproPath = "/artifacts/repro.go"
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
	list, err := st.ListFindings(ctx, domain.FindingFilter{})
	if err != nil {
		t.Fatalf("ListFindings: %v", err)
	}
	if len(list) != 1 || list[0].Confidence != stored.Confidence {
		t.Errorf("ListFindings confidence = %v, want %v", list[0].Confidence, stored.Confidence)
	}
}

// TestUpdateFindingSeverity verifies that UpdateFindingSeverity persists
// severity + verdict_detail, recomputes Confidence, and round-trips through
// GetFinding. It also verifies ErrNotFound for an unknown id.
func TestUpdateFindingSeverity(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	// Insert a finding at medium severity.
	f := sampleFinding()
	f.Severity = domain.SeverityMedium
	stored, err := st.UpsertFinding(ctx, f)
	if err != nil {
		t.Fatalf("UpsertFinding: %v", err)
	}
	originalConf := stored.Confidence

	// Re-rank to low with a rationale.
	const rationale = "zero non-test callers of processBuffer found by AST reference scan"
	if err := st.UpdateFindingSeverity(ctx, stored.ID, domain.SeverityLow, rationale); err != nil {
		t.Fatalf("UpdateFindingSeverity: %v", err)
	}

	// Round-trip through GetFinding.
	got, err := st.GetFinding(ctx, stored.ID)
	if err != nil {
		t.Fatalf("GetFinding: %v", err)
	}
	if got.Severity != domain.SeverityLow {
		t.Errorf("Severity = %s, want low", got.Severity)
	}
	if got.VerdictDetail != rationale {
		t.Errorf("VerdictDetail = %q, want %q", got.VerdictDetail, rationale)
	}
	// Confidence must have been recomputed and differ from the medium-severity value.
	if got.Confidence == originalConf {
		t.Errorf("Confidence unchanged after severity downrank: %v", got.Confidence)
	}
	// Confidence for low severity must be less than for medium (same tier, no corroboration).
	if got.Confidence >= originalConf {
		t.Errorf("Confidence after downrank (%v) must be < original (%v)", got.Confidence, originalConf)
	}

	// ErrNotFound for an unknown id.
	err = st.UpdateFindingSeverity(ctx, "nonexistent-id", domain.SeverityHigh, "irrelevant")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound for unknown id, got %v", err)
	}
}

// TestFindingSites_EncodeDecode verifies the encode/decode round-trip for the
// Sites column, including paths with pipes and the empty case.
func TestFindingSites_EncodeDecode(t *testing.T) {
	cases := []struct {
		name  string
		sites []domain.Site
	}{
		{"nil", nil},
		{"empty", []domain.Site{}},
		{"single", []domain.Site{{File: "internal/cli/publish.go", Line: 45}}},
		{"multi", []domain.Site{
			{File: "internal/cli/publish.go", Line: 45},
			{File: "internal/cli/publish.go", Line: 78},
			{File: "internal/cli/publish.go", Line: 112},
		}},
		{"with pipe in path", []domain.Site{
			{File: "src/foo|bar.go", Line: 1},
			{File: "src/baz.go", Line: 2},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			enc := encodeSites(tc.sites)
			got := decodeSites(enc)
			// Normalize: nil and empty both decode as nil.
			if len(tc.sites) == 0 {
				if got != nil {
					t.Errorf("want nil, got %+v", got)
				}
				return
			}
			if len(got) != len(tc.sites) {
				t.Fatalf("len = %d, want %d; encoded=%q decoded=%+v", len(got), len(tc.sites), enc, got)
			}
			for i, want := range tc.sites {
				if got[i] != want {
					t.Errorf("[%d] got %+v, want %+v", i, got[i], want)
				}
			}
		})
	}
}

// TestUpsertFinding_SitesRoundTrip verifies Sites persist through
// UpsertFinding and are recovered by GetFinding.
func TestUpsertFinding_SitesRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	sites := []domain.Site{
		{File: "internal/cli/publish.go", Line: 45},
		{File: "internal/cli/publish.go", Line: 78},
		{File: "internal/cli/publish.go", Line: 112},
	}

	f := sampleFinding()
	f.Sites = sites
	stored, err := st.UpsertFinding(ctx, f)
	if err != nil {
		t.Fatalf("UpsertFinding: %v", err)
	}
	if len(stored.Sites) != len(sites) {
		t.Fatalf("stored Sites len = %d, want %d", len(stored.Sites), len(sites))
	}
	for i, want := range sites {
		if stored.Sites[i] != want {
			t.Errorf("stored.Sites[%d] = %+v, want %+v", i, stored.Sites[i], want)
		}
	}

	// Recover via GetFinding.
	got, err := st.GetFinding(ctx, stored.ID)
	if err != nil {
		t.Fatalf("GetFinding: %v", err)
	}
	if len(got.Sites) != len(sites) {
		t.Fatalf("retrieved Sites len = %d, want %d", len(got.Sites), len(sites))
	}
	for i, want := range sites {
		if got.Sites[i] != want {
			t.Errorf("retrieved Sites[%d] = %+v, want %+v", i, got.Sites[i], want)
		}
	}
}

// TestMigration013_SitesColumn verifies that the 013 migration adds the sites
// column and that a finding inserted after migration round-trips Sites.
func TestMigration013_SitesColumn(t *testing.T) {
	ctx := context.Background()
	// openTemp applies all migrations including 013.
	st := openTemp(t)

	sites := []domain.Site{
		{File: "src/RenderSystem.cpp", Line: 42},
		{File: "src/RenderSystem.hpp", Line: 15},
	}
	f := domain.Finding{
		Fingerprint: domain.Fingerprint("boundary-conditions", "src/RenderSystem.cpp", fmt.Sprintf("%d|%s", 42, "buffer overflow")),
		Title:       "buffer overflow",
		Description: "write past array end",
		Severity:    domain.SeverityHigh,
		Tier:        domain.TierVerified,
		Status:      domain.StatusOpen,
		Lens:        "boundary-conditions",
		File:        "src/RenderSystem.cpp",
		Line:        42,
		CommitSHA:   "cafebabe",
		FileHash:    "h1",
		Sites:       sites,
	}
	stored, err := st.UpsertFinding(ctx, f)
	if err != nil {
		t.Fatalf("UpsertFinding: %v", err)
	}
	got, err := st.GetFinding(ctx, stored.ID)
	if err != nil {
		t.Fatalf("GetFinding: %v", err)
	}
	if len(got.Sites) != 2 {
		t.Fatalf("Sites len = %d, want 2: %+v", len(got.Sites), got.Sites)
	}
	if got.Sites[0].File != "src/RenderSystem.cpp" || got.Sites[0].Line != 42 {
		t.Errorf("Sites[0] = %+v", got.Sites[0])
	}
	if got.Sites[1].File != "src/RenderSystem.hpp" || got.Sites[1].Line != 15 {
		t.Errorf("Sites[1] = %+v", got.Sites[1])
	}
}

// TestUpsertFinding_PreservesWitnessOnRescan covers bugbot-w1bh: a below-quorum
// (NeedsHuman) finding can carry a non-promoting repro witness (ReproWitness),
// and a later re-scan with an empty incoming witness must NOT clear the stored
// one — the same preservation rule as ReproPath. Tier and ReproPath stay empty
// (a witness never promotes).
func TestUpsertFinding_PreservesWitnessOnRescan(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	// 1. Initial upsert: a below-quorum survivor (tier 2, NeedsHuman, no repro).
	f := sampleFinding()
	f.Tier = 2
	f.NeedsHuman = true
	f.NeedsHumanReason = domain.NeedsHumanReasonBelowQuorum
	f.ReproPath = ""
	f.ReproWitness = ""
	stored, err := st.UpsertFinding(ctx, f)
	if err != nil {
		t.Fatalf("initial upsert: %v", err)
	}

	// 2. Witness: simulate witnessFinding — set ReproWitness only, leaving Tier,
	// ReproPath, and NeedsHuman as-is.
	witnessed := stored
	witnessed.ReproWitness = "/artifacts/witness/repro_test.go"
	after, err := st.UpsertFinding(ctx, witnessed)
	if err != nil {
		t.Fatalf("witness upsert: %v", err)
	}
	if after.ReproWitness != "/artifacts/witness/repro_test.go" {
		t.Fatalf("after witness: repro_witness=%q, want the bundle path", after.ReproWitness)
	}
	if after.Tier != 2 {
		t.Errorf("witness changed tier: got %d, want 2 (a witness must not promote)", after.Tier)
	}
	if after.ReproPath != "" {
		t.Errorf("witness set repro_path=%q, want empty (a witness must not promote)", after.ReproPath)
	}
	if !after.NeedsHuman {
		t.Errorf("witness cleared needs_human, want still true")
	}

	// 3. Re-scan: a fresh T2 finding with an EMPTY witness for the same
	// fingerprint. The stored witness must survive.
	rescan := domain.Finding{
		Fingerprint: f.Fingerprint,
		Title:       f.Title,
		Description: f.Description,
		Reasoning:   "updated reasoning from re-scan",
		Severity:    "critical", // changed
		Tier:        2,
		Status:      domain.StatusOpen,
		Lens:        f.Lens,
		File:        f.File,
		Line:        f.Line,
		CommitSHA:   "newcommit",
		FileHash:    "hash-v3",
		NeedsHuman:  true,
		// domain.NeedsHumanReason not set: UpsertFinding preserves stored reason on update.
		ReproWitness: "", // empty — must NOT clobber the stored witness
	}
	rescanned, err := st.UpsertFinding(ctx, rescan)
	if err != nil {
		t.Fatalf("rescan upsert: %v", err)
	}
	if rescanned.ReproWitness != "/artifacts/witness/repro_test.go" {
		t.Errorf("rescan CLEARED repro_witness: got %q, want preserved", rescanned.ReproWitness)
	}
	if rescanned.Severity != "critical" {
		t.Errorf("severity not updated: %q", rescanned.Severity)
	}

	// Read back from DB to confirm persistence.
	dbRow, err := st.GetFindingByFingerprint(ctx, f.Fingerprint)
	if err != nil {
		t.Fatalf("GetFindingByFingerprint: %v", err)
	}
	if dbRow.ReproWitness != "/artifacts/witness/repro_test.go" {
		t.Errorf("DB repro_witness=%q after rescan, want preserved", dbRow.ReproWitness)
	}
}
