package funnel

import (
	"context"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/ingest"
)

// TestVerifyStream_InlineSeverityDownrank proves bugbot-596: a Tier-2 survivor
// is persisted with its reachability-VALIDATED severity and a swept_at stamp
// INLINE at persist time — without any post-scan SweepDrain / impactSweep pass.
//
// The fixture has an UNEXPORTED Go function deadHelper with zero non-test
// callers, so classifyReachability deterministically downranks it
// (reachKnownDownrank -> low) with no LLM call. The finder emits a candidate at
// that function; the verifier never refutes, so it survives as T2. f.Sweep
// alone (no drainToFixpoint, no SweepDrain) must yield a finding that is already
// low AND swept — i.e. validateSeverityInline ran on the verify-and-persist
// path. Before the fix the survivor would carry its raw finder severity (high)
// and an unset swept_at until the bulk drain reconciled it.
func TestVerifyStream_InlineSeverityDownrank(t *testing.T) {
	ctx := context.Background()
	st := openImpactStore(t)
	repoDir := makeGitRepo(t, map[string]string{
		// deadHelper is unexported and called by nobody; main does not reference
		// it. Zero non-test callers -> deterministic downrank to low.
		"main.go": "package main\n\nfunc deadHelper(cfg *int) int {\n\treturn *cfg\n}\n\nfunc main() {\n\tprintln(\"hi\")\n}\n",
	})
	repo, err := ingest.Open(ctx, repoDir)
	if err != nil {
		t.Skipf("ingest.Open: %v (git required)", err)
	}

	deadCand := `{"file": "main.go", "line": 4, "title": "nil deref of cfg in deadHelper",
		"description": "cfg may be nil", "severity": "high",
		"evidence": "deadHelper returns *cfg without a nil check", "confidence": "high"}`
	finder := newScriptedClient().onSystemContains("nil-safety/error-handling", candJSON(deadCand))
	verifier := newScriptedClient()
	verifier.fallback = notRefutedJSON // every refuter seat votes not-refuted -> survives

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo,
		Options{Discovery: DiscoveryConfig{Lenses: []string{"nil-safety/error-handling"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })

	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	var got *domain.Finding
	for i := range res.Findings {
		if strings.Contains(res.Findings[i].Title, "deadHelper") {
			got = &res.Findings[i]
		}
	}
	if got == nil {
		t.Fatalf("no survived finding for deadHelper; got %d findings: %+v", len(res.Findings), res.Findings)
	}
	if got.Tier != domain.TierVerified {
		t.Fatalf("tier = %d, want %d (T2 survivor)", got.Tier, domain.TierVerified)
	}
	// Inline reachability validation downranked the raw finder severity (high) to low.
	if got.Severity != domain.SeverityLow {
		t.Errorf("severity = %s, want low (inline reachability downrank of a dead unexported function)", got.Severity)
	}
	// swept_at stamped INLINE — no post-scan SweepDrain was called in this test.
	if got.SweptAt.IsZero() {
		t.Error("SweptAt is zero: severity was not validated inline at persist (bugbot-596 regression)")
	}

	// The persisted row must match the in-memory result.
	stored, err := st.GetFinding(ctx, got.ID)
	if err != nil {
		t.Fatalf("GetFinding: %v", err)
	}
	if stored.Severity != domain.SeverityLow || stored.SweptAt.IsZero() {
		t.Errorf("persisted finding severity=%s swept=%v, want low + swept", stored.Severity, !stored.SweptAt.IsZero())
	}

	// Inline-swept survivor must be excluded from the bulk drain's WorkRemaining.
	unswept, err := st.UnsweptOpenFindings(ctx)
	if err != nil {
		t.Fatalf("UnsweptOpenFindings: %v", err)
	}
	for _, u := range unswept {
		if u.ID == got.ID {
			t.Error("inline-swept survivor still appears in UnsweptOpenFindings (would be re-swept in bulk)")
		}
	}
}
