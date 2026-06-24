package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// RunMetric is one point in the valid-findings-per-token series: a finished
// scan run paired with the finding/token counters needed to judge detection
// efficiency. The counters are projected from the run's stats_json blob (the
// funnel writes them via FinishScanRun). CartographerEnabled is the slice key
// for comparing the package-summary pass on vs off.
type RunMetric struct {
	ScanRunID           string
	Kind                ScanKind
	CommitSHA           string
	StartedAt           time.Time
	FinishedAt          time.Time
	CartographerEnabled bool
	Hypothesized        int
	Verified            int
	Killed              int
	// FinderRuns is the number of finder (lens, chunk) agents that actually
	// launched this run — the unit count the scan-estimate calibrates token
	// and wall-time cost against (tokens/unit, tokens/sec).
	FinderRuns   int
	InputTokens  int64
	OutputTokens int64
}

// TotalTokens is the run's input+output spend — the denominator of the
// findings-per-token metric. InputTokens already includes cached tokens (the
// llm.Usage convention), so this is the honest total cost of the run.
func (m RunMetric) TotalTokens() int64 { return m.InputTokens + m.OutputTokens }

// VerifiedPer1K is verified (Tier-2) findings per 1,000 total tokens — the
// efficiency metric that decides whether a change (e.g. the cartographer pass)
// earns its token cost. A zero-token run yields 0 (no divide-by-zero).
func (m RunMetric) VerifiedPer1K() float64 {
	t := m.TotalTokens()
	if t == 0 {
		return 0
	}
	return 1000 * float64(m.Verified) / float64(t)
}

// runStatsProjection is the subset of funnel.Stats that RunMetrics reads out of
// stats_json. The store package cannot import funnel (funnel imports store), so
// this is a deliberate read-side projection: its json tags MUST match the tags
// on funnel.Stats. A tag drift surfaces as a zeroed counter, which the metrics
// command and TestRunMetrics would make obvious.
type runStatsProjection struct {
	Hypothesized        int   `json:"hypothesized"`
	Verified            int   `json:"verified"`
	Killed              int   `json:"killed"`
	FinderRuns          int   `json:"finder_runs"`
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	CartographerEnabled bool  `json:"cartographer_enabled"`
}

// RunMetrics returns the most recent FINISHED scan runs (newest first by
// started_at; at most limit, or all when limit <= 0), each projected into a
// RunMetric. A run whose stats_json is empty or unparseable is still returned
// (with zero counters) rather than dropped, so the series never silently loses
// a run — a run that exists but reports nothing is itself signal.
func (s *Store) RunMetrics(ctx context.Context, limit int) ([]RunMetric, error) {
	q := `SELECT id, kind, commit_sha, started_at, finished_at, stats_json
		FROM scan_runs
		WHERE finished_at IS NOT NULL
		ORDER BY started_at DESC, id DESC`
	var args []any
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	return queryRows(ctx, s, "run_metrics", q, args, func(rows *sql.Rows) (RunMetric, error) {
		var (
			m             RunMetric
			kind, started string
			finished      sql.NullString
			statsJSON     string
		)
		if err := rows.Scan(&m.ScanRunID, &kind, &m.CommitSHA, &started, &finished, &statsJSON); err != nil {
			return RunMetric{}, err
		}
		m.Kind = ScanKind(kind)
		var err error
		if m.StartedAt, err = parseTime(started); err != nil {
			return RunMetric{}, err
		}
		if finished.Valid {
			if m.FinishedAt, err = parseTime(finished.String); err != nil {
				return RunMetric{}, err
			}
		}
		if statsJSON != "" {
			var p runStatsProjection
			// A parse failure leaves the zero counters in place; the run is
			// still reported (see the doc comment).
			if json.Unmarshal([]byte(statsJSON), &p) == nil {
				m.Hypothesized = p.Hypothesized
				m.Verified = p.Verified
				m.Killed = p.Killed
				m.FinderRuns = p.FinderRuns
				m.InputTokens = p.InputTokens
				m.OutputTokens = p.OutputTokens
				m.CartographerEnabled = p.CartographerEnabled
			}
		}
		return m, nil
	})
}
