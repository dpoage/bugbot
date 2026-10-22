package tui

import (
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/store"
)

// transcriptDirs returns the candidate directories transcripts may live in
// for cfg: the general cfg.TranscriptDir (where every finder/verifier/
// arbiter/cartographer transcript lands, and — absent a repro-specific
// override — reproducer/patch-prover files too), then cfg.Repro.TranscriptDir
// when it redirects those stages elsewhere (the same precedence as
// internal/engine/repro.go's reproTranscriptDir, from the reader's side).
// Empty and duplicate entries are dropped; an all-empty config yields nil,
// which disables discovery entirely.
func transcriptDirs(cfg config.Config) []string {
	var dirs []string
	for _, d := range []string{cfg.TranscriptDir, cfg.Repro.TranscriptDir} {
		if d == "" || slices.Contains(dirs, d) {
			continue
		}
		dirs = append(dirs, d)
	}
	return dirs
}

// discoverTranscript locates a JSONL transcript for agent unit u across the
// candidate directories dirs (see transcriptDirs).
//
// Every agent run writes "<RunID>.jsonl" (agent.JSONL). Finder/verifier runs
// pin their RunID with funnel.TranscriptRunID(u.ID), so their files carry u.ID
// as the RunID's key and the join is EXACT (exactMatchTranscript, tried
// first). Reproducer/patch-prover rows are built after an opaque repro hook
// returns, with no Runner to thread a key into, so their runs keep an
// unkeyed llmkit RunID and are found by the mint-time window heuristic.
// Transcripts written before llmkit v0.6 (timestamp-named files of the old
// event format) are not discovered: their names are not RunIDs and their
// lines would not load.
func discoverTranscript(dirs []string, u store.AgentUnit) string {
	if u.StartedAt.IsZero() {
		return ""
	}
	entries := make([][]os.DirEntry, len(dirs))
	for i, dir := range dirs {
		// Missing/unreadable directories contribute no candidates.
		entries[i], _ = os.ReadDir(dir)
	}
	// An exact key match in ANY directory beats a heuristic guess in any
	// other.
	for i, dir := range dirs {
		if path, ok := exactMatchTranscript(dir, entries[i], u.ID); ok {
			return path
		}
	}
	best := ""
	bestDelta := time.Duration(-1)
	for i, dir := range dirs {
		path, delta, ok := heuristicMatchTranscript(dir, entries[i], u)
		if ok && (bestDelta < 0 || delta < bestDelta) {
			best, bestDelta = path, delta
		}
	}
	return best
}

// exactMatchTranscript returns the transcript in entries whose RunID is keyed
// id (funnel.ParseTranscriptName). id must be non-empty. When several match —
// a verifier row's refuter seats and arbiter share one key — the
// lexicographically first, which is also the earliest-minted, is returned.
func exactMatchTranscript(dir string, entries []os.DirEntry, id string) (string, bool) {
	if id == "" {
		return "", false
	}
	best := ""
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if _, key, ok := funnel.ParseTranscriptName(name); !ok || key != id {
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

// heuristicMatchTranscript is the fallback for unkeyed runs: it picks the
// unkeyed transcript whose RunID mint time falls within [u.StartedAt,
// u.FinishedAt] (with a little slack for a still-running or skipped unit),
// choosing the closest to StartedAt. A keyed transcript belongs to another
// unit and is never a candidate. Returns the matched path, its
// |minted−StartedAt| delta (so the multi-directory caller can pick the
// globally closest candidate), and whether any candidate qualified.
func heuristicMatchTranscript(dir string, entries []os.DirEntry, u store.AgentUnit) (string, time.Duration, bool) {
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
		ts, key, ok := funnel.ParseTranscriptName(name)
		if !ok || key != "" {
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
	if best == "" {
		return "", 0, false
	}
	return best, bestDelta, true
}
