package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// PublishedIssue records that a finding has been filed as a GitHub issue. It
// is keyed by fingerprint so the publish reconciler can look up the issue
// number for any finding without scanning a secondary index.
type PublishedIssue struct {
	Fingerprint string
	IssueNumber int
	State       string // "open" or "closed"
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// UpsertPublishedIssue records (or refreshes) the GitHub issue linked to a
// finding fingerprint. On conflict it updates issue_number, state, and
// updated_at while preserving created_at, so a re-create after a manual close
// records the new number without losing the original creation timestamp.
func (s *Store) UpsertPublishedIssue(ctx context.Context, fingerprint string, issueNumber int, state string) error {
	now := nowUTC().Format(timeLayout)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO published_issues (fingerprint, issue_number, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(fingerprint) DO UPDATE SET
		  issue_number = excluded.issue_number,
		  state        = excluded.state,
		  updated_at   = excluded.updated_at`,
		fingerprint, issueNumber, state, now, now,
	)
	if err != nil {
		return annotateErr(s.path, "upsert_published_issue", err)
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
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM published_issues WHERE fingerprint = ?`,
		fingerprint,
	)
	if err != nil {
		return annotateErr(s.path, "delete_published_issue", err)
	}
	return nil
}

// GetPublishedIssue returns the published_issues row for fingerprint, or
// ErrNotFound if no issue has been filed for this finding.
func (s *Store) GetPublishedIssue(ctx context.Context, fingerprint string) (PublishedIssue, error) {
	var pi PublishedIssue
	var createdAt, updatedAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT fingerprint, issue_number, state, created_at, updated_at
		   FROM published_issues
		  WHERE fingerprint = ?`, fingerprint,
	).Scan(&pi.Fingerprint, &pi.IssueNumber, &pi.State, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PublishedIssue{}, ErrNotFound
	}
	if err != nil {
		return PublishedIssue{}, annotateErr(s.path, "get_published_issue", err)
	}
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
	rows, err := s.db.QueryContext(ctx,
		`SELECT fingerprint, issue_number, state, created_at, updated_at
		   FROM published_issues
		  ORDER BY fingerprint ASC, rowid ASC`,
	)
	if err != nil {
		return nil, annotateErr(s.path, "list_published_issues", err)
	}
	defer func() { _ = rows.Close() }()
	return scanRows(rows, func(r *sql.Rows) (PublishedIssue, error) {
		var pi PublishedIssue
		var createdAt, updatedAt string
		if err := r.Scan(&pi.Fingerprint, &pi.IssueNumber, &pi.State, &createdAt, &updatedAt); err != nil {
			return PublishedIssue{}, err
		}
		if pi.CreatedAt, err = parseTime(createdAt); err != nil {
			return PublishedIssue{}, err
		}
		if pi.UpdatedAt, err = parseTime(updatedAt); err != nil {
			return PublishedIssue{}, err
		}
		return pi, nil
	})
}

// CountPublishedIssues tallies published_issues rows by state for the status
// world-state block. The zero map means nothing has ever been published.
func (s *Store) CountPublishedIssues(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT state, COUNT(*) FROM published_issues GROUP BY state`)
	if err != nil {
		return nil, annotateErr(s.path, "count_published_issues", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	_, err = scanRows(rows, func(r *sql.Rows) (struct{}, error) {
		var state string
		var n int
		if err := r.Scan(&state, &n); err != nil {
			return struct{}{}, err
		}
		out[state] = n
		return struct{}{}, nil
	})
	return out, err
}
