package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// FinderTraversal is one row in the finder_traversals audit table. Each row
// represents a successful (finderOK) finder unit that optionally reported a
// traversal summary — the sites it enumerated and visited — alongside the
// candidate count it produced (possibly zero).
//
// A row with CandidateCount=0 and non-empty Enumerated is the primary
// observability signal: the finder reached sites and dismissed them all.
// Without this table, a zero-candidate finderOK unit is indistinguishable from
// a unit that never ran (bugbot-g7e).
//
// Only units that include a traversal field in their JSON output produce a row;
// units that omit the optional traversal field contribute nothing. No synthetic
// rows are created.
type FinderTraversal struct {
	ID          string
	ScanRunID   string
	Lens        string
	Strategy    string
	Files       []string // repo-relative file paths the unit was assigned
	Enumerated  []string // sites the finder considered as candidates for inspection
	Visited     []string // sites the finder actually traced in detail
	CandidateCount int
	CreatedAt   time.Time
}

// AddFinderTraversal inserts a single finder_traversals row. The ID is
// generated if empty. CreatedAt is set to now if zero. Files, Enumerated, and
// Visited are stored as JSON arrays. A failed insert is returned to the caller;
// the funnel caller treats it as best-effort and never aborts the scan.
func (s *Store) AddFinderTraversal(ctx context.Context, ft FinderTraversal) error {
	if ft.ID == "" {
		ft.ID = newID()
	}
	if ft.CreatedAt.IsZero() {
		ft.CreatedAt = nowUTC()
	}

	filesJSON, err := marshalStringSlice(ft.Files)
	if err != nil {
		return err
	}
	enumeratedJSON, err := marshalStringSlice(ft.Enumerated)
	if err != nil {
		return err
	}
	visitedJSON, err := marshalStringSlice(ft.Visited)
	if err != nil {
		return err
	}

	_, err = s.exec(ctx, "add_finder_traversal", `
		INSERT INTO finder_traversals
		  (id, scan_run_id, lens, strategy, files_json,
		   enumerated_json, visited_json, candidate_count, created_at)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		ft.ID, ft.ScanRunID, ft.Lens, ft.Strategy, filesJSON,
		enumeratedJSON, visitedJSON, ft.CandidateCount,
		ft.CreatedAt.UTC().Format(timeLayout),
	)
	return err
}

// ListFinderTraversals returns all finder_traversals rows for the given scan
// run, ordered by id (ascending). Recency of the audit feed is via scan_run_id,
// not per-row ordering; id is a ULID-style time-ordered id, so the ascending
// list matches the order rows were recorded in.
func (s *Store) ListFinderTraversals(ctx context.Context, scanRunID string) ([]FinderTraversal, error) {
	return queryRows(ctx, s, "list_finder_traversals", `
		SELECT id, scan_run_id, lens, strategy, files_json,
		       enumerated_json, visited_json, candidate_count, created_at
		FROM finder_traversals
		WHERE scan_run_id = ?
		ORDER BY id`, []any{scanRunID}, scanFinderTraversal)
}

// PruneFinderTraversals deletes finder_traversals rows whose scan_run_id is NOT
// among the keepRuns most recent scan_runs. It returns the number of rows
// deleted. Callers should treat a non-nil error as best-effort: log it and
// continue rather than aborting the scan.
//
// Recency is computed from the ULID-style id (id.go: later IDs sort after
// earlier ones), NOT from created_at: RFC3339Nano TEXT does not sort
// lexicographically across the second boundary — see the caution on
// filestate.go's epochSentinel and the matching pattern in PruneAgentUnits.
func (s *Store) PruneFinderTraversals(ctx context.Context, keepRuns int) (int64, error) {
	if keepRuns <= 0 {
		keepRuns = 1
	}
	res, err := s.exec(ctx, "prune_finder_traversals", `
		DELETE FROM finder_traversals
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
		return 0, annotateErr(s.path, "prune_finder_traversals", err)
	}
	return n, nil
}

// scanFinderTraversal reads one finder_traversals row from a *sql.Rows cursor.
func scanFinderTraversal(rows *sql.Rows) (FinderTraversal, error) {
	var ft FinderTraversal
	var filesJSON, enumeratedJSON, visitedJSON string
	var createdAt string

	if err := rows.Scan(
		&ft.ID, &ft.ScanRunID, &ft.Lens, &ft.Strategy,
		&filesJSON, &enumeratedJSON, &visitedJSON,
		&ft.CandidateCount, &createdAt,
	); err != nil {
		return FinderTraversal{}, err
	}

	if err := unmarshalStringSlice(filesJSON, &ft.Files); err != nil {
		return FinderTraversal{}, err
	}
	if err := unmarshalStringSlice(enumeratedJSON, &ft.Enumerated); err != nil {
		return FinderTraversal{}, err
	}
	if err := unmarshalStringSlice(visitedJSON, &ft.Visited); err != nil {
		return FinderTraversal{}, err
	}

	t, err := parseTime(createdAt)
	if err != nil {
		return FinderTraversal{}, err
	}
	ft.CreatedAt = t
	return ft, nil
}

// marshalStringSlice encodes a string slice as a compact JSON array. A nil
// slice is encoded as "[]" so columns default to the empty-array sentinel.
func marshalStringSlice(ss []string) (string, error) {
	if ss == nil {
		return "[]", nil
	}
	b, err := json.Marshal(ss)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// unmarshalStringSlice decodes a JSON array column into a string slice. An
// empty or "[]" value yields a nil slice.
func unmarshalStringSlice(s string, out *[]string) error {
	if s == "" || s == "[]" {
		*out = nil
		return nil
	}
	return json.Unmarshal([]byte(s), out)
}
