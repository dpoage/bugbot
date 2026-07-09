package funnel

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/ingest"
)

// TestCandidatesSchema_DefectKindEnumMatchesDomain pins the sync invariant
// domain.AllDefectKinds' doc claims: the candidatesSchema JSON enum for
// defect_kind is a hand-maintained COPY of the Go-side taxonomy and must be
// identical, element-for-element, in the same order. If a kind is ever added,
// removed, or reordered on one side without the other, a finder could emit a
// value the schema accepts but domain.DefectKind.Valid rejects (or vice
// versa) — this test is the guard against that drift.
func TestCandidatesSchema_DefectKindEnumMatchesDomain(t *testing.T) {
	var schema struct {
		Properties struct {
			Candidates struct {
				Items struct {
					Properties struct {
						DefectKind struct {
							Enum []string `json:"enum"`
						} `json:"defect_kind"`
					} `json:"properties"`
				} `json:"items"`
			} `json:"candidates"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(candidatesSchema, &schema); err != nil {
		t.Fatalf("unmarshal candidatesSchema: %v", err)
	}
	got := schema.Properties.Candidates.Items.Properties.DefectKind.Enum
	if len(got) == 0 {
		t.Fatal("candidatesSchema's defect_kind.enum is empty or missing — schema shape changed?")
	}
	want := make([]string, len(domain.AllDefectKinds))
	for i, k := range domain.AllDefectKinds {
		want[i] = string(k)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("candidatesSchema defect_kind enum = %v\nwant (domain.AllDefectKinds) = %v\n(the two lists have drifted — update whichever is stale)", got, want)
	}
}

// TestRunFinder_RejectsBadDefectKind proves RunJSON's deep schema validator
// rejects an out-of-enum defect_kind against the REAL candidatesSchema (not
// a hand-rolled mirror): a finder response with defect_kind:"use-after-free"
// (not in domain.AllDefectKinds) fails validation on the first completion,
// the repair round-trip is offered the same invalid shape and also fails, and
// the finder unit surfaces as finderParseFailed with zero candidates — never
// a silently-accepted bad kind reaching triage's identity computation.
func TestRunFinder_RejectsBadDefectKind(t *testing.T) {
	badCand := `{"file": "bug.go", "line": 10, "title": "nil deref of cfg in Greeting",
		"description": "cfg may be nil", "severity": "high",
		"evidence": "Greeting returns cfg.Name without a nil check", "confidence": "high",
		"defect_kind": "use-after-free", "subject": "Greeting"}`

	finder := newScriptedClient()
	finder.fallback = candJSON(badCand) // same invalid body on the repair attempt too

	f, tools := newFunnelForFinder(t, finder, newScriptedClient())

	budget := &budgetState{}
	cands, status, _, err := f.runFinder(
		context.Background(), finder, tools, "senior Go engineer", f.lenses[0],
		[]ingest.Language{ingest.LangGo}, finderTask([]string{"bug.go"}, nil, ""), budget,
	)
	if err != nil {
		t.Fatalf("runFinder: %v", err)
	}
	if status != finderParseFailed {
		t.Fatalf("runFinder status = %d, want finderParseFailed (%d): an out-of-enum defect_kind must fail schema validation, not silently pass", status, finderParseFailed)
	}
	if len(cands) != 0 {
		t.Fatalf("finder cands = %+v, want none: a defect_kind outside the enum must never reach triage", cands)
	}

	// The client must have been called twice: the initial attempt plus the
	// one repair round-trip RunJSON offers before giving up.
	if n := finder.callCount(); n < 2 {
		t.Errorf("finder.callCount() = %d, want >= 2 (initial + repair round-trip)", n)
	}
}
