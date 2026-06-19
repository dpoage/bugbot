package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// AgentRole classifies which pipeline stage the agent served.
type AgentRole string

const (
	AgentRoleFinder     AgentRole = "finder"
	AgentRoleVerifier   AgentRole = "verifier"
	AgentRoleReproducer AgentRole = "reproducer"
)

// AgentStatus is the terminal outcome for an agent unit.
// The valid values vary by role; see AgentUnit documentation for the full
// vocabulary.
type AgentStatus string

const (
	// finder outcomes
	AgentStatusOK              AgentStatus = "ok"
	AgentStatusParseFailed     AgentStatus = "parse_failed"
	AgentStatusBudgetStopped   AgentStatus = "budget_stopped"
	AgentStatusSkippedHard     AgentStatus = "skipped_hard_budget"
	AgentStatusSkippedDegraded AgentStatus = "skipped_degraded"

	// verifier outcomes
	AgentStatusSurvived       AgentStatus = "survived"
	AgentStatusKilled         AgentStatus = "killed"
	AgentStatusOrphanedBudget AgentStatus = "orphaned_budget"

	// reproducer outcomes
	AgentStatusReproduced AgentStatus = "reproduced"
	AgentStatusExhausted  AgentStatus = "exhausted"
	AgentStatusInfraError AgentStatus = "infra_error"
)

// AgentStrategy is the finder strategy name; empty for non-finder roles.
type AgentStrategy string

// AgentUnit is one row in the agent_units observability table. Each row
// represents a single finder, verifier, or reproducer agent execution, or a
// unit that was skipped before launch (status = skipped_hard_budget or
// skipped_degraded). Skipped units carry zero tokens and an empty started_at.
//
// Status vocabulary (shared across roles):
//
//	finder:     ok, parse_failed, budget_stopped, skipped_hard_budget, skipped_degraded
//	verifier:   survived, killed, orphaned_budget
//	reproducer: reproduced, exhausted, infra_error
type AgentUnit struct {
	ID              string
	ScanRunID       string
	Role            AgentRole
	Lens            string // bare lens name (finder) or candidate's lens (verifier)
	Strategy        AgentStrategy
	LaunchOrder     int
	Files           []string  // target files for this unit; nil/empty for diff-intent and verifiers
	StartedAt       time.Time // zero for skipped units
	FinishedAt      time.Time // zero for skipped units
	Status          AgentStatus
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
		u.ID, u.ScanRunID, string(u.Role), u.Lens, string(u.Strategy),
		u.LaunchOrder, string(filesJSON),
		startedAt, finishedAt, string(u.Status),
		u.InputTokens, u.OutputTokens, u.CacheReadTokens,
		u.Candidates, u.LeadsPosted, u.Detail,
		u.CreatedAt.UTC().Format(timeLayout),
	)
	if err != nil {
		return annotateErr(s.path, "add_agent_unit", err)
	}
	return nil
}

// ListAgentUnits returns all agent_units rows for the given scan run, ordered
// by launch_order then id (both ascending). Do NOT reorder by any timestamp
// column: RFC3339Nano strings do not sort consistently (see filestate.go).
//
// launch_order is unique per scan_run (assigned at unit construction) and id
// is the unique primary key, so the ordering is already total and
// deterministic without a rowid tiebreak.
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
		return nil, annotateErr(s.path, "list_agent_units", err)
	}
	defer func() { _ = rows.Close() }()
	return scanRows(rows, scanAgentUnit)
}

// PruneAgentUnits deletes agent_unit rows whose scan_run_id is NOT among the
// keepRuns most recent scan_runs. It returns the number of rows deleted.
// Callers should treat a non-nil error as best-effort: log it and continue
// rather than aborting the scan.
func (s *Store) PruneAgentUnits(ctx context.Context, keepRuns int) (int64, error) {
	if keepRuns <= 0 {
		keepRuns = 1
	}
	// Identify the set of scan_run_ids to KEEP (the keepRuns most recent runs).
	// Recency is computed from the ULID-style id (id.go: later IDs sort after
	// earlier ones), NOT from started_at: RFC3339Nano TEXT does not sort
	// lexicographically across the second boundary ("...:17Z" > "...:17.5Z"
	// because 'Z' > '.'), so ORDER BY started_at silently keeps a wrong run
	// whenever timestamps straddle a whole second — see the caution on
	// filestate.go's epochSentinel.
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM agent_units
		WHERE scan_run_id NOT IN (
			SELECT id FROM scan_runs
			ORDER BY id DESC
			LIMIT ?
		)`, keepRuns,
	)
	if err != nil {
		return 0, annotateErr(s.path, "prune_agent_units", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, annotateErr(s.path, "prune_agent_units", err)
	}
	return n, nil
}

// scanAgentUnit reads one agent_units row from a *sql.Rows cursor.
func scanAgentUnit(rows *sql.Rows) (AgentUnit, error) {
	var u AgentUnit
	var role, strategy, status string
	var startedAt, finishedAt, createdAt string
	var filesJSON string

	if err := rows.Scan(
		&u.ID, &u.ScanRunID, &role, &u.Lens, &strategy,
		&u.LaunchOrder, &filesJSON,
		&startedAt, &finishedAt, &status,
		&u.InputTokens, &u.OutputTokens, &u.CacheReadTokens,
		&u.Candidates, &u.LeadsPosted, &u.Detail, &createdAt,
	); err != nil {
		return AgentUnit{}, err
	}

	u.Role = AgentRole(role)
	u.Strategy = AgentStrategy(strategy)
	u.Status = AgentStatus(status)

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
