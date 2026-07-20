package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
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
	// ManagedLabels is the set of bugbot-managed labels (severity:*,
	// bugbot:*) last applied to the GitHub issue; nil/empty for rows that
	// predate the feature or never had labels applied. Stored comma-joined
	// and sorted so the publish reconciler can diff desired vs. applied
	// labels without a gh read per issue.
	ManagedLabels []string
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

// encodeManagedLabels encodes a label list for the managed_labels column:
// comma-joined, sorted copy (the caller's slice is never mutated). Nil/empty
// yields the empty string, the column's "never applied" sentinel. Unlike
// corroborating_lenses (JSON-encoded, see encodeLenses), managed label names
// (severity:*, bugbot:*) can never contain commas and are only ever compared
// as whole sets, so the simpler encoding is stable and greppable.
func encodeManagedLabels(labels []string) string {
	if len(labels) == 0 {
		return ""
	}
	sorted := slices.Clone(labels)
	slices.Sort(sorted)
	return strings.Join(sorted, ",")
}

// decodeManagedLabels parses the managed_labels column back into a slice.
// The empty string yields nil.
func decodeManagedLabels(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// SetPublishedManagedLabels records the bugbot-managed labels last applied to
// the GitHub issue for fingerprint. labels is stored comma-joined and sorted
// (a copy is sorted; the caller's slice is not mutated); nil/empty clears the
// column back to the "never applied" sentinel. It deliberately does NOT bump
// updated_at: the publish planner compares finding.updated_at against
// published.updated_at to decide whether a body push is due, and label
// bookkeeping is not a body push. Setting labels for a fingerprint with no
// published_issues row is a nil no-op, mirroring DeletePublishedIssue's
// idempotency, so the reconciler needs no separate existence check.
// Comma-containing label names are unsupported: the column is comma-joined,
// so such a label would split into fragments on read. Bugbot-managed labels
// (severity:*, bugbot:*) never contain commas.
func (s *Store) SetPublishedManagedLabels(ctx context.Context, fingerprint string, labels []string) error {
	_, err := s.exec(ctx, "set_published_managed_labels",
		`UPDATE published_issues SET managed_labels = ? WHERE fingerprint = ?`,
		encodeManagedLabels(labels), fingerprint,
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
	var createdAt, updatedAt, managedLabels string
	err := s.queryRow(ctx, "get_published_issue",
		`SELECT fingerprint, issue_number, state, created_at, updated_at, body_hash, managed_labels
		   FROM published_issues
		  WHERE fingerprint = ?`,
		[]any{fingerprint},
		func(row *sql.Row) error {
			return row.Scan(&pi.Fingerprint, &pi.IssueNumber, &state, &createdAt, &updatedAt, &pi.BodyHash, &managedLabels)
		},
	)
	if errors.Is(err, sql.ErrNoRows) {
		return PublishedIssue{}, ErrNotFound
	}
	if err != nil {
		return PublishedIssue{}, err
	}
	pi.State = IssueState(state)
	pi.ManagedLabels = decodeManagedLabels(managedLabels)
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
		`SELECT fingerprint, issue_number, state, created_at, updated_at, body_hash, managed_labels
		   FROM published_issues
		  ORDER BY fingerprint ASC, rowid ASC`,
		nil,
		func(r *sql.Rows) (PublishedIssue, error) {
			var pi PublishedIssue
			var state string
			var createdAt, updatedAt, managedLabels string
			if err := r.Scan(&pi.Fingerprint, &pi.IssueNumber, &state, &createdAt, &updatedAt, &pi.BodyHash, &managedLabels); err != nil {
				return PublishedIssue{}, err
			}
			pi.State = IssueState(state)
			pi.ManagedLabels = decodeManagedLabels(managedLabels)
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
