package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/store"
)

// ---------------------------------------------------------------------------
// Rename tracking (bugbot-ezmx.6): a git rename detected within a poll's
// commit range must rewrite the moved file's finding/suppression identity so
// neither resurfaces as a duplicate/unsuppressed row under the new path.
// ---------------------------------------------------------------------------

// TestDaemonPollRewritesRenamedFindingIdentity is the headline acceptance
// scenario: a file carrying an open, dismissed finding is renamed; the next
// poll cycle must rewrite that finding onto the new path (no duplicate row)
// with the suppression still honored, even though the scripted finder finds
// nothing new at the new path.
func TestDaemonPollRewritesRenamedFindingIdentity(t *testing.T) {
	const newFile = "renamed.go"

	fr := newFixtureRepo(t)
	fr.write(fixtureFile, "package p\n\nfunc f(x *int) int { return *x }\n")
	base := fr.commit("init")

	st := openStore(t)
	if _, err := st.BeginScanRun(context.Background(), store.ScanSweep, base); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertFileStates(context.Background(), []store.FileState{{
		Path: lastSeenSentinel, ContentHash: base, LastScannedCommit: base,
	}}); err != nil {
		t.Fatal(err)
	}

	// Seed an open finding at the original path and dismiss it, mirroring a
	// maintainer having already triaged this as a false positive.
	locus := funnel.NewLocusResolver(fr.dir).Resolve(fixtureFile, fixtureLine)
	oldFP := domain.Fingerprint("nil-deref", fixtureFile, locus)
	seeded, err := st.UpsertFinding(context.Background(), domain.Finding{
		Fingerprint: oldFP,
		LocusKey:    domain.LocusKey(fixtureFile, locus),
		Title:       "possible nil dereference",
		Severity:    "high",
		Tier:        2,
		Status:      domain.StatusOpen,
		Lens:        "nil-deref",
		File:        fixtureFile,
		Line:        fixtureLine,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateStatus(context.Background(), oldFP, domain.StatusDismissed, "false positive"); err != nil {
		t.Fatal(err)
	}

	// Rename the file (pure rename, no content edit) and commit.
	fr.git("mv", fixtureFile, newFile)
	fr.commit("rename bug.go -> renamed.go")

	// Scripted finder reports nothing new, isolating this test to the rename
	// rewrite rather than the scan's own dedup behavior.
	llmc := newFakeLLM(emptyJSON, notRefutedJSON)
	clk := newFakeClock(mustTime(t, testStart))
	cfg := DaemonConfig{
		PollInterval:   10 * time.Millisecond,
		SweepInterval:  time.Hour,
		PerCycleTokens: 1_000_000,
		PerDayTokens:   10_000_000,
	}
	d, _ := buildDaemon(t, fr, st, llmc, cfg, clk)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runInBackground(ctx, d)

	if !clk.fire(ctx, t) {
		t.Fatal("clock fire failed")
	}
	waitFor(t, func() bool { return countScanRuns(t, st, store.ScanTargeted) >= 1 })

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	// Pre-v3 seeded row (empty DefectKind/Subject): rename rewrites onto the
	// v3-scheme fingerprint with kind/subject passed through as empty.
	newFP := domain.FingerprintV3(newFile, locus, "", "")

	// Old identity gone.
	if _, err := st.GetFindingByFingerprint(context.Background(), oldFP); err != store.ErrNotFound {
		t.Fatalf("old fingerprint should be gone after rename, got err=%v", err)
	}

	// New identity carries the same finding forward, still dismissed.
	got, err := st.GetFindingByFingerprint(context.Background(), newFP)
	if err != nil {
		t.Fatalf("GetFindingByFingerprint(new): %v", err)
	}
	if got.ID != seeded.ID {
		t.Fatalf("rename should preserve finding id: want %q got %q", seeded.ID, got.ID)
	}
	if got.File != newFile {
		t.Fatalf("file not rewritten: got %q", got.File)
	}
	if got.Status != domain.StatusDismissed {
		t.Fatalf("dismissed status must survive the rename, got %q", got.Status)
	}
	sup, err := st.IsSuppressed(context.Background(), newFP, "")
	if err != nil {
		t.Fatal(err)
	}
	if !sup {
		t.Fatal("suppression must carry forward to the new fingerprint")
	}

	// No duplicate: exactly one finding row total.
	all, err := st.ListFindings(context.Background(), domain.FindingFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("expected exactly 1 finding after rename, got %d", len(all))
	}
}

// TestDaemonApplyRenamesIdempotentOnReplay drives the daemon's rename entry
// point (applyRenames) directly over the same commit range twice, mirroring a
// crash-replay where the watermark commit range is reprocessed (bugbot-r4x3).
// The second pass must be a clean no-op: no error, no second row, no drift.
func TestDaemonApplyRenamesIdempotentOnReplay(t *testing.T) {
	const newFile = "renamed.go"

	fr := newFixtureRepo(t)
	fr.write(fixtureFile, "package p\n\nfunc f(x *int) int { return *x }\n")
	base := fr.commit("init")

	st := openStore(t)
	locus := funnel.NewLocusResolver(fr.dir).Resolve(fixtureFile, fixtureLine)
	oldFP := domain.Fingerprint("nil-deref", fixtureFile, locus)
	seeded, err := st.UpsertFinding(context.Background(), domain.Finding{
		Fingerprint: oldFP,
		LocusKey:    domain.LocusKey(fixtureFile, locus),
		Title:       "possible nil dereference",
		Severity:    "high",
		Tier:        2,
		Status:      domain.StatusOpen,
		Lens:        "nil-deref",
		File:        fixtureFile,
		Line:        fixtureLine,
	})
	if err != nil {
		t.Fatal(err)
	}

	fr.git("mv", fixtureFile, newFile)
	head := fr.commit("rename bug.go -> renamed.go")

	d, err := New(Deps{
		Repo:    fr.open(),
		Store:   st,
		Clients: funnel.RoleClients{Finder: newFakeLLM(emptyJSON, notRefutedJSON), Verifier: newFakeLLM(emptyJSON, notRefutedJSON)},
		Logger:  discardLogger(),
	}, DaemonConfig{PollInterval: time.Hour, SweepInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	d.applyRenames(ctx, base, head)
	d.applyRenames(ctx, base, head) // replay: must be a no-op, not an error

	// Pre-v3 seeded row: same v3-scheme-with-empty-kind/subject rewrite.
	newFP := domain.FingerprintV3(newFile, locus, "", "")
	got, err := st.GetFindingByFingerprint(ctx, newFP)
	if err != nil {
		t.Fatalf("GetFindingByFingerprint(new): %v", err)
	}
	if got.ID != seeded.ID {
		t.Fatalf("id drifted across replay: want %q got %q", seeded.ID, got.ID)
	}
	all, err := st.ListFindings(ctx, domain.FindingFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("replay must not create a second row, got %d findings", len(all))
	}
}
