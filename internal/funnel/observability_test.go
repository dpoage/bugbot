package funnel

import (
	"context"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/store"
)

// TestAgentUnits_FinderIntegration verifies per-unit observability for a sweep
// with two lenses: nil-safety returns valid candidate JSON; every other lens
// returns unparseable prose. Expected: one row per unit with correct statuses,
// nonzero token fields on launched units, candidates count correct on the
// candidate-bearing unit, and strategy column populated.
//
// This test also serves as the VACUITY CHECK anchor: see the note at the
// bottom of this function about commenting out AddAgentUnit to verify the test
// fails.
func TestAgentUnits_FinderIntegration(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	// nil-safety returns one valid candidate; all others return unparseable prose
	// so they produce parse_failed units.
	finder := newScriptedClient()
	finder.onSystemContains("nil-safety/error-handling", candJSON(realCand))
	finder.fallback = "prose that never parses as json"
	verifier := newScriptedClient()
	verifier.fallback = notRefutedJSON

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{
		MaxParallel: 1, // serialize so ordering is deterministic for assertions
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}

	allUnits, err := st.ListAgentUnits(ctx, res.ScanRunID)
	if err != nil {
		t.Fatalf("ListAgentUnits: %v", err)
	}

	// Filter to finder units only (verifier rows are also recorded).
	var units []store.AgentUnit
	for _, u := range allUnits {
		if u.Role == "finder" {
			units = append(units, u)
		}
	}

	// Compute expected finder unit count: on a sweep,
	//   nTaxonomy wide-strategy units + 1 api-contract-misuse@contract-trace-deep
	nTaxonomy := len(BuiltinLenses()) - 1 // all builtins except diff-intent
	wantUnits := nTaxonomy + 1            // +1 for contract-trace-deep
	if len(units) != wantUnits {
		t.Logf("all units:")
		for _, u := range allUnits {
			t.Logf("  order=%d role=%s lens=%s strategy=%s status=%s", u.LaunchOrder, u.Role, u.Lens, u.Strategy, u.Status)
		}
		t.Fatalf("got %d finder agent_unit rows, want %d (nTaxonomy=%d wide + 1 deep + no diff-intent on sweep)",
			len(units), wantUnits, nTaxonomy)
	}

	// Find the nil-safety unit (the one that produced candidates).
	var nilSafetyUnit *store.AgentUnit
	var parseFailedCount int
	for i := range units {
		u := &units[i]
		if u.Lens == "nil-safety/error-handling" && u.Strategy == "sweep-wide" {
			nilSafetyUnit = u
		}
		if u.Status == "parse_failed" {
			parseFailedCount++
		}
		// Every unit must have a strategy column set.
		if u.Strategy == "" {
			t.Errorf("unit %d (lens=%s): Strategy is empty", u.LaunchOrder, u.Lens)
		}
		// Launched units (ok, parse_failed, budget_stopped) must have nonzero tokens.
		switch u.Status {
		case "ok", "parse_failed", "budget_stopped":
			if u.InputTokens == 0 && u.OutputTokens == 0 {
				t.Errorf("unit %d (lens=%s status=%s): tokens are both zero on a launched unit",
					u.LaunchOrder, u.Lens, u.Status)
			}
			if u.StartedAt.IsZero() {
				t.Errorf("unit %d (lens=%s status=%s): started_at is zero on launched unit",
					u.LaunchOrder, u.Lens, u.Status)
			}
		}
	}

	if nilSafetyUnit == nil {
		t.Fatalf("no agent_unit row found for nil-safety/error-handling@sweep-wide")
	}
	if nilSafetyUnit.Status != "ok" {
		t.Errorf("nil-safety unit status = %q, want ok", nilSafetyUnit.Status)
	}
	if nilSafetyUnit.Candidates != 1 {
		t.Errorf("nil-safety unit candidates = %d, want 1", nilSafetyUnit.Candidates)
	}

	// The api-contract-misuse@contract-trace-deep unit must be present with the
	// correct strategy.
	var deepUnit *store.AgentUnit
	for i := range units {
		u := &units[i]
		if u.Lens == "api-contract-misuse" && u.Strategy == "contract-trace-deep" {
			deepUnit = &units[i]
			break
		}
	}
	if deepUnit == nil {
		// List all units to help debug.
		t.Logf("all units:")
		for _, u := range units {
			t.Logf("  order=%d lens=%s strategy=%s status=%s", u.LaunchOrder, u.Lens, u.Strategy, u.Status)
		}
		t.Fatal("no agent_unit row found for api-contract-misuse@contract-trace-deep")
	}
	// parse_failed is expected because fallback is unparseable prose.
	if deepUnit.Status != "parse_failed" {
		t.Errorf("deep unit status = %q, want parse_failed (fallback is prose)", deepUnit.Status)
	}

	// All non-nil-safety units should be parse_failed (unparseable prose).
	wantParseFailed := wantUnits - 1 // all except nil-safety@sweep-wide which is "ok"
	if parseFailedCount != wantParseFailed {
		t.Errorf("parse_failed count = %d, want %d", parseFailedCount, wantParseFailed)
	}

	// VACUITY CHECK NOTE (required by design spec):
	// To confirm this test actually checks the recording: temporarily comment out
	// the f.recordFinderUnitWithTime calls in hypothesize.go (or the AddAgentUnit
	// call in recordFinderUnitWithTime in observability.go). The assertions
	// len(units) != wantUnits and nilSafetyUnit == nil must both FAIL. Restore
	// after confirming. The test was verified to fail with AddAgentUnit removed,
	// confirming it is not vacuous.
}

