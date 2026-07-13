package tui

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dpoage/bugbot/internal/store"
)

// transcriptTimestampLayout matches agent.Runner's autosave naming:
// "<RFC3339-ish timestamp>-[<key>-]<task-slug>.jsonl" (see
// internal/agent/runner.go autosave). It is duplicated here rather than
// exported from internal/agent because it is a filename convention, not part
// of the transcript format.
const transcriptTimestampLayout = "20060102T150405.000Z"

// discoverTranscript locates a JSONL transcript for agent unit u inside dir.
//
// Every agent run now autosaves (config.TranscriptDir defaults every role
// on; config.Repro.TranscriptDir may still redirect reproducer/patch-prover
// specifically — see internal/engine/repro.go's reproTranscriptDir), and
// every finder/verifier run embeds u.ID as the transcript filename's key
// (agent.WithTranscriptKey, threaded through in internal/funnel/hypothesize.go
// and verify_stream.go): the filename is
// "<timestamp>-<u.ID>-<task-slug>.jsonl". That makes the join EXACT for those
// roles — no timestamp guessing needed — so exactMatch is tried first.
//
// Reproducer/patch-prover transcripts do NOT carry u.ID: their agent_units
// row is built in internal/funnel/repro_hook.go AFTER an opaque
// funnel.Options.Repro hook returns, with no access to the Runner (or its
// Outcome/Transcript) that produced the transcript, so there is nowhere to
// thread a pre-minted key from. Their files keep the plain
// "<timestamp>-<task-slug>.jsonl" shape and are found by the pre-existing
// timestamp-window heuristic, which discoverTranscript falls back to when no
// exact match exists — also covering any transcript written before this
// join key existed.
func discoverTranscript(dir string, u store.AgentUnit) string {
	if dir == "" || u.StartedAt.IsZero() {
		return ""
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	if path, ok := exactMatchTranscript(dir, entries, u.ID); ok {
		return path
	}
	return heuristicMatchTranscript(dir, entries, u)
}

// exactMatchTranscript scans entries for a ".jsonl" file whose name contains
// "-<id>-" — the join key agent.WithTranscriptKey embeds between the
// autosave timestamp and the task slug (see discoverTranscript). id must be
// non-empty (an empty id would match every dashed filename). When several
// files match — a verifier row's refuter-panel-plus-arbiter transcripts all
// share one key — the lexicographically first is returned, which is also the
// chronologically first since every filename starts with a sortable
// timestamp.
func exactMatchTranscript(dir string, entries []os.DirEntry, id string) (string, bool) {
	if id == "" {
		return "", false
	}
	marker := "-" + id + "-"
	best := ""
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".jsonl") || !strings.Contains(name, marker) {
			continue
		}
		if best == "" || name < best {
			best = name
		}
	}
	if best == "" {
		return "", false
	}
	return filepath.Join(dir, best), true
}

// heuristicMatchTranscript is the pre-join-key fallback: it picks the
// transcript file whose embedded timestamp falls within [u.StartedAt,
// u.FinishedAt] (with a little slack for a still-running or skipped unit),
// choosing the closest match to StartedAt when several qualify. This is a
// best-effort guess, not an exact join — see discoverTranscript for when it
// is used.
func heuristicMatchTranscript(dir string, entries []os.DirEntry, u store.AgentUnit) string {
	end := u.FinishedAt
	if end.IsZero() {
		end = time.Now()
	}
	const slack = time.Minute

	best := ""
	bestDelta := time.Duration(-1)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		ts, ok := parseTranscriptTimestamp(name)
		if !ok {
			continue
		}
		if ts.Before(u.StartedAt.Add(-slack)) || ts.After(end.Add(slack)) {
			continue
		}
		delta := ts.Sub(u.StartedAt)
		if delta < 0 {
			delta = -delta
		}
		if bestDelta < 0 || delta < bestDelta {
			best = filepath.Join(dir, name)
			bestDelta = delta
		}
	}
	return best
}

// parseTranscriptTimestamp extracts the leading timestamp from an autosaved
// transcript filename ("<timestamp>-<slug>.jsonl" or
// "<timestamp>-<key>-<slug>.jsonl").
func parseTranscriptTimestamp(name string) (time.Time, bool) {
	idx := strings.Index(name, "-")
	if idx <= 0 {
		return time.Time{}, false
	}
	ts, err := time.Parse(transcriptTimestampLayout, name[:idx])
	if err != nil {
		return time.Time{}, false
	}
	return ts, true
}
