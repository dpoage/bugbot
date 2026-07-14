package funnel

import (
	"context"
	"testing"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/store"
)

// seedSuspectedFinding inserts a durable OPEN Tier-3 suspected finding against
// bug.go (present in the fixture repo) so ReverifySuspected can pick it up.
// The fingerprint is built from the enclosing-symbol locus (via NewLocusResolver,
// the same path triage uses), so the triage consumer's recompute dedups onto this
// exact row when the verifier promotes (UpsertFinding match) or dismisses.
func seedSuspectedFinding(t *testing.T, st *store.Store, repo *ingest.Repo) domain.Finding {
	t.Helper()
	locus := NewLocusResolver(repo.Root()).Resolve("bug.go", 10)
	fi := domain.Finding{
		Fingerprint: domain.FingerprintV3("bug.go", locus, domain.DefectNilDeref, "Greeting"),
		Title:       "orphan T3 needs re-verification",
		Description: "left orphaned by a prior hard-budget stop",
		Severity:    domain.SeverityHigh,
		Tier:        domain.TierSuspected,
		Status:      domain.StatusOpen,
		Lens:        "nil-safety/error-handling",
		File:        "bug.go",
		Line:        10,
		LocusKey:    domain.LocusKey("bug.go", locus),
		DefectKind:  domain.DefectNilDeref,
		Subject:     "Greeting",
	}
	return seedFinding(t, st, fi)
}

// seedPreV3SuspectedFinding inserts a durable OPEN Tier-3 suspected finding
// that predates Fingerprint v3: its Fingerprint is the v2 scheme
// (domain.Fingerprint(lens,file,locus)) and DefectKind/Subject are empty,
// exactly as any T3 row persisted before bugbot-ezmx.1 shipped would be.
func seedPreV3SuspectedFinding(t *testing.T, st *store.Store, repo *ingest.Repo) domain.Finding {
	t.Helper()
	locus := NewLocusResolver(repo.Root()).Resolve("bug.go", 10)
	fi := domain.Finding{
		Fingerprint: domain.Fingerprint("nil-safety/error-handling", "bug.go", locus),
		Title:       "pre-v3 orphan T3 needs re-verification",
		Description: "left orphaned by a prior hard-budget stop, before defect_kind/subject existed",
		Severity:    domain.SeverityHigh,
		Tier:        domain.TierSuspected,
		Status:      domain.StatusOpen,
		Lens:        "nil-safety/error-handling",
		File:        "bug.go",
		Line:        10,
		LocusKey:    domain.LocusKey("bug.go", locus),
		// DefectKind/Subject intentionally left empty: this is what every
		// pre-migration row looks like.
	}
	return seedFinding(t, st, fi)
}

// TestReverifySuspected_PreV3Row_KeepsStoredFingerprint pins bugbot-ezmx.1
// finding 5: reverifying a PRE-v3 suspected finding (empty DefectKind/Subject,
// v2-scheme Fingerprint) must NOT re-mint a v3 fingerprint from the
// reconstructed candidate's empty kind/subject — that would produce a
// DIFFERENT fingerprint than the stored row's, so UpsertFinding would insert
// a NEW row instead of promoting the existing one, silently duplicating the
// finding (the exact failure this epic exists to remove) and leaving the
// original T3 row stranded open forever. triageState.process's `!c.Reverify`
// guard keeps a Reverify candidate's stored fingerprint verbatim; this test
// proves the stored v2 fingerprint is still resolvable (and promoted) after
// ReverifySuspected runs, and that no second row was created.
func TestReverifySuspected_PreV3Row_KeepsStoredFingerprint(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	seeded := seedPreV3SuspectedFinding(t, st, repo)

	finder := newScriptedClient()
	finder.fallback = candJSON(realCand) // if invoked, would return a candidate

	verifier := newScriptedClient()
	verifier.fallback = notRefutedJSON // every seat says survive

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = f.Close() }()

	if _, err := f.ReverifySuspected(ctx); err != nil {
		t.Fatalf("ReverifySuspected: %v", err)
	}
	if n := finder.callCount(); n != 0 {
		t.Errorf("finder.callCount() = %d, want 0 (modeReverify must skip the finder)", n)
	}

	// The ORIGINAL v2-scheme fingerprint must still resolve, now promoted —
	// proving the row was UPDATED IN PLACE, not orphaned by a re-mint.
	got, err := st.GetFindingByFingerprint(ctx, seeded.Fingerprint)
	if err != nil {
		t.Fatalf("GetFindingByFingerprint(stored v2 fingerprint): %v (the row was orphaned — a NEW fingerprint was minted instead of reusing the stored one)", err)
	}
	if got.Tier != domain.TierVerified {
		t.Errorf("after ReverifySurvivor: Tier=%v, want TierVerified", got.Tier)
	}
	if got.Status != domain.StatusOpen {
		t.Errorf("after ReverifySurvivor: Status=%q, want %q", got.Status, domain.StatusOpen)
	}
	if got.ID != seeded.ID {
		t.Errorf("promoted row ID = %q, want the original seeded row's ID %q (a different ID means a duplicate was created)", got.ID, seeded.ID)
	}

	// No second (duplicate) row should exist for this locus.
	open, err := st.ListFindings(ctx, domain.FindingFilter{Status: domain.StatusOpen})
	if err != nil {
		t.Fatalf("ListFindings: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("open findings = %d, want 1 (a second count means a duplicate row was inserted): %+v", len(open), open)
	}
}

