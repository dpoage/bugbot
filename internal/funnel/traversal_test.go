package funnel

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/ingest"
)

// traversalCandJSON renders a finder response that includes an optional
// traversal field alongside the candidates array.
func traversalCandJSON(traversalEnumerated, traversalVisited []string, cands ...string) string {
	enumBytes, _ := json.Marshal(traversalEnumerated)
	visitedBytes, _ := json.Marshal(traversalVisited)
	return `{"candidates": [` + strings.Join(cands, ",") + `],` +
		`"traversal":{"enumerated":` + string(enumBytes) + `,"visited":` + string(visitedBytes) + `}}`
}

// TestSweep_FinderWithTraversal_PersiststFinderTraversalRow (bugbot-my7a.1)
// verifies the funnel-level wiring of AddFinderTraversal: when a finder unit
// reports a traversal field in its output JSON, the funnel must persist exactly
// ONE finder_traversals row with the unit's lens, strategy, files, and
// candidate count. A finder that omits the traversal field must persist zero
// rows.
//
// Setup mirrors TestSweep_KilledCandidate_PersistsDeadHypothesis:
// finderWithTraversal emits realCand with a traversal summary on the
// nil-safety lens; finderNoTraversal emits emptyCandidates with no traversal.
// After Sweep completes:
//   - Exactly ONE finder_traversals row exists, from the traversal-reporting
//     unit. Its Lens, Files, Enumerated, Visited, and CandidateCount must match
//     the unit's actual output.
//   - The unit that omitted the traversal field produces zero rows.
func TestSweep_FinderWithTraversal_PersistsFinderTraversalRow(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	// Finder that includes a traversal field on the nil-safety lens. It emits
	// one real candidate plus the traversal summary. The bogus lens returns
	// emptyCandidates (no traversal) so only one finder_traversals row is
	// written across the whole sweep.
	enumerated := []string{"(*Greeter).Greet", "Greeting"}
	visited := []string{"Greeting"}
	finderJSON := traversalCandJSON(enumerated, visited, realCand)

	finder := newScriptedClient()
	finder.onSystemContains("nil-safety/error-handling", finderJSON)
	finder.fallback = emptyCandidates // all other lenses: no traversal

	verifier := newScriptedClient()
	verifier.fallback = notRefutedJSON

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.ScanRunID == "" {
		t.Fatal("res.ScanRunID is empty; ListFinderTraversals needs it")
	}

	rows, err := st.ListFinderTraversals(ctx, res.ScanRunID)
	if err != nil {
		t.Fatalf("ListFinderTraversals: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListFinderTraversals returned %d rows, want 1 (only the traversal-reporting unit; no-traversal units must NOT produce a row)", len(rows))
	}
	got := rows[0]

	// Lens must match the unit's lens.
	if got.Lens != "nil-safety/error-handling" {
		t.Errorf("Lens = %q, want nil-safety/error-handling", got.Lens)
	}
	// ScanRunID must match.
	if got.ScanRunID != res.ScanRunID {
		t.Errorf("ScanRunID = %q, want %q", got.ScanRunID, res.ScanRunID)
	}
	// CandidateCount is 1 (realCand only; finder emitted exactly one).
	if got.CandidateCount != 1 {
		t.Errorf("CandidateCount = %d, want 1", got.CandidateCount)
	}
	// Enumerated and Visited match the traversal summary.
	if len(got.Enumerated) != len(enumerated) {
		t.Errorf("Enumerated = %v, want %v", got.Enumerated, enumerated)
	} else {
		for i, want := range enumerated {
			if got.Enumerated[i] != want {
				t.Errorf("Enumerated[%d] = %q, want %q", i, got.Enumerated[i], want)
			}
		}
	}
	if len(got.Visited) != len(visited) {
		t.Errorf("Visited = %v, want %v", got.Visited, visited)
	} else {
		for i, want := range visited {
			if got.Visited[i] != want {
				t.Errorf("Visited[%d] = %q, want %q", i, got.Visited[i], want)
			}
		}
	}
	// Files must be non-empty (the fixture repo has at least bug.go).
	if len(got.Files) == 0 {
		t.Error("Files is empty; the unit must record the files it was assigned")
	}
	// ID and CreatedAt must be set.
	if got.ID == "" {
		t.Error("ID is empty")
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}
}

// TestSweep_FinderWithNoTraversal_PersistsZeroFinderTraversalRows verifies
// that a finder unit which OMITS the traversal field produces NO
// finder_traversals rows. This is the default path for finders that have not
// been updated to report traversal; they must not produce noisy empty rows.
func TestSweep_FinderWithNoTraversal_PersistsZeroFinderTraversalRows(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	// All lenses return emptyCandidates with no traversal field.
	finder := newScriptedClient()
	finder.fallback = emptyCandidates

	verifier := newScriptedClient()
	verifier.fallback = notRefutedJSON

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.ScanRunID == "" {
		t.Fatal("res.ScanRunID is empty")
	}

	rows, err := st.ListFinderTraversals(ctx, res.ScanRunID)
	if err != nil {
		t.Fatalf("ListFinderTraversals: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("ListFinderTraversals returned %d rows, want 0 (no traversal field in finder output)", len(rows))
	}
}

// TestCandidatesSchema_ValidatesWithAndWithoutTraversal verifies that
// candidatesSchema accepts both shapes: output WITH a traversal field and
// output WITHOUT it. The traversal field is optional; omitting it must not
// cause a schema validation error (RunJSON would then return finderParseFailed).
// This exercises the real RunJSON → candidatesSchema → schema-validator path,
// not a hand-rolled mirror — see TestRunFinder_RejectsBadDefectKind for the
// same pattern.
func TestCandidatesSchema_ValidatesWithAndWithoutTraversal(t *testing.T) {
	withTraversal := `{"candidates":[],"traversal":{"enumerated":["(*Foo).Bar"],"visited":["(*Foo).Bar"]}}`
	withoutTraversal := `{"candidates":[]}`
	withExtraField := `{"candidates":[],"traversal":{"enumerated":[],"visited":[],"unknown_field":"bad"}}`

	for _, tc := range []struct {
		name       string
		body       string
		wantOK     bool
		wantStatus finderStatus
	}{
		{"with_traversal", withTraversal, true, finderOK},
		{"without_traversal", withoutTraversal, true, finderOK},
		{"with_extra_field_in_traversal", withExtraField, false, finderParseFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			finder := newScriptedClient()
			finder.fallback = tc.body

			f, tools := newFunnelForFinder(t, finder, newScriptedClient())
			budget := &budgetState{}
			_, status, _, err := f.runFinder(
				context.Background(), finder, tools, "senior Go engineer", f.lenses[0],
				[]ingest.Language{ingest.LangGo}, finderTask([]string{"bug.go"}, nil, ""), budget,
			)
			if err != nil {
				t.Fatalf("runFinder: %v", err)
			}
			if status != tc.wantStatus {
				t.Errorf("status = %d, want %d for %s", status, tc.wantStatus, tc.name)
			}
		})
	}
}