// TestAgentUnits_BudgetSkipRecording verifies that units skipped by the hard
// budget gate produce rows with status=skipped_hard_budget, zero tokens, and
// empty started_at. Uses MaxParallel=1 so budget accrues deterministically.
// The assertion is one-sided: we only assert that AT LEAST ONE skipped_hard_budget
// row exists (goroutines race for the semaphore — asserting a specific unit
// would reintroduce a scheduling flake).
func TestAgentUnits_BudgetSkipRecording(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	// Finder that returns one candidate. Budget sized so the pool exhausts after
	// the first completion (~150 tokens: 100 in + 50 out). With MaxParallel=1
	// the second unit sees an exhausted pool and is hard-stopped.
	finder := newScriptedClient()
	finder.fallback = candJSON(realCand)
	verifier := newScriptedClient()
	verifier.fallback = notRefutedJSON

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{
		TokenBudget:           100, // < 150 — pool exhausts after first completion
		CacheReadBudgetWeight: 1.0,
		MaxParallel:           1,
		Lenses:                []string{"nil-safety/error-handling", "concurrency"}, // use exactly 2 lenses to keep test fast
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}

	units, err := st.ListAgentUnits(ctx, res.ScanRunID)
	if err != nil {
		t.Fatalf("ListAgentUnits: %v", err)
	}

	// Find at least one skipped_hard_budget row.
	var skippedRows []store.AgentUnit
	for _, u := range units {
		if u.Status == "skipped_hard_budget" {
			skippedRows = append(skippedRows, u)
		}
	}
	if len(skippedRows) == 0 {
		t.Fatalf("expected at least one skipped_hard_budget row; got units: %v", units)
	}

	// Validate properties of a skipped row.
	for _, u := range skippedRows {
		if !u.StartedAt.IsZero() {
			t.Errorf("skipped unit %s: started_at should be zero, got %v", u.Lens, u.StartedAt)
		}
		if u.InputTokens != 0 || u.OutputTokens != 0 {
			t.Errorf("skipped unit %s: tokens should be zero, got in=%d out=%d", u.Lens, u.InputTokens, u.OutputTokens)
		}
		if u.Role != "finder" {
			t.Errorf("skipped unit %s: role = %q, want finder", u.Lens, u.Role)
		}
	}
}

