package tui

import (
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/store"
)

func TestTranscriptRunIDRoundTrip(t *testing.T) {
	before := time.Now().Add(-time.Second)
	id := funnel.TranscriptRunID("0f0e0d0c0b0a09080706050403020100")
	minted, key, ok := funnel.ParseTranscriptName(string(id) + ".jsonl")
	if !ok || key != "0f0e0d0c0b0a09080706050403020100" || minted.Before(before) || minted.After(time.Now()) {
		t.Errorf("ParseTranscriptName(%q) = (%v, %q, %v)", id, minted, key, ok)
	}
	if _, key, ok := funnel.ParseTranscriptName(string(funnel.TranscriptRunID("")) + ".jsonl"); !ok || key != "" {
		t.Errorf("unkeyed RunID parsed as key=%q ok=%v", key, ok)
	}
	if _, _, ok := funnel.ParseTranscriptName("20260713T100030.000Z-unit-abc-task.jsonl"); ok {
		t.Errorf("a pre-v0.6 timestamp name must not parse as a RunID")
	}
}

// TestDiscoverTranscript_KeyIsExact verifies the key match is whole-key, not a
// substring: a key that merely contains the unit's ID belongs to another unit.
func TestDiscoverTranscript_KeyIsExact(t *testing.T) {
	dir := t.TempDir()
	started := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	writeTranscriptFile(t, dir, runFile(started, "abc123ff"))

	u := store.AgentUnit{ID: "abc123", StartedAt: started, FinishedAt: started.Add(time.Minute)}
	if got := discoverTranscript([]string{dir}, u); got != "" {
		t.Errorf("discoverTranscript = %q, want \"\" (another unit's keyed file)", got)
	}
}
