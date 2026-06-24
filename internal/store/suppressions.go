package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Suppression records that a fingerprint was dismissed by a maintainer. Its
// presence causes UpsertFinding to keep that fingerprint dismissed forever.
type Suppression struct {
	Fingerprint string
	Reason      string
	CreatedAt   time.Time
}

// AddSuppression records a suppression for the fingerprint and flips any
// existing finding with that fingerprint to StatusDismissed, in one
// transaction. It is idempotent: re-suppressing an already-suppressed
// fingerprint updates the reason and leaves the original created_at.
//
// This is the public entry point for the triage/report layer to dismiss a
// finding. The matching finding (if any) need not exist yet — suppressing a
// fingerprint pre-emptively still prevents a future UpsertFinding from opening
// it.
func (s *Store) AddSuppression(ctx context.Context, fingerprint, reason string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if err := addSuppressionTx(ctx, tx, fingerprint, reason); err != nil {
			return annotateErr(s.path, "add_suppression", err)
		}
		// Flip any existing finding to dismissed so it stops being reported.
		if _, err := tx.ExecContext(ctx,
			`UPDATE findings SET status = ?, updated_at = ? WHERE fingerprint = ?`,
			string(StatusDismissed), nowUTC().Format(timeLayout), fingerprint,
		); err != nil {
			return annotateErr(s.path, "add_suppression", err)
		}
		return nil
	})
}

// IsSuppressed reports whether the fingerprint has been dismissed. Triage calls
// this to skip candidates the maintainers have already rejected.
func (s *Store) IsSuppressed(ctx context.Context, fingerprint string) (bool, error) {
	var one int
	err := s.queryRow(ctx, "is_suppressed",
		`SELECT 1 FROM suppressions WHERE fingerprint = ?`,
		[]any{fingerprint},
		func(row *sql.Row) error {
			return row.Scan(&one)
		},
	)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ListSuppressions returns all suppressions, newest first. The primary key is
// created_at DESC; rowid is the tiebreaker so a batch of suppressions added
// in the same instant (identical created_at) returns in a stable insertion
// order across runs. Without the tiebreak, two calls could legitimately
// return tied rows in different orders on different drivers/versions.
func (s *Store) ListSuppressions(ctx context.Context) ([]Suppression, error) {
	return queryRows(ctx, s, "list_suppressions",
		`SELECT fingerprint, reason, created_at FROM suppressions ORDER BY created_at DESC, rowid DESC`,
		nil,
		func(r *sql.Rows) (Suppression, error) {
			var sp Suppression
			var created string
			if err := r.Scan(&sp.Fingerprint, &sp.Reason, &created); err != nil {
				return Suppression{}, err
			}
			var perr error
			if sp.CreatedAt, perr = parseTime(created); perr != nil {
				return Suppression{}, perr
			}
			return sp, nil
		})
}

// addSuppressionTx upserts a suppression within an existing transaction. On
// conflict it updates the reason but preserves the original created_at.
func addSuppressionTx(ctx context.Context, tx *sql.Tx, fingerprint, reason string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO suppressions (fingerprint, reason, created_at)
		VALUES (?, ?, ?)
		ON CONFLICT(fingerprint) DO UPDATE SET reason = excluded.reason`,
		fingerprint, reason, nowUTC().Format(timeLayout),
	)
	return err
}

// isSuppressedTx is the transactional form of IsSuppressed.
func isSuppressedTx(ctx context.Context, tx *sql.Tx, fingerprint string) (bool, error) {
	var one int
	err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM suppressions WHERE fingerprint = ?`, fingerprint).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