// TestReverifySuspected_PromotesSurvivor seeds an OPEN T3 suspected finding
// on bug.go (present in the fixture repo), runs ReverifySuspected with a
// verifier that returns not-refuted for every seat, and asserts:
//   - the durable row is promoted to Tier 2 (verified) and stays open
//   - the finder was NEVER invoked (callCount == 0)
func TestReverifySuspected_PromotesSurvivor(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	seedSuspectedFinding(t, st, repo)

	finder := newScriptedClient()
	finder.fallback = candJSON(realCand) // if invoked, would return a candidate

	verifier := newScriptedClient()
	verifier.fallback = notRefutedJSON // every seat says survive

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = f.Close() }()

	res, err := f.ReverifySuspected(ctx)
	if err != nil {
		t.Fatalf("ReverifySuspected: %v", err)
	}
	if res == nil {
		t.Fatal("ReverifySuspected returned nil result")
	}

	// Finder must never have been invoked.
	if n := finder.callCount(); n != 0 {
		t.Errorf("finder.callCount() = %d, want 0 (modeReverify must skip the finder)", n)
	}

	got, err := st.GetFindingByFingerprint(ctx, domain.FingerprintV3("bug.go", NewLocusResolver(repo.Root()).Resolve("bug.go", 10), domain.DefectNilDeref, "Greeting"))
	if err != nil {
		t.Fatalf("GetFindingByFingerprint: %v", err)
	}
	if got.Tier != domain.TierVerified {
		t.Errorf("after ReverifySurvivor: Tier=%v, want TierVerified", got.Tier)
	}
	if got.Status != domain.StatusOpen {
		t.Errorf("after ReverifySurvivor: Status=%q, want %q", got.Status, domain.StatusOpen)
	}
}

// TestReverifySuspected_DismissesRefuted seeds an OPEN T3 suspected finding
// and runs ReverifySuspected with a verifier that returns refuted. The
// verify-stream KILL region's Reverify branch must dismiss the durable row
// (the normal kill path persists nothing; reverify needs to close the row).
func TestReverifySuspected_DismissesRefuted(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	seedSuspectedFinding(t, st, repo)

	finder := newScriptedClient()
	finder.fallback = candJSON(realCand)

	verifier := newScriptedClient()
	verifier.fallback = refutedJSON // every seat refutes

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = f.Close() }()

	if _, err := f.ReverifySuspected(ctx); err != nil {
		t.Fatalf("ReverifySuspected: %v", err)
	}

	if n := finder.callCount(); n != 0 {
		t.Errorf("finder.callCount() = %d, want 0 (modeReverify must skip the finder)", n)
	}

	// The T3 row must be dismissed (not left open).
	got, err := st.GetFindingByFingerprint(ctx, domain.FingerprintV3("bug.go", NewLocusResolver(repo.Root()).Resolve("bug.go", 10), domain.DefectNilDeref, "Greeting"))
	if err != nil {
		t.Fatalf("GetFindingByFingerprint: %v", err)
	}
	if got.Status != domain.StatusDismissed {
		t.Errorf("after ReverifySurvivor refuted: Status=%q, want %q", got.Status, domain.StatusDismissed)
	}
}

// TestReverifySuspected_Empty verifies that an empty durable-T3 store is a
// single-query no-op: empty Result, no LLM calls at all.
func TestReverifySuspected_Empty(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	finder := newScriptedClient()
	verifier := newScriptedClient()

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = f.Close() }()

	res, err := f.ReverifySuspected(ctx)
	if err != nil {
		t.Fatalf("ReverifySuspected: %v", err)
	}
	if res == nil {
		t.Fatal("ReverifySuspected returned nil result")
	}
	if res.Stats.Resumed != 0 {
		t.Errorf("Stats.Resumed = %d, want 0", res.Stats.Resumed)
	}

	if n := finder.callCount(); n != 0 {
		t.Errorf("finder.callCount() = %d, want 0", n)
	}
	if n := verifier.callCount(); n != 0 {
		t.Errorf("verifier.callCount() = %d, want 0 (no T3s to judge)", n)
	}
}

