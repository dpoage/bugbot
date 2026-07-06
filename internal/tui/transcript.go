package tui

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dpoage/bugbot/internal/store"
)

// transcriptTimestampLayout matches agent.Runner's autosave naming:
// "<RFC3339-ish timestamp>-<task-slug>.jsonl" (see internal/agent/runner.go
// autosave). It is duplicated here rather than exported from internal/agent
// because it is a filename convention, not part of the transcript format.
const transcriptTimestampLayout = "20060102T150405.000Z"

// discoverTranscript best-effort locates a JSONL transcript for agent unit u
// inside dir. Only reproducer/patch-prover units are ever autosaved (see
// config.Repro.TranscriptDir), so most units correctly resolve to "" (no
// transcript) — that is a normal outcome, not a failure.
//
// The autosave filename encodes only a timestamp and a slug of the task text,
// neither of which is the agent_unit's id or lens verbatim, so there is no
// exact join key. The heuristic instead picks the transcript file whose
// embedded timestamp falls within [u.StartedAt, u.FinishedAt] (with a little
// slack for a still-running or skipped unit), choosing the closest match to
// StartedAt when several qualify.
func discoverTranscript(dir string, u store.AgentUnit) string {
	if dir == "" || u.StartedAt.IsZero() {
		return ""
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}

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
// transcript filename ("<timestamp>-<slug>.jsonl").
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
