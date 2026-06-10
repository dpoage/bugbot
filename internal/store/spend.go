package store

import (
	"context"
	"time"
)

// Spend is one entry in the token-spend ledger: the tokens consumed by a single
// LLM call, attributed to a scan run, role, provider, and model.
//
// InputTokens is the TOTAL prompt size including any prompt-cache reads/writes
// (the llm.Usage convention), so input+output budget math is cache-agnostic.
// CacheReadTokens / CacheCreationTokens are subsets of InputTokens recording
// how much of the prompt was served from (read) or written to (creation) the
// provider's prompt cache; they exist to report cache savings, not to add to
// the total.
type Spend struct {
	ID                  string
	TS                  time.Time
	ScanRunID           string
	Role                string
	Provider            string
	Model               string
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
}

// SpendTotals is a rollup of token spend over some scope. CacheReadTokens and
// CacheCreationTokens are subsets of InputTokens (see Spend).
type SpendTotals struct {
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
}

// Total returns the sum of input and output tokens, the figure budget checks
// compare against per-cycle and per-day limits. Cached tokens are already
// included in InputTokens, so they are deliberately not added here.
func (t SpendTotals) Total() int64 { return t.InputTokens + t.OutputTokens }

// RecordSpend appends a ledger entry. If s.TS is zero it is set to now. The
// generated id is returned. ScanRunID may be empty for spend not tied to a run.
func (s *Store) RecordSpend(ctx context.Context, sp Spend) (string, error) {
	if sp.ID == "" {
		sp.ID = newID()
	}
	ts := sp.TS
	if ts.IsZero() {
		ts = nowUTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO spend
		  (id, ts, scan_run_id, role, provider, model, input_tokens, output_tokens,
		   cache_read_tokens, cache_creation_tokens)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		sp.ID, ts.UTC().Format(timeLayout), sp.ScanRunID, sp.Role, sp.Provider,
		sp.Model, sp.InputTokens, sp.OutputTokens,
		sp.CacheReadTokens, sp.CacheCreationTokens,
	)
	if err != nil {
		return "", err
	}
	return sp.ID, nil
}

// TotalsSince sums token spend with ts >= t. Used for per-day budget checks
// (pass the start of the current day) and any other time-windowed rollup.
func (s *Store) TotalsSince(ctx context.Context, t time.Time) (SpendTotals, error) {
	var tot SpendTotals
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
		       COALESCE(SUM(cache_read_tokens), 0), COALESCE(SUM(cache_creation_tokens), 0)
		FROM spend WHERE ts >= ?`, t.UTC().Format(timeLayout),
	).Scan(&tot.InputTokens, &tot.OutputTokens, &tot.CacheReadTokens, &tot.CacheCreationTokens)
	if err != nil {
		return SpendTotals{}, err
	}
	return tot, nil
}

// TotalsForScanRun sums token spend attributed to a single scan run. Used for
// per-cycle budget checks, where one investigation cycle maps to one scan run.
func (s *Store) TotalsForScanRun(ctx context.Context, scanRunID string) (SpendTotals, error) {
	var tot SpendTotals
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
		       COALESCE(SUM(cache_read_tokens), 0), COALESCE(SUM(cache_creation_tokens), 0)
		FROM spend WHERE scan_run_id = ?`, scanRunID,
	).Scan(&tot.InputTokens, &tot.OutputTokens, &tot.CacheReadTokens, &tot.CacheCreationTokens)
	if err != nil {
		return SpendTotals{}, err
	}
	return tot, nil
}