// seedUnderValidatedFinding inserts a durable OPEN Tier-2 finding against
// bug.go whose genuine-verdict count is below MinReviewerValidation, so
// ReverifyUnderValidated can pick it up. Same locus/fingerprint recipe as
// seedSuspectedFinding so triage dedups the replay onto this exact row.
func seedUnderValidatedFinding(t *testing.T, st *store.Store, repo *ingest.Repo, genuine int, needsHuman bool) domain.Finding {
	t.Helper()
	locus := NewLocusResolver(repo.Root()).Resolve("bug.go", 10)
	fi := domain.Finding{
		Fingerprint:     domain.FingerprintV3("bug.go", locus, domain.DefectNilDeref, "Greeting"),
		Title:           "T2 survivor of a degraded 1-seat panel",
		Description:     "verified while the budget was degraded; only one reviewer judged it",
		Severity:        domain.SeverityHigh,
		Tier:            domain.TierVerified,
		Status:          domain.StatusOpen,
		Lens:            "nil-safety/error-handling",
		File:            "bug.go",
		Line:            10,
		LocusKey:        domain.LocusKey("bug.go", locus),
		DefectKind:      domain.DefectNilDeref,
		Subject:         "Greeting",
		GenuineVerdicts: genuine,
	}
	if needsHuman {
		fi.NeedsHuman = true
		fi.NeedsHumanReason = domain.NeedsHumanReasonBelowQuorum
	}
	return seedFinding(t, st, fi)
}

// TestReverifyUnderValidated_ReachesMinimumAndConverges seeds an OPEN T2
// finding validated by a single reviewer, runs ReverifyUnderValidated with a
// verifier that returns not-refuted for every seat, and asserts:
//   - the finder was NEVER invoked (drain is verify-only)
//   - the row stays open T2 and its genuine-verdict count reaches the
//     3-reviewer minimum (the full default panel produced 3 genuine verdicts)
//   - a SECOND run is a no-op (zero further verifier calls): the finding
//     reached the minimum and dropped out of the WorkRemaining query.
func TestReverifyUnderValidated_ReachesMinimumAndConverges(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	seeded := seedUnderValidatedFinding(t, st, repo, 1, false)

	finder := newScriptedClient()
	finder.fallback = candJSON(realCand)

	verifier := newScriptedClient()
	verifier.fallback = notRefutedJSON

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = f.Close() }()

	res, err := f.ReverifyUnderValidated(ctx)
	if err != nil {
		t.Fatalf("ReverifyUnderValidated: %v", err)
	}
	if res.Stats.Resumed != 1 {
		t.Errorf("Stats.Resumed = %d, want 1", res.Stats.Resumed)
	}
	if n := finder.callCount(); n != 0 {
		t.Errorf("finder.callCount() = %d, want 0 (modeRevalidate must skip the finder)", n)
	}

	got, err := st.GetFindingByFingerprint(ctx, seeded.Fingerprint)
	if err != nil {
		t.Fatalf("GetFindingByFingerprint: %v", err)
	}
	if got.Status != domain.StatusOpen || got.Tier != domain.TierVerified {
		t.Errorf("after revalidation: Status=%q Tier=%v, want open T2", got.Status, got.Tier)
	}
	if got.GenuineVerdicts < MinReviewerValidation {
		t.Errorf("after revalidation: GenuineVerdicts = %d, want >= %d (full panel must record its genuine-verdict count)", got.GenuineVerdicts, MinReviewerValidation)
	}
	if got.ID != seeded.ID {
		t.Errorf("revalidated row ID = %q, want the seeded row's ID %q (a different ID means a duplicate was created)", got.ID, seeded.ID)
	}

	// Convergence: the finding now meets the minimum, so a second drain must
	// issue no further verifier calls.
	callsAfterFirst := verifier.callCount()
	if _, err := f.ReverifyUnderValidated(ctx); err != nil {
		t.Fatalf("second ReverifyUnderValidated: %v", err)
	}
	if n := verifier.callCount(); n != callsAfterFirst {
		t.Errorf("second run issued %d further verifier calls, want 0 (drain must converge once the minimum is reached)", n-callsAfterFirst)
	}
}

