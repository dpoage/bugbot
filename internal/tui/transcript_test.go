package tui

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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
	got := discoverTranscript(dir, u)
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
	got := discoverTranscript(dir, u)
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
	got := discoverTranscript(dir, u)
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
	if got := discoverTranscript(dir, u); got != "" {
		t.Errorf("discoverTranscript = %q, want \"\" (no match)", got)
	}
}

// TestDiscoverTranscript_EmptyDirOrStartedAt verifies the pre-existing
// early-out guards (no directory configured, or a skipped unit with a zero
// StartedAt) still short-circuit to "" without touching the filesystem.
func TestDiscoverTranscript_EmptyDirOrStartedAt(t *testing.T) {
	dir := t.TempDir()
	writeTranscriptFile(t, dir, "20260713T100000.000Z-unit-abc-task.jsonl")

	if got := discoverTranscript("", store.AgentUnit{ID: "unit-abc", StartedAt: time.Now()}); got != "" {
		t.Errorf("discoverTranscript with empty dir = %q, want \"\"", got)
	}
	if got := discoverTranscript(dir, store.AgentUnit{ID: "unit-abc"}); got != "" {
		t.Errorf("discoverTranscript with zero StartedAt = %q, want \"\"", got)
	}
}
