package eval

import (
	"context"
	"testing"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/llm"
)

// TestRecordedMode_EndToEnd proves the recorded-replay MECHANISM against the
// real funnel: it synthesizes per-role transcripts (as if captured from a real
// session), feeds them through RoleTranscriptStores, and confirms the funnel
// produces the seeded finding with zero false positives — the same outcome as
// scripted mode, but driven by replayed recordings.
//
// The funnel runs all built-in lenses serially (MaxParallel defaults to 1 in
// RunCase), so the finder role sees one agent run per lens. We record an
// empty-candidate session for every lens except the nil-safety lens, which
// reports the real bug. Then the single surviving candidate drives one verifier
// agent run per refuter (DefaultRefuters), so the verifier role gets that many
// "not refuted" sessions.
//
// This is the regression scaffold: committing REAL-model transcripts as testdata
// is future work (it requires API keys + TranscriptDir capture). The synthetic
// recordings here exercise the full replay path end-to-end.
func TestRecordedMode_EndToEnd(t *testing.T) {
	requireGit(t)
	ctx := context.Background()

	const realTitle = "nil deref of cfg in Greeting"

	lenses := funnel.BuiltinLenses()

	// One finder session per lens, in builtin (yield) order — the order the
	// funnel launches them under MaxParallel=1.
	var finderSessions []*agent.Transcript
	for _, l := range lenses {
		if l.Name == lensNil {
			finderSessions = append(finderSessions, recordOneShot(
				"audit greet.go",
				Candidates(CandidateJSON{
					File: "greet.go", Line: 10, Title: realTitle,
					Description: "cfg may be nil", Severity: "high",
					Evidence: "no nil check", Confidence: "high",
				}),
			))
		} else {
			finderSessions = append(finderSessions, recordOneShot("audit greet.go", EmptyCandidates))
		}
	}

	// One verifier session per refuter for the single surviving candidate.
	var verifierSessions []*agent.Transcript
	for i := 0; i < funnel.DefaultRefuters; i++ {
		verifierSessions = append(verifierSessions, recordOneShot(
			"refute "+realTitle,
			`{"refuted": false, "reasoning": "reachable and unguarded", "confidence": "high"}`,
		))
	}

	c := NewRecordedCase(
		"recorded-nil-deref",
		FixtureSpec{Files: map[string]string{"greet.go": nilDerefSrc}},
		[]SeededBug{{File: "greet.go", Line: 10, LineTolerance: 2, Kind: "nil-deref"}},
		&RecordedCase{
			Finder:   NewRoleTranscriptStore("finder", llm.Capabilities{}, finderSessions...),
			Verifier: NewRoleTranscriptStore("verifier", llm.Capabilities{}, verifierSessions...),
		},
		funnel.Options{},
		nil,
	)

	res, err := RunSuite(ctx, []Case{c}, ModeRecorded)
	if err != nil {
		t.Fatalf("recorded suite: %v", err)
	}
	t.Log("\n" + res.String())

	if res.TruePositives != 1 || res.FalsePositives != 0 || res.FalseNegatives != 0 {
		t.Fatalf("recorded tp/fp/fn = %d/%d/%d, want 1/0/0",
			res.TruePositives, res.FalsePositives, res.FalseNegatives)
	}
	if res.Precision() != 1.0 {
		t.Errorf("recorded precision = %.3f, want 1.0", res.Precision())
	}
}

// TestRecordedMode_MissingRecordings_Errors confirms a recorded case with no
// recordings fails loudly rather than silently scoring an empty run.
func TestRecordedMode_MissingRecordings_Errors(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	c := Case{Name: "no-recordings", Repo: FixtureSpec{Files: map[string]string{"a.go": "package x\n"}}}
	if _, err := RunSuite(ctx, []Case{c}, ModeRecorded); err == nil {
		t.Errorf("expected error for recorded case with no recordings")
	}
}
