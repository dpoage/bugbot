package agent

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TestWithTranscriptKey_EmbeddedInFilename verifies WithTranscriptKey embeds
// the caller-supplied key between the autosave timestamp and the task slug —
// the exact filename shape internal/tui/transcript.go's discoverTranscript
// relies on to join a transcript file back to its store row by an EXACT
// substring match ("-<key>-") instead of a timestamp-window guess.
func TestWithTranscriptKey_EmbeddedInFilename(t *testing.T) {
	dir := t.TempDir()
	fc := newFakeClient(textResp("done", 1, 1))
	r := NewRunner(fc, nil, "sys", WithTranscriptDir(dir), WithTranscriptKey("unit-abc123"))

	if _, err := r.Run(context.Background(), "My Task!"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1: %v", len(entries), entries)
	}
	name := entries[0].Name()
	if !strings.Contains(name, "-unit-abc123-") {
		t.Errorf("filename %q does not contain the transcript key between dashes (\"-unit-abc123-\")", name)
	}
	if !strings.HasSuffix(name, "-my-task.jsonl") {
		t.Errorf("filename %q does not end with the expected task slug", name)
	}
}

// TestWithTranscriptKey_EmptyKeyIsNoOp verifies an empty key (the zero value,
// and what every pre-existing caller that has no stable ID to mint up front
// passes implicitly by never calling WithTranscriptKey) reproduces the
// original "<timestamp>-<slug>.jsonl" filename shape byte-for-byte — no
// double dash, no empty segment — so existing reproducer/patch-prover
// autosave behavior is unaffected.
func TestWithTranscriptKey_EmptyKeyIsNoOp(t *testing.T) {
	dir := t.TempDir()
	fc := newFakeClient(textResp("done", 1, 1))
	r := NewRunner(fc, nil, "sys", WithTranscriptDir(dir), WithTranscriptKey(""))

	if _, err := r.Run(context.Background(), "My Task!"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1: %v", len(entries), entries)
	}
	name := entries[0].Name()
	if !strings.HasSuffix(name, "-my-task.jsonl") {
		t.Errorf("filename %q does not end with the expected task slug", name)
	}
	// The timestamp prefix is exactly one dash away from the slug: no key
	// segment was inserted.
	if strings.Count(name, "-") != strings.Count("20060102T150405.000Z-my-task.jsonl", "-") {
		t.Errorf("filename %q has an unexpected dash count for a no-key run", name)
	}
}
