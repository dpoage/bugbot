package eval

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestRecordedCorpusReplay replays the committed real-model corpus through the
// CURRENT funnel pipeline and asserts each case's scores reproduce EXACTLY the
// numbers captured in its manifest at record time.
//
// This is the regression guard for the recorded corpus: same pipeline + same
// transcripts must yield the same scores. It is NOT a precision invariant — a
// real model's precision is a measurement, not something the harness gets to
// require — so it deliberately does NOT apply eval.Gate. A divergence here means
// either the transcripts no longer drive the pipeline the same way (a prompt or
// staging change) or the scoring logic moved; both are real regressions worth a
// red test.
//
// The corpus is optional: when testdata/recorded is absent or empty the test
// skips. Capturing the corpus is done out-of-band via `go test -tags record`
// (see record_test.go) with real API keys.
//
// Note on <think> blocks: MiniMax M3 emits reasoning <think>...</think> spans
// before its JSON answer. RunJSON strips leading/trailing wrapping think blocks
// at parse time (agent.stripThinkBlocks), and agent.ReplayClient matches only
// the TOOL-CALL STRUCTURE of each request — it is intentionally lenient on exact
// message text (see agent/replay.go: "Matching is intentionally lenient on exact
// message text — only the tool-call structure must line up"). So recorded replies
// that carry think blocks replay fine: the recorded assistant text (think block
// and all) is served verbatim and re-parsed by the same stripping path that
// parsed it live.
func TestRecordedCorpusReplay(t *testing.T) {
	requireGit(t)

	dir := DefaultRecordedDir
	cases, err := LoadRecordedCases(dir)
	if err != nil {
		t.Fatalf("load recorded corpus: %v", err)
	}
	if len(cases) == 0 {
		t.Skipf("no recorded corpus at %q; record one with `go test -tags record ./internal/eval/ -run TestRecordCorpus` and the LLM_LIVE_* vars", dir)
	}

	ctx := context.Background()
	res, err := RunSuite(ctx, cases, ModeRecorded)
	if err != nil {
		t.Fatalf("replay recorded suite: %v", err)
	}
	t.Log("\n" + res.String())

	// Determinism: each replayed case must match its recorded manifest exactly.
	for _, cr := range res.Cases {
		m, err := LoadManifest(filepath.Join(dir, cr.Name))
		if err != nil {
			t.Errorf("case %q: %v", cr.Name, err)
			continue
		}
		want := m.Scores
		if cr.TruePositives != want.TruePositives ||
			cr.FalsePositives != want.FalsePositives ||
			cr.FalseNegatives != want.FalseNegatives {
			t.Errorf("case %q: replay tp/fp/fn = %d/%d/%d, manifest recorded %d/%d/%d (replay diverged from recording)",
				cr.Name,
				cr.TruePositives, cr.FalsePositives, cr.FalseNegatives,
				want.TruePositives, want.FalsePositives, want.FalseNegatives)
		}
		if cr.Precision() != want.Precision {
			t.Errorf("case %q: replay precision = %.6f, manifest %.6f", cr.Name, cr.Precision(), want.Precision)
		}
		if cr.Recall() != want.Recall {
			t.Errorf("case %q: replay recall = %.6f, manifest %.6f", cr.Name, cr.Recall(), want.Recall)
		}
	}
}

// TestLoadRecordedCases_MissingDirSkips confirms an absent corpus directory is
// not an error (the corpus is optional).
func TestLoadRecordedCases_MissingDirSkips(t *testing.T) {
	cases, err := LoadRecordedCases(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("missing corpus dir should not error: %v", err)
	}
	if cases != nil {
		t.Errorf("missing corpus dir should yield nil cases, got %d", len(cases))
	}
}

// TestLoadRecordedCases_UnknownSubdirErrors confirms a subdir that is not a
// builtin case name fails loudly rather than being silently ignored.
func TestLoadRecordedCases_UnknownSubdirErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "not-a-builtin-case"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRecordedCases(dir); err == nil {
		t.Errorf("expected error for unknown corpus subdir")
	}
}

// TestLoadRecordedCases_EmptyRoleErrors confirms a case dir missing a role's
// sessions errors (a recorded case needs at least one session per role).
func TestLoadRecordedCases_EmptyRoleErrors(t *testing.T) {
	dir := t.TempDir()
	// A valid builtin case name, but no transcript files at all.
	if err := os.MkdirAll(filepath.Join(dir, "nil-deref-seeded"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRecordedCases(dir); err == nil {
		t.Errorf("expected error for case dir with no sessions")
	}
}

// TestClassifyTranscript covers the role classifier against synthetic finder and
// verifier first-user-message preambles and an unclassifiable transcript.
func TestClassifyTranscript(t *testing.T) {
	finder := recordOneShot(
		"Audit these target files for bugs in your assigned focus area. Read each one.\n\nTARGET FILES:\n  - x.go\n",
		EmptyCandidates,
	)
	if role, err := classifyTranscript(finder); err != nil || role != roleFinder {
		t.Errorf("finder classify = %v, %v; want roleFinder, nil", role, err)
	}

	verifier := recordOneShot(
		"Try to refute this bug report. Read the actual code before deciding.\n\nBUG REPORT\n  file: x.go\n",
		NotRefutedJSON,
	)
	if role, err := classifyTranscript(verifier); err != nil || role != roleVerifier {
		t.Errorf("verifier classify = %v, %v; want roleVerifier, nil", role, err)
	}

	unknown := recordOneShot("hello there, this is neither", "{}")
	if _, err := classifyTranscript(unknown); err == nil {
		t.Errorf("expected classify error for unclassifiable transcript")
	}
}
