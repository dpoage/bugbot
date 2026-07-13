package tui

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/store"
)

func writeTranscriptFile(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile %q: %v", name, err)
	}
}

// TestDiscoverTranscript_ExactMatch verifies a unit whose transcript filename
// embeds its own ID (the agent.WithTranscriptKey join key finder/verifier
// runs now use) is found by an EXACT substring match, even when a decoy file
// also falls inside the timestamp-window heuristic's range — the exact match
// must win, not the heuristic.
func TestDiscoverTranscript_ExactMatch(t *testing.T) {
	dir := t.TempDir()
	started := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	finished := started.Add(2 * time.Minute)

	// Decoy: an old-style file (no key) that also falls in the time window —
	// if the heuristic ran first it would wrongly pick this one.
	writeTranscriptFile(t, dir, "20260713T100030.000Z-decoy-task.jsonl")
	// The real match: same-ish timestamp, but carries the unit's ID as a key.
	exactName := "20260713T100100.000Z-unit-abc123-my-task.jsonl"
	writeTranscriptFile(t, dir, exactName)

	u := store.AgentUnit{ID: "unit-abc123", StartedAt: started, FinishedAt: finished}
	got := discoverTranscript([]string{dir}, u)
	want := filepath.Join(dir, exactName)
	if got != want {
		t.Errorf("discoverTranscript = %q, want exact match %q", got, want)
	}
}

// TestDiscoverTranscript_ExactMatch_MultipleFiles verifies that when several
// transcript files share the same key — a verifier row's refuter-panel seats
// plus its arbiter all key off one unit ID — discoverTranscript picks the
// lexicographically (== chronologically, given the timestamp prefix) first
// one deterministically, rather than an arbitrary directory-order pick.
func TestDiscoverTranscript_ExactMatch_MultipleFiles(t *testing.T) {
	dir := t.TempDir()
	started := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)

	first := "20260713T100000.000Z-unit-panel-seat-a.jsonl"
	second := "20260713T100005.000Z-unit-panel-seat-b.jsonl"
	writeTranscriptFile(t, dir, second)
	writeTranscriptFile(t, dir, first)

	u := store.AgentUnit{ID: "unit-panel", StartedAt: started, FinishedAt: started.Add(time.Minute)}
	got := discoverTranscript([]string{dir}, u)
	want := filepath.Join(dir, first)
	if got != want {
		t.Errorf("discoverTranscript = %q, want the chronologically-first match %q", got, want)
	}
}

// TestDiscoverTranscript_FallsBackToHeuristic verifies a unit with no exact
// ID match in the directory — the reproducer/patch-prover case, whose
// autosave path has no access to a pre-minted join key — falls back to the
// pre-existing timestamp-window heuristic instead of returning "".
func TestDiscoverTranscript_FallsBackToHeuristic(t *testing.T) {
	dir := t.TempDir()
	started := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	finished := started.Add(2 * time.Minute)

	name := "20260713T100030.000Z-repro-task.jsonl"
	writeTranscriptFile(t, dir, name)

	u := store.AgentUnit{ID: "unit-no-file-carries-this-id", StartedAt: started, FinishedAt: finished}
	got := discoverTranscript([]string{dir}, u)
	want := filepath.Join(dir, name)
	if got != want {
		t.Errorf("discoverTranscript = %q, want heuristic fallback match %q", got, want)
	}
}

// TestDiscoverTranscript_NoMatchAnywhere verifies a unit with neither an
// exact-key file nor any file in its timestamp window resolves to "" — the
// normal, expected outcome for most units when transcript capture failed or
// raced a directory listing, not an error.
func TestDiscoverTranscript_NoMatchAnywhere(t *testing.T) {
	dir := t.TempDir()
	started := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)

	writeTranscriptFile(t, dir, "20260101T000000.000Z-unrelated-task.jsonl")

	u := store.AgentUnit{ID: "unit-abc", StartedAt: started, FinishedAt: started.Add(time.Minute)}
	if got := discoverTranscript([]string{dir}, u); got != "" {
		t.Errorf("discoverTranscript = %q, want \"\" (no match)", got)
	}
}

