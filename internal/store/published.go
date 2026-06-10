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
	return err
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
		return PublishedIssue{}, err
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
// by fingerprint (ascending). An empty store returns a nil slice without error.
func (s *Store) ListPublishedIssues(ctx context.Context) ([]PublishedIssue, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT fingerprint, issue_number, state, created_at, updated_at
		   FROM published_issues
		  ORDER BY fingerprint ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []PublishedIssue
	for rows.Next() {
		var pi PublishedIssue
		var createdAt, updatedAt string
		if err := rows.Scan(&pi.Fingerprint, &pi.IssueNumber, &pi.State, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		if pi.CreatedAt, err = parseTime(createdAt); err != nil {
			return nil, err
		}
		if pi.UpdatedAt, err = parseTime(updatedAt); err != nil {
			return nil, err
		}
		out = append(out, pi)
	}
	return out, rows.Err()
}