// TestReverifyUnderValidated_DismissesRefuted seeds an under-validated OPEN
// T2 finding and runs the drain with a verifier that refutes: the
// Candidate.Reverify kill path must dismiss the durable row instead of
// leaving it open forever.
func TestReverifyUnderValidated_DismissesRefuted(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	seeded := seedUnderValidatedFinding(t, st, repo, 1, false)

	finder := newScriptedClient()
	finder.fallback = candJSON(realCand)

	verifier := newScriptedClient()
	verifier.fallback = refutedJSON

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = f.Close() }()

	if _, err := f.ReverifyUnderValidated(ctx); err != nil {
		t.Fatalf("ReverifyUnderValidated: %v", err)
	}

	got, err := st.GetFindingByFingerprint(ctx, seeded.Fingerprint)
	if err != nil {
		t.Fatalf("GetFindingByFingerprint: %v", err)
	}
	if got.Status != domain.StatusDismissed {
		t.Errorf("after refuted revalidation: Status=%q, want %q", got.Status, domain.StatusDismissed)
	}
}

// TestReverifyUnderValidated_ClearsBelowQuorumFlag seeds a below-quorum
// NeedsHuman T2 survivor. A revalidation panel where every seat produces a
// genuine verdict meets the quorum floor, which RESOLVES the below-quorum
// cause: the sticky needs_human upsert guard must be released through the
// targeted store path.
func TestReverifyUnderValidated_ClearsBelowQuorumFlag(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	seeded := seedUnderValidatedFinding(t, st, repo, 1, true)

	finder := newScriptedClient()
	finder.fallback = candJSON(realCand)

	verifier := newScriptedClient()
	verifier.fallback = notRefutedJSON

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = f.Close() }()

	if _, err := f.ReverifyUnderValidated(ctx); err != nil {
		t.Fatalf("ReverifyUnderValidated: %v", err)
	}

	got, err := st.GetFindingByFingerprint(ctx, seeded.Fingerprint)
	if err != nil {
		t.Fatalf("GetFindingByFingerprint: %v", err)
	}
	if got.NeedsHuman || got.NeedsHumanReason != domain.NeedsHumanReasonNone {
		t.Errorf("below-quorum flag survived a full-quorum revalidation: NeedsHuman=%v reason=%q", got.NeedsHuman, got.NeedsHumanReason)
	}
	if got.Status != domain.StatusOpen || got.Tier != domain.TierVerified {
		t.Errorf("after revalidation: Status=%q Tier=%v, want open T2", got.Status, got.Tier)
	}
}

// TestReverifyUnderValidated_SkipsWhenPanelCannotConverge: with Refuters
// explicitly configured below MinReviewerValidation, every rerun would record
// a count that can never reach the minimum — the drain must be a deliberate
// no-op (no snapshot, no LLM calls) instead of re-spending every invocation.
func TestReverifyUnderValidated_SkipsWhenPanelCannotConverge(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	seeded := seedUnderValidatedFinding(t, st, repo, 1, false)

	finder := newScriptedClient()
	verifier := newScriptedClient()

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{Limits: StageLimits{Refuters: 1}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = f.Close() }()

	res, err := f.ReverifyUnderValidated(ctx)
	if err != nil {
		t.Fatalf("ReverifyUnderValidated: %v", err)
	}
	if res == nil {
		t.Fatal("ReverifyUnderValidated returned nil result")
	}
	if n := verifier.callCount(); n != 0 {
		t.Errorf("verifier.callCount() = %d, want 0 (sub-minimum panel must skip the drain)", n)
	}

	got, err := st.GetFindingByFingerprint(ctx, seeded.Fingerprint)
	if err != nil {
		t.Fatalf("GetFindingByFingerprint: %v", err)
	}
	if got.GenuineVerdicts != 1 {
		t.Errorf("GenuineVerdicts = %d, want 1 (untouched)", got.GenuineVerdicts)
	}
}

// TestReverifyUnderValidated_Empty verifies that a store whose open T2
// findings all meet the minimum is a single-query no-op: no snapshot, no LLM
// calls.
func TestReverifyUnderValidated_Empty(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	seedUnderValidatedFinding(t, st, repo, MinReviewerValidation, false)

	finder := newScriptedClient()
	verifier := newScriptedClient()

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = f.Close() }()

	res, err := f.ReverifyUnderValidated(ctx)
	if err != nil {
		t.Fatalf("ReverifyUnderValidated: %v", err)
	}
	if res.Stats.Resumed != 0 {
		t.Errorf("Stats.Resumed = %d, want 0", res.Stats.Resumed)
	}
	if n := verifier.callCount(); n != 0 {
		t.Errorf("verifier.callCount() = %d, want 0 (nothing under-validated)", n)
	}
	if n := finder.callCount(); n != 0 {
		t.Errorf("finder.callCount() = %d, want 0", n)
	}
}