// TestDiscoverTranscript_EmptyDirOrStartedAt verifies the pre-existing
// early-out guards (no directory configured, or a skipped unit with a zero
// StartedAt) still short-circuit to "" without touching the filesystem.
func TestDiscoverTranscript_EmptyDirOrStartedAt(t *testing.T) {
	dir := t.TempDir()
	writeTranscriptFile(t, dir, "20260713T100000.000Z-unit-abc-task.jsonl")

	if got := discoverTranscript(nil, store.AgentUnit{ID: "unit-abc", StartedAt: time.Now()}); got != "" {
		t.Errorf("discoverTranscript with no dirs = %q, want \"\"", got)
	}
	if got := discoverTranscript([]string{dir}, store.AgentUnit{ID: "unit-abc"}); got != "" {
		t.Errorf("discoverTranscript with zero StartedAt = %q, want \"\"", got)
	}
}

// TestDiscoverTranscript_MultiDir_ExactBeatsCrossDirHeuristic is the
// bugbot-dbs8 regression: with both a general transcript dir and a
// repro-specific override configured, an exact key match in EITHER directory
// must beat an in-window keyless candidate in the other — and a keyless
// repro file must still be found by the heuristic when no exact match exists
// anywhere. Before the fix the TUI feeds searched only
// cfg.Repro.TranscriptDir (empty by default), so keyed finder transcripts in
// the general dir were never discovered at all.
func TestDiscoverTranscript_MultiDir_ExactBeatsCrossDirHeuristic(t *testing.T) {
	general := t.TempDir()
	reproDir := t.TempDir()
	started := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	finished := started.Add(2 * time.Minute)

	// Keyless repro-style file in the repro dir, squarely inside the window
	// and CLOSER to StartedAt than the keyed file — a heuristic-first (or
	// per-dir sequential) lookup would wrongly pick it for the finder unit.
	reproName := "20260713T100005.000Z-repro-task.jsonl"
	writeTranscriptFile(t, reproDir, reproName)
	// Keyed finder transcript in the general dir.
	exactName := "20260713T100100.000Z-unit-fdr9-my-task.jsonl"
	writeTranscriptFile(t, general, exactName)

	dirs := []string{general, reproDir}

	finder := store.AgentUnit{ID: "unit-fdr9", StartedAt: started, FinishedAt: finished}
	if got, want := discoverTranscript(dirs, finder), filepath.Join(general, exactName); got != want {
		t.Errorf("finder: discoverTranscript = %q, want cross-dir exact match %q", got, want)
	}

	repro := store.AgentUnit{ID: "unit-no-keyed-file", StartedAt: started, FinishedAt: finished}
	if got, want := discoverTranscript(dirs, repro), filepath.Join(reproDir, reproName); got != want {
		t.Errorf("repro: discoverTranscript = %q, want heuristic match across dirs %q", got, want)
	}
}

// TestTranscriptDirs verifies the reader-side directory precedence the TUI
// feeds hand to discoverTranscript (bugbot-dbs8): general dir first, the
// repro override appended only when set and distinct, empties dropped, and
// nil for a fully-disabled config.
func TestTranscriptDirs(t *testing.T) {
	cases := []struct {
		name    string
		general string
		repro   string
		want    []string
	}{
		{"general only (default config shape)", ".bugbot/transcripts", "", []string{".bugbot/transcripts"}},
		{"general plus repro override", ".bugbot/transcripts", "artifacts/repro", []string{".bugbot/transcripts", "artifacts/repro"}},
		{"duplicate collapsed", "same", "same", []string{"same"}},
		{"repro only (autosave disabled generally)", "", "artifacts/repro", []string{"artifacts/repro"}},
		{"both empty disables discovery", "", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{TranscriptDir: tc.general}
			cfg.Repro.TranscriptDir = tc.repro
			if got := transcriptDirs(cfg); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("transcriptDirs = %#v, want %#v", got, tc.want)
			}
		})
	}
}
