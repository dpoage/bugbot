package tui

import (
	"fmt"
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

// runFile names the JSONL file of a run minted at ts, keyed by key ("" for an
// unkeyed llmkit RunID).
func runFile(ts time.Time, key string) string {
	id := fmt.Sprintf("%013d-0123456789abcdef", ts.UnixMilli())
	if key != "" {
		id += "-" + key
	}
	return id + ".jsonl"
}

// TestDiscoverTranscript_ExactMatch verifies a unit whose transcript RunID is
// keyed by its own ID (funnel.TranscriptRunID) is found by an EXACT key match,
// even when an unkeyed decoy also falls inside the heuristic's window.
func TestDiscoverTranscript_ExactMatch(t *testing.T) {
	dir := t.TempDir()
	started := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	finished := started.Add(2 * time.Minute)

	writeTranscriptFile(t, dir, runFile(started.Add(30*time.Second), ""))
	exactName := runFile(started.Add(time.Minute), "abc123")
	writeTranscriptFile(t, dir, exactName)

	u := store.AgentUnit{ID: "abc123", StartedAt: started, FinishedAt: finished}
	got := discoverTranscript([]string{dir}, u)
	want := filepath.Join(dir, exactName)
	if got != want {
		t.Errorf("discoverTranscript = %q, want exact match %q", got, want)
	}
}

// TestDiscoverTranscript_ExactMatch_MultipleFiles verifies that when several
// transcripts share the same key — a verifier row's refuter-panel seats plus
// its arbiter — the earliest-minted one is picked deterministically.
func TestDiscoverTranscript_ExactMatch_MultipleFiles(t *testing.T) {
	dir := t.TempDir()
	started := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)

	first := runFile(started, "panel")
	second := runFile(started.Add(5*time.Second), "panel")
	writeTranscriptFile(t, dir, second)
	writeTranscriptFile(t, dir, first)

	u := store.AgentUnit{ID: "panel", StartedAt: started, FinishedAt: started.Add(time.Minute)}
	got := discoverTranscript([]string{dir}, u)
	want := filepath.Join(dir, first)
	if got != want {
		t.Errorf("discoverTranscript = %q, want the earliest-minted match %q", got, want)
	}
}

// TestDiscoverTranscript_FallsBackToHeuristic verifies a unit with no exact
// ID match — the reproducer/patch-prover case, whose runs carry no key —
// falls back to the mint-time window heuristic over unkeyed runs.
func TestDiscoverTranscript_FallsBackToHeuristic(t *testing.T) {
	dir := t.TempDir()
	started := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	finished := started.Add(2 * time.Minute)

	name := runFile(started.Add(30*time.Second), "")
	writeTranscriptFile(t, dir, name)

	u := store.AgentUnit{ID: "no-file-carries-this-id", StartedAt: started, FinishedAt: finished}
	got := discoverTranscript([]string{dir}, u)
	want := filepath.Join(dir, name)
	if got != want {
		t.Errorf("discoverTranscript = %q, want heuristic fallback match %q", got, want)
	}
}

// TestDiscoverTranscript_NoMatchAnywhere verifies a unit with neither an
// exact-key file nor any unkeyed file in its window resolves to "": keyed
// files of other units and pre-v0.6 timestamp-named files are never picked.
func TestDiscoverTranscript_NoMatchAnywhere(t *testing.T) {
	dir := t.TempDir()
	started := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)

	writeTranscriptFile(t, dir, runFile(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), ""))
	writeTranscriptFile(t, dir, runFile(started.Add(10*time.Second), "other-unit"))
	writeTranscriptFile(t, dir, "20260713T100010.000Z-repro-task.jsonl")

	u := store.AgentUnit{ID: "abc", StartedAt: started, FinishedAt: started.Add(time.Minute)}
	if got := discoverTranscript([]string{dir}, u); got != "" {
		t.Errorf("discoverTranscript = %q, want \"\" (no match)", got)
	}
}

// TestDiscoverTranscript_EmptyDirOrStartedAt verifies the early-out guards (no
// directory configured, or a skipped unit with a zero StartedAt).
func TestDiscoverTranscript_EmptyDirOrStartedAt(t *testing.T) {
	dir := t.TempDir()
	writeTranscriptFile(t, dir, runFile(time.Now(), "abc"))

	if got := discoverTranscript(nil, store.AgentUnit{ID: "abc", StartedAt: time.Now()}); got != "" {
		t.Errorf("discoverTranscript with no dirs = %q, want \"\"", got)
	}
	if got := discoverTranscript([]string{dir}, store.AgentUnit{ID: "abc"}); got != "" {
		t.Errorf("discoverTranscript with zero StartedAt = %q, want \"\"", got)
	}
}

// TestDiscoverTranscript_MultiDir_ExactBeatsCrossDirHeuristic is the
// bugbot-dbs8 regression: with both a general transcript dir and a
// repro-specific override configured, an exact key match in EITHER directory
// must beat an in-window unkeyed candidate in the other — and an unkeyed
// repro file must still be found by the heuristic when no exact match exists.
func TestDiscoverTranscript_MultiDir_ExactBeatsCrossDirHeuristic(t *testing.T) {
	general := t.TempDir()
	reproDir := t.TempDir()
	started := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	finished := started.Add(2 * time.Minute)

	// Unkeyed repro run, CLOSER to StartedAt than the keyed file.
	reproName := runFile(started.Add(5*time.Second), "")
	writeTranscriptFile(t, reproDir, reproName)
	exactName := runFile(started.Add(time.Minute), "fdr9")
	writeTranscriptFile(t, general, exactName)

	dirs := []string{general, reproDir}

	finder := store.AgentUnit{ID: "fdr9", StartedAt: started, FinishedAt: finished}
	if got, want := discoverTranscript(dirs, finder), filepath.Join(general, exactName); got != want {
		t.Errorf("finder: discoverTranscript = %q, want cross-dir exact match %q", got, want)
	}

	repro := store.AgentUnit{ID: "no-keyed-file", StartedAt: started, FinishedAt: finished}
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