// TestAgentUnits_ParseFailedDetailRecorded verifies that when a finder produces
// no parseable output (finderParseFailed), the stored agent_units row has a
// non-empty Detail field containing the postmortem classification. This is the
// integration test for the full pipeline from runFinderWithPrompt → Detail column.
func TestAgentUnits_ParseFailedDetailRecorded(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	// Every finder returns unparseable prose so every unit is parse_failed.
	finder := newScriptedClient()
	finder.fallback = "prose that never parses as JSON"
	verifier := newScriptedClient()

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{
		MaxParallel: 1,
		Lenses:      []string{"nil-safety/error-handling"}, // one lens to keep test fast
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}

	allUnits, err := st.ListAgentUnits(ctx, res.ScanRunID)
	if err != nil {
		t.Fatalf("ListAgentUnits: %v", err)
	}

	var parseFailedUnit *store.AgentUnit
	for i := range allUnits {
		u := &allUnits[i]
		if u.Role == "finder" && u.Status == "parse_failed" {
			parseFailedUnit = u
			break
		}
	}
	if parseFailedUnit == nil {
		t.Fatal("expected at least one parse_failed finder unit row")
	}

	// The Detail field must be populated with a postmortem.
	if parseFailedUnit.Detail == "" {
		t.Error("parse_failed unit Detail is empty; want postmortem string")
	}
	// Must contain the class= prefix from finderPostmortemDetail.
	if !strings.Contains(parseFailedUnit.Detail, "class=") {
		t.Errorf("parse_failed unit Detail missing class= field: %q", parseFailedUnit.Detail)
	}
	// The unparseable case (prose, not a rate-limit) should be class=unparseable.
	if !strings.Contains(parseFailedUnit.Detail, "class=unparseable") {
		t.Errorf("parse_failed unit Detail = %q, want class=unparseable (prose fallback)", parseFailedUnit.Detail)
	}
}

// TestAgentUnits_VerifierRows verifies that verifier unit rows are recorded
// for two scenarios: a candidate that is killed by a unanimous panel (status=killed)
// and one that survives (status=survived). Also exercises the detail string
// for seat name content.
func TestAgentUnits_VerifierRows(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	// Two candidates: the real bug (not refuted) and the bogus one (refuted).
	finder := finderOnNilLens(newScriptedClient())
	verifier := verifierRouting(newScriptedClient())

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{
		MaxParallel: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}

	units, err := st.ListAgentUnits(ctx, res.ScanRunID)
	if err != nil {
		t.Fatalf("ListAgentUnits: %v", err)
	}

	// Find verifier rows.
	var survivedRow, killedRow *store.AgentUnit
	for i := range units {
		u := &units[i]
		if u.Role != "verifier" {
			continue
		}
		switch u.Status {
		case "survived":
			survivedRow = u
		case "killed":
			killedRow = u
		}
	}

	if survivedRow == nil {
		t.Error("expected a 'survived' verifier row; found none")
	}
	if killedRow == nil {
		t.Error("expected a 'killed' verifier row; found none")
	}

	if survivedRow != nil {
		// Survived row: candidates=1 (the unit survived), lens = nil-safety.
		if survivedRow.Candidates != 1 {
			t.Errorf("survived row candidates = %d, want 1", survivedRow.Candidates)
		}
		if survivedRow.Lens != "nil-safety/error-handling" {
			t.Errorf("survived row lens = %q, want nil-safety/error-handling", survivedRow.Lens)
		}
		// Detail must reference seats (refuters are named by seat).
		if !strings.Contains(survivedRow.Detail, "seats=") {
			t.Errorf("survived row detail missing 'seats=': %q", survivedRow.Detail)
		}
		// Tokens should be nonzero (refuters ran).
		if survivedRow.InputTokens == 0 {
			t.Errorf("survived row: InputTokens=0, expected nonzero (refuters ran)")
		}
	}

	if killedRow != nil {
		// Killed row: candidates=0 (the unit was killed), lens = nil-safety.
		if killedRow.Candidates != 0 {
			t.Errorf("killed row candidates = %d, want 0", killedRow.Candidates)
		}
		if !strings.Contains(killedRow.Detail, "seats=") {
			t.Errorf("killed row detail missing 'seats=': %q", killedRow.Detail)
		}
		if killedRow.InputTokens == 0 {
			t.Errorf("killed row: InputTokens=0, expected nonzero (refuters ran)")
		}
	}
}
