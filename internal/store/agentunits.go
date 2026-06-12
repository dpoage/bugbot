package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// AgentUnit is one row in the agent_units observability table. Each row
// represents a single finder, verifier, or reproducer agent execution, or a
// unit that was skipped before launch (status = skipped_hard_budget or
// skipped_degraded). Skipped units carry zero tokens and an empty started_at.
//
// Status vocabulary (shared across roles):
//
//	finder:     ok, parse_failed, budget_stopped, skipped_hard_budget, skipped_degraded
//	verifier:   survived, killed, orphaned_budget
//	reproducer: reproduced, exhausted, invalid_plan, infra_error
type AgentUnit struct {
	ID              string
	ScanRunID       string
	Role            string    // "finder" | "verifier" | "reproducer"
	Lens            string    // bare lens name (finder) or candidate's lens (verifier)
	Strategy        string    // finder strategy name; "" for other roles
	LaunchOrder     int       // index in the units slice at construction time
	Files           []string  // target files for this unit; nil/empty for diff-intent and verifiers
	StartedAt       time.Time // zero for skipped units
	FinishedAt      time.Time // zero for skipped units
	Status          string    // see status vocabulary above
	InputTokens     int64
	OutputTokens    int64
	CacheReadTokens int64
	Candidates      int    // finder: candidates emitted; verifier: 1 if survived else 0
	LeadsPosted     int    // finder only
	Detail          string // verifier: seat/arbiter summary; repro: outcome note
	CreatedAt       time.Time
}

// AddAgentUnit inserts a single agent_units row. The ID is generated if empty.
// CreatedAt is set to now if zero.
func (s *Store) AddAgentUnit(ctx context.Context, u AgentUnit) error {
	if u.ID == "" {
		u.ID = newID()
	}
	if u.CreatedAt.IsZero() {
		u.CreatedAt = nowUTC()
	}

	filesJSON, err := json.Marshal(u.Files)
	if err != nil {
		filesJSON = []byte("[]")
	}

	var startedAt, finishedAt string
	if !u.StartedAt.IsZero() {
		startedAt = u.StartedAt.UTC().Format(timeLayout)
	}
	if !u.FinishedAt.IsZero() {
		finishedAt = u.FinishedAt.UTC().Format(timeLayout)
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO agent_units
		  (id, scan_run_id, role, lens, strategy, launch_order, files_json,
		   started_at, finished_at, status,
		   input_tokens, output_tokens, cache_read_tokens,
		   candidates, leads_posted, detail, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		u.ID, u.ScanRunID, u.Role, u.Lens, u.Strategy,
		u.LaunchOrder, string(filesJSON),
		startedAt, finishedAt, u.Status,
		u.InputTokens, u.OutputTokens, u.CacheReadTokens,
		u.Candidates, u.LeadsPosted, u.Detail,
		u.CreatedAt.UTC().Format(timeLayout),
	)
	return err
}

// ListAgentUnits returns all agent_units rows for the given scan run, ordered
// by launch_order then id (both ascending). Do NOT reorder by any timestamp
// column: RFC3339Nano strings do not sort consistently (see filestate.go).
func (s *Store) ListAgentUnits(ctx context.Context, scanRunID string) ([]AgentUnit, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, scan_run_id, role, lens, strategy, launch_order, files_json,
		       started_at, finished_at, status,
		       input_tokens, output_tokens, cache_read_tokens,
		       candidates, leads_posted, detail, created_at
		FROM agent_units
		WHERE scan_run_id = ?
		ORDER BY launch_order, id`,
		scanRunID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []AgentUnit
	for rows.Next() {
		u, err := scanAgentUnit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// PruneAgentUnits deletes agent_unit rows whose scan_run_id is NOT among the
// keepRuns most recent scan_runs (ordered by started_at then id). It returns
// the number of rows deleted. Callers should treat a non-nil error as
// best-effort: log it and continue rather than aborting the scan.
func (s *Store) PruneAgentUnits(ctx context.Context, keepRuns int) (int64, error) {
	if keepRuns <= 0 {
		keepRuns = 1
	}
	// Identify the set of scan_run_ids to KEEP (the keepRuns most recent runs).
	// We keep by started_at DESC, id DESC so the ordering is stable when two
	// runs share the same started_at. Do NOT order by rowid (not guaranteed to
	// align with started_at for backfilled rows).
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM agent_units
		WHERE scan_run_id NOT IN (
			SELECT id FROM scan_runs
			ORDER BY started_at DESC, id DESC
			LIMIT ?
		)`, keepRuns,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// scanAgentUnit reads one agent_units row from a *sql.Rows cursor.
func scanAgentUnit(rows *sql.Rows) (AgentUnit, error) {
	var u AgentUnit
	var startedAt, finishedAt, createdAt string
	var filesJSON string

	if err := rows.Scan(
		&u.ID, &u.ScanRunID, &u.Role, &u.Lens, &u.Strategy,
		&u.LaunchOrder, &filesJSON,
		&startedAt, &finishedAt, &u.Status,
		&u.InputTokens, &u.OutputTokens, &u.CacheReadTokens,
		&u.Candidates, &u.LeadsPosted, &u.Detail, &createdAt,
	); err != nil {
		return AgentUnit{}, err
	}

	if err := json.Unmarshal([]byte(filesJSON), &u.Files); err != nil {
		u.Files = nil
	}
	if startedAt != "" {
		t, err := parseTime(startedAt)
		if err != nil {
			return AgentUnit{}, err
		}
		u.StartedAt = t
	}
	if finishedAt != "" {
		t, err := parseTime(finishedAt)
		if err != nil {
			return AgentUnit{}, err
		}
		u.FinishedAt = t
	}
	t, err := parseTime(createdAt)
	if err != nil {
		return AgentUnit{}, err
	}
	u.CreatedAt = t
	return u, nil
}
