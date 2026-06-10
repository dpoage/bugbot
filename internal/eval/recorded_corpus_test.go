package eval

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dpoage/bugbot/internal/funnel"
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
		// Chunk size is load-bearing: it re-anchors how many finder agents a fixed
		// file set produces, so the recorded FinderSessions only mean what they say
		// at the chunk size that produced them. When the manifest records one (newer
		// corpora), it must match the chunk size the current replay actually uses;
		// otherwise the corpus was recorded under a different finder fan-out and a
		// matching score would be coincidental. Manifests recorded before this field
		// existed (chunk_size == 0, omitted) are skipped so they replay unchanged.
		if m.ChunkSize != 0 {
			// Recorded cases replay with the builtin's default options (chunk size
			// unset => funnel.DefaultChunkSize), so that is the fan-out this replay
			// actually uses.
			replayChunkSize := effectiveChunkSize(0)
			if m.ChunkSize != replayChunkSize {
				t.Errorf("case %q: manifest chunk_size = %d, replay uses %d — corpus was recorded at a different finder fan-out; re-record",
					cr.Name, m.ChunkSize, replayChunkSize)
			}
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

// TestEffectiveChunkSize pins the chunk-size resolution that the manifest
// records and the replay asserts: a non-positive option resolves to the funnel
// default, a positive option is used verbatim.
func TestEffectiveChunkSize(t *testing.T) {
	if got := effectiveChunkSize(0); got != funnel.DefaultChunkSize {
		t.Errorf("effectiveChunkSize(0) = %d, want default %d", got, funnel.DefaultChunkSize)
	}
	if got := effectiveChunkSize(-1); got != funnel.DefaultChunkSize {
		t.Errorf("effectiveChunkSize(-1) = %d, want default %d", got, funnel.DefaultChunkSize)
	}
	if got := effectiveChunkSize(30); got != 30 {
		t.Errorf("effectiveChunkSize(30) = %d, want 30", got)
	}
}

// TestManifest_ChunkSizeRoundTrip proves the new chunk_size field survives the
// write/load round-trip, and that a manifest recorded before the field existed
// (no chunk_size) loads as zero so the replay assertion treats it as "unknown,
// skip" rather than failing.
func TestManifest_ChunkSizeRoundTrip(t *testing.T) {
	dir := t.TempDir()

	// New manifest carrying a chunk size.
	if err := writeManifest(dir, RecordedManifest{Case: "x", ChunkSize: 8}); err != nil {
		t.Fatal(err)
	}
	got, err := LoadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.ChunkSize != 8 {
		t.Errorf("round-tripped ChunkSize = %d, want 8", got.ChunkSize)
	}

	// Legacy manifest without the field: chunk_size absent => zero on load, which
	// the replay assertion skips (omitempty also keeps it out of the JSON).
	legacy := []byte(`{"case":"x","finder_sessions":1}`)
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), legacy, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = LoadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.ChunkSize != 0 {
		t.Errorf("legacy manifest ChunkSize = %d, want 0 (field absent)", got.ChunkSize)
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
