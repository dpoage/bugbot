package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// IssueState is the lifecycle state of a published GitHub issue.
type IssueState string

const (
	// IssueStatePending records that a create was started but not yet confirmed
	// (crash-safe tombstone: the next run recovers by searching for the
	// fingerprint marker and adopting or recreating the issue).
	IssueStatePending IssueState = "pending"
	// IssueStateOpen is the normal post-create state.
	IssueStateOpen IssueState = "open"
	// IssueStateClosing is set once the auto-close comment lands; a subsequent
	// run will issue the PATCH to close the GitHub issue.
	IssueStateClosing IssueState = "closing"
	// IssueStateClosed is the terminal state after the GitHub issue is closed.
	IssueStateClosed IssueState = "closed"
)

// PublishedIssue records that a finding has been filed as a GitHub issue. It
// is keyed by fingerprint so the publish reconciler can look up the issue
// number for any finding without scanning a secondary index.
type PublishedIssue struct {
	Fingerprint string
	IssueNumber int
	State       IssueState
	CreatedAt   time.Time
	UpdatedAt   time.Time
	// BodyHash is the sha256 hex digest of the issue body last pushed to
	// GitHub (create/update/reopen), or "" when no body has been pushed yet
	// (pending/closing/closed rows, or a pre-migration row). The publish
	// apply loop compares this against a freshly rendered body's hash before
	// issuing a PATCH, so a metadata-only finding touch (impact sweep,
	// AddCorroboratingLenses, AppendFindingSites) that leaves the rendered
	// body unchanged no longer costs a no-op gh call.
	BodyHash string
}

// UpsertPublishedIssue records (or refreshes) the GitHub issue linked to a
// finding fingerprint. On conflict it updates issue_number, state, body_hash,
// and updated_at while preserving created_at, so a re-create after a manual
// close records the new number without losing the original creation
// timestamp. bodyHash is the sha256 hex digest of the body last pushed to
// GitHub for this row, or "" for actions that never push a body (pending,
// closing, closed, adopt) — callers must pass it explicitly so an upsert can
// never silently retain a stale hash from a previous, different body.
func (s *Store) UpsertPublishedIssue(ctx context.Context, fingerprint string, issueNumber int, state IssueState, bodyHash string) error {
	now := nowUTC().Format(timeLayout)
	_, err := s.exec(ctx, "upsert_published_issue", `
		INSERT INTO published_issues (fingerprint, issue_number, state, created_at, updated_at, body_hash)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(fingerprint) DO UPDATE SET
		  issue_number = excluded.issue_number,
		  state        = excluded.state,
		  updated_at   = excluded.updated_at,
		  body_hash    = excluded.body_hash`,
		fingerprint, issueNumber, string(state), now, now, bodyHash,
	)
	if err != nil {
		return err
	}
	return nil
}

// DeletePublishedIssue removes the published_issues row for fingerprint. It is
// used by the publish reconciler when a GitHub issue has been deleted
// (HTTP 410) or transferred/renamed (HTTP 404) and the local row is stale;
// the caller recreates the issue with a fresh number in the same run. The
// method is idempotent: deleting a row that does not exist is not an error,
// so callers in the reconcile loop do not need a separate "is it there?"
// check. Returns nil even when no row was deleted, mirroring the pattern
// used by other delete methods in this package.
func (s *Store) DeletePublishedIssue(ctx context.Context, fingerprint string) error {
	_, err := s.exec(ctx, "delete_published_issue",
		`DELETE FROM published_issues WHERE fingerprint = ?`,
		fingerprint,
	)
	if err != nil {
		return err
	}
	return nil
}

// GetPublishedIssue returns the published_issues row for fingerprint, or
// ErrNotFound if no issue has been filed for this finding.
func (s *Store) GetPublishedIssue(ctx context.Context, fingerprint string) (PublishedIssue, error) {
	var pi PublishedIssue
	var state string
	var createdAt, updatedAt string
	err := s.queryRow(ctx, "get_published_issue",
		`SELECT fingerprint, issue_number, state, created_at, updated_at, body_hash
		   FROM published_issues
		  WHERE fingerprint = ?`,
		[]any{fingerprint},
		func(row *sql.Row) error {
			return row.Scan(&pi.Fingerprint, &pi.IssueNumber, &state, &createdAt, &updatedAt, &pi.BodyHash)
		},
	)
	if errors.Is(err, sql.ErrNoRows) {
		return PublishedIssue{}, ErrNotFound
	}
	if err != nil {
		return PublishedIssue{}, err
	}
	pi.State = IssueState(state)
	var perr error
	if pi.CreatedAt, perr = parseTime(createdAt); perr != nil {
		return PublishedIssue{}, perr
	}
	if pi.UpdatedAt, perr = parseTime(updatedAt); perr != nil {
		return PublishedIssue{}, perr
	}
	return pi, nil
}

// ListPublishedIssues returns all published_issues rows ordered deterministically
// by fingerprint (ascending), tiebroken by rowid so a future schema that
// permits duplicate fingerprints (or a non-unique secondary key) still
// returns a stable order. fingerprint is the PK today so the tiebreak is
// theoretically inactive; the column reference costs nothing and keeps
// the contract honest if a later migration ever changes the uniqueness.
func (s *Store) ListPublishedIssues(ctx context.Context) ([]PublishedIssue, error) {
	return queryRows(ctx, s, "list_published_issues",
		`SELECT fingerprint, issue_number, state, created_at, updated_at, body_hash
		   FROM published_issues
		  ORDER BY fingerprint ASC, rowid ASC`,
		nil,
		func(r *sql.Rows) (PublishedIssue, error) {
			var pi PublishedIssue
			var state string
			var createdAt, updatedAt string
			if err := r.Scan(&pi.Fingerprint, &pi.IssueNumber, &state, &createdAt, &updatedAt, &pi.BodyHash); err != nil {
				return PublishedIssue{}, err
			}
			pi.State = IssueState(state)
			var perr error
			if pi.CreatedAt, perr = parseTime(createdAt); perr != nil {
				return PublishedIssue{}, perr
			}
			if pi.UpdatedAt, perr = parseTime(updatedAt); perr != nil {
				return PublishedIssue{}, perr
			}
			return pi, nil
		})
}

// CountPublishedIssues tallies published_issues rows by state for the status
// world-state block. The zero map means nothing has ever been published.
//
// Uses queryRows (not the writer-only query()) so it is safe to call from
// read-only callers such as the status command (internal/cli/worldstate.go).
func (s *Store) CountPublishedIssues(ctx context.Context) (map[IssueState]int, error) {
	type row struct {
		state string
		n     int
	}
	rows, err := queryRows(ctx, s, "count_published_issues",
		`SELECT state, COUNT(*) FROM published_issues GROUP BY state`, nil,
		func(r *sql.Rows) (row, error) {
			var v row
			return v, r.Scan(&v.state, &v.n)
		})
	if err != nil {
		return nil, err
	}
	out := map[IssueState]int{}
	for _, v := range rows {
		out[IssueState(v.state)] = v.n
	}
	return out, nil
}
