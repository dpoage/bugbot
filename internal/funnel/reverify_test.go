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
	fi := domain.Finding{
		Fingerprint: domain.Fingerprint("nil-safety/error-handling", "bug.go", NewLocusResolver(repo.Root()).Resolve("bug.go", 10)),
		Title:       "orphan T3 needs re-verification",
		Description: "left orphaned by a prior hard-budget stop",
		Severity:    domain.SeverityHigh,
		Tier:        domain.TierSuspected,
		Status:      domain.StatusOpen,
		Lens:        "nil-safety/error-handling",
		File:        "bug.go",
		Line:        10,
	}
	return seedFinding(t, st, fi)
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

	got, err := st.GetFindingByFingerprint(ctx, domain.Fingerprint("nil-safety/error-handling", "bug.go", NewLocusResolver(repo.Root()).Resolve("bug.go", 10)))
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
	got, err := st.GetFindingByFingerprint(ctx, domain.Fingerprint("nil-safety/error-handling", "bug.go", NewLocusResolver(repo.Root()).Resolve("bug.go", 10)))
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
