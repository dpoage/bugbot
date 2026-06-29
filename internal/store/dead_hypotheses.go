package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// DeadHypothesis is one row in the dead_hypotheses audit table. Each row
// represents a candidate that the verifier panel (or arbiter on a split)
// decided to kill, persisted with the structured kill-verdict breakdown plus
// the buildReasoning trace so the kill can be audited post-hoc.
//
// Triage drops and budget-orphaned candidates are NOT recorded here (triage
// drops are pure counters; orphans are already durable as T3 findings).
//
// The structured breakdown columns (SeatNames, RefutedCount, TotalSeats,
// ArbiterRan, ArbiterRefuted, ArbiterVerdict) are the queryable surface —
// built from the same compact shape agent_units.Detail uses. ReasoningTrace
// is the buildReasoning output and IS allowed to hold model-authored free
// text: this is a deliberately-scoped audit store, unlike agent_units.Detail
// (see observability.go: "no model-authored free text").
type DeadHypothesis struct {
	ID          string
	ScanRunID   string
	Fingerprint string
	Lens        string
	File        string
	Line        int
	Title       string
	Severity    string

	SeatNames      []string // refuter seat names; empty for single-refuter or no-refuter paths
	RefutedCount   int      // seats whose verdict was Refuted
	TotalSeats     int      // len(SeatNames) at the moment of the kill
	ArbiterRan     bool
	ArbiterRefuted bool   // meaningful only when ArbiterRan
	ArbiterVerdict string // "refuted" | "survived" | "" (no arbiter, or parse-fail fallback)

	ReasoningTrace string

	CreatedAt time.Time
}

// AddDeadHypothesis inserts a single dead_hypotheses row. The ID is generated
// if empty. CreatedAt is set to now if zero. SeatNames is stored as a
// comma-separated string (seat names are short, well-controlled, and not used
// for joining). A failed insert is returned to the caller; the funnel caller
// treats it as best-effort and never aborts the scan.
func (s *Store) AddDeadHypothesis(ctx context.Context, h DeadHypothesis) error {
	if h.ID == "" {
		h.ID = newID()
	}
	if h.CreatedAt.IsZero() {
		h.CreatedAt = nowUTC()
	}

	arbiterVerdict := h.ArbiterVerdict
	arbiterRan := 0
	arbiterRefuted := 0
	if h.ArbiterRan {
		arbiterRan = 1
		if h.ArbiterRefuted {
			arbiterRefuted = 1
		}
	}

	_, err := s.exec(ctx, "add_dead_hypothesis", `
		INSERT INTO dead_hypotheses
		  (id, scan_run_id, fingerprint, lens, file, line, title, severity,
		   seat_names, refuted_count, total_seats,
		   arbiter_ran, arbiter_refuted, arbiter_verdict,
		   reasoning_trace, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		h.ID, h.ScanRunID, h.Fingerprint, h.Lens, h.File, h.Line, h.Title, h.Severity,
		strings.Join(h.SeatNames, ","), h.RefutedCount, h.TotalSeats,
		arbiterRan, arbiterRefuted, arbiterVerdict,
		h.ReasoningTrace,
		h.CreatedAt.UTC().Format(timeLayout),
	)
	if err != nil {
		return err
	}
	return nil
}

// ListDeadHypotheses returns all dead_hypotheses rows for the given scan run,
// ordered by id (ascending). Recency of the audit feed is via scan_run_id, not
// per-row ordering; id is a ULID-style time-ordered id, so the ascending list
// matches the order kills were recorded in.
func (s *Store) ListDeadHypotheses(ctx context.Context, scanRunID string) ([]DeadHypothesis, error) {
	return queryRows(ctx, s, "list_dead_hypotheses", `
		SELECT id, scan_run_id, fingerprint, lens, file, line, title, severity,
		       seat_names, refuted_count, total_seats,
		       arbiter_ran, arbiter_refuted, arbiter_verdict,
		       reasoning_trace, created_at
		FROM dead_hypotheses
		WHERE scan_run_id = ?
		ORDER BY id`, []any{scanRunID}, scanDeadHypothesis)
}

// PruneDeadHypotheses deletes dead_hypothesis rows whose scan_run_id is NOT
// among the keepRuns most recent scan_runs. It returns the number of rows
// deleted. Callers should treat a non-nil error as best-effort: log it and
// continue rather than aborting the scan.
//
// Recency is computed from the ULID-style id (id.go: later IDs sort after
// earlier ones), NOT from created_at: RFC3339Nano TEXT does not sort
// lexicographically across the second boundary — see the caution on
// filestate.go's epochSentinel and the matching pattern in PruneAgentUnits.
func (s *Store) PruneDeadHypotheses(ctx context.Context, keepRuns int) (int64, error) {
	if keepRuns <= 0 {
		keepRuns = 1
	}
	res, err := s.exec(ctx, "prune_dead_hypotheses", `
		DELETE FROM dead_hypotheses
		WHERE scan_run_id NOT IN (
			SELECT id FROM scan_runs
			ORDER BY id DESC
			LIMIT ?
		)`, keepRuns,
	)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, annotateErr(s.path, "prune_dead_hypotheses", err)
	}
	return n, nil
}

// scanDeadHypothesis reads one dead_hypotheses row from a *sql.Rows cursor.
func scanDeadHypothesis(rows *sql.Rows) (DeadHypothesis, error) {
	var h DeadHypothesis
	var seatNames string
	var arbiterRan, arbiterRefuted int
	var createdAt string

	if err := rows.Scan(
		&h.ID, &h.ScanRunID, &h.Fingerprint, &h.Lens, &h.File, &h.Line, &h.Title, &h.Severity,
		&seatNames, &h.RefutedCount, &h.TotalSeats,
		&arbiterRan, &arbiterRefuted, &h.ArbiterVerdict,
		&h.ReasoningTrace, &createdAt,
	); err != nil {
		return DeadHypothesis{}, err
	}

	if seatNames != "" {
		h.SeatNames = strings.Split(seatNames, ",")
	}
	h.ArbiterRan = arbiterRan != 0
	h.ArbiterRefuted = arbiterRefuted != 0

	t, err := parseTime(createdAt)
	if err != nil {
		return DeadHypothesis{}, err
	}
	h.CreatedAt = t
	return h, nil
}
