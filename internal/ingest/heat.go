package ingest

import (
	"context"
	"math"
	"strconv"
	"strings"
	"time"
)

// Heat scoring parameters.
//
// The decay constant τ (tau) controls how quickly older commits lose weight.
// With τ = 30 days:
//   - A commit from today contributes exp(0) = 1.0.
//   - A commit from 30 days ago contributes exp(-1) ≈ 0.368.
//   - A commit from 90 days ago contributes exp(-3) ≈ 0.050.
//   - A commit from 180 days ago contributes exp(-6) ≈ 0.0025.
//
// This exponential decay means recent activity dominates the score, which
// matches the empirical finding that bugs cluster in recently-churned code
// (more churn = more recent change = higher probability of regression). The
// 30-day half-life is a reasonable default: short enough to weight last
// month's changes meaningfully more than last quarter's, long enough that a
// single busy day does not completely drown out older context.
const (
	// heatDecayTauDays is τ in days. The decay weight for a commit is
	// exp(-age_days / heatDecayTauDays).
	heatDecayTauDays = 30.0
	// heatDefaultWindowDays is the default look-back in days when the caller
	// passes a zero window to ChurnHeat.
	heatDefaultWindowDays = 180
	// heatCommitCap is the maximum number of commits ChurnHeat reads from git.
	// It bounds memory and subprocess runtime while still covering months of
	// typical repository activity.
	heatCommitCap = 500
	// heatGitTimeout is the deadline for the git log subprocess inside ChurnHeat.
	// An unbounded call on a pathological repo would block every Sweep; 10 s is
	// generous for any sane history while still bounding the worst case.
	heatGitTimeout = 10 * time.Second
)

// ChurnHeat computes a per-file heat score for every file touched in recent
// git history. The score for each file is the sum over touching commits of
// exp(-age_days/τ), where τ = 30 days. Files touched by many recent commits
// score highest, which statistically correlates with bug density.
//
// window is the look-back duration; zero uses 180 days. Internally this issues
// one bounded git call: `git log --since=<window> -n 500 --name-only
// --format=%ct`.
//
// A shallow git history, an empty repository, or git being unavailable all
// return an empty map with no error: callers degrade gracefully to
// alphabetical ordering.
func ChurnHeat(ctx context.Context, repoDir string, window time.Duration) (map[string]float64, error) {
	windowDays := heatDefaultWindowDays
	if window > 0 {
		windowDays = int(window.Hours() / 24)
		if windowDays < 1 {
			windowDays = 1
		}
	}

	// git --since accepts "<N> seconds ago"; use seconds for precision.
	sinceSeconds := int64(windowDays) * 24 * 3600
	sinceArg := "--since=" + strconv.FormatInt(sinceSeconds, 10) + " seconds ago"
	nArg := "-n" + strconv.Itoa(heatCommitCap)

	// Wrap with a hard deadline so a pathological repo cannot block every Sweep.
	// On timeout the context error causes runGitRaw to return non-nil, which
	// falls through to the degradation path (nil,nil) below.
	heatCtx, cancel := context.WithTimeout(ctx, heatGitTimeout)
	defer cancel()

	// -c core.quotepath=off prevents git from quoting non-ASCII path components
	// (accented, CJK, etc.) as octal-escaped C-strings. Without this flag,
	// `git log --name-only` emits `"caf\303\251.txt"` while the snapshot side
	// (git ls-files -z) yields raw UTF-8 — the paths never match and heat is
	// silently zero for those files.
	out, err := runGitRaw(heatCtx, repoDir,
		"-c", "core.quotepath=off",
		"log",
		nArg,
		sinceArg,
		"--name-only",
		"--format=%ct",
	)
	if err != nil {
		// Any git failure (not a repo, shallow clone, no history, timeout, etc.)
		// → empty heat, no error. Callers degrade to alphabetical.
		return nil, nil //nolint:nilerr
	}

	return parseHeat(out, time.Now()), nil
}

// parseHeat parses the output of `git log --name-only --format=%ct` and
// returns a map from repo-relative file path to heat score. now is the
// reference time for computing commit ages (injected by tests for
// determinism; production passes time.Now()).
//
// Output format: blank-line-separated stanzas. Each stanza begins with a
// Unix epoch timestamp (%ct) on its own line, followed by zero or more
// repo-relative file paths (one per line). Blank lines between stanzas are
// skipped.
//
// Non-code paths (DetectLanguage == LangOther — configs, manifests, data,
// docs, vendored files with unknown extensions) are dropped from the map.
// They are usually edited in lockstep with code changes but do not
// themselves carry bug surface, so allowing them to dominate the heat
// ranking wastes the top finder seats on inputs the lens cannot act on.
// Callers already treat an absent key as score 0 / ranked last, so the
// skip is transparent. Shell scripts (LangShell) and all real code
// languages pass through unchanged; languages newly added to the
// extension map (e.g. Elixir) compose automatically.
func parseHeat(data []byte, now time.Time) map[string]float64 {
	heat := make(map[string]float64)
	nowUnix := float64(now.Unix())

	var currentTS float64 = -1
	// strings.Split (not bufio.Scanner) so lines longer than the 64KB
	// default scanner token cap are not silently dropped. Long file paths
	// (deeply-nested generated code, vendored trees) can exceed that
	// limit and must still count toward the heat score.
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		// A pure-integer line is a commit timestamp (%ct).
		if ts, err := strconv.ParseInt(line, 10, 64); err == nil {
			currentTS = float64(ts)
			continue
		}
		// Any non-empty non-integer line following a timestamp is a file path.
		if currentTS < 0 {
			// No timestamp seen yet; skip orphaned lines (shouldn't happen in
			// practice, but be defensive).
			continue
		}
		// Drop non-code paths (configs, manifests, data, docs, unknown
		// extensions) so they cannot dominate the heat ranking.
		if DetectLanguage(line) == LangOther {
			continue
		}
		// age in days: (now - commit_time) / 86400.
		ageDays := (nowUnix - currentTS) / 86400.0
		if ageDays < 0 {
			ageDays = 0
		}
		weight := math.Exp(-ageDays / heatDecayTauDays)
		heat[line] += weight
	}
	return heat
}
