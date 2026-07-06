package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ReproState is the lifecycle state of a repro_attempts row.
type ReproState string

const (
	// ReproStatePending: initial state; eligible to be claimed.
	ReproStatePending ReproState = "pending"
	// ReproStateRunning: claimed by a worker; an attempt is in progress.
	ReproStateRunning ReproState = "running"
	// ReproStateDone: terminal state; reproduction either succeeded or
	// definitively failed (finding could not be reproduced).
	ReproStateDone ReproState = "done"
	// ReproStateInfraRetry: an infrastructure error occurred; will be retried on
	// the next cycle if attempt_count < max_attempts.
	ReproStateInfraRetry ReproState = "infra_retry"
	// ReproStateAbandoned: terminal state; infra errors exceeded max_attempts.
	ReproStateAbandoned ReproState = "abandoned"
)

// DefaultReproMaxAttempts is the bounded retry cap for infrastructure errors.
const DefaultReproMaxAttempts = 3

// ReproAttempt is one row in the repro_attempts queue table.
type ReproAttempt struct {
	ID           string
	Fingerprint  string
	State        ReproState
	AttemptCount int
	MaxAttempts  int
	LastError    string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// EnqueueRepro inserts a repro_attempts row for the given fingerprint if none
// exists yet. Idempotent: a second call for the same fingerprint is a no-op
// (the UNIQUE constraint on fingerprint is enforced by idx_repro_attempts_fingerprint).
// Returns the inserted (or existing) row.
func (s *Store) EnqueueRepro(ctx context.Context, fingerprint string) (ReproAttempt, error) {
	if fingerprint == "" {
		return ReproAttempt{}, fmt.Errorf("store: EnqueueRepro requires a fingerprint")
	}
	now := nowUTC()
	id := newID()
	// INSERT OR IGNORE: if a row already exists, this is a no-op; we then SELECT
	// the existing row to return it.
	err := s.retry(ctx, func() error {
		_, err := s.db.ExecContext(ctx, `
			INSERT OR IGNORE INTO repro_attempts
			  (id, fingerprint, state, attempt_count, max_attempts, last_error, created_at, updated_at)
			VALUES (?, ?, ?, 0, ?, '', ?, ?)`,
			id, fingerprint, string(ReproStatePending), DefaultReproMaxAttempts,
			now.Format(timeLayout), now.Format(timeLayout),
		)
		return err
	})
	if err != nil {
		return ReproAttempt{}, annotateErr(s.path, "enqueue_repro", err)
	}
	return s.GetReproAttempt(ctx, fingerprint)
}

// GetReproAttempt returns the repro_attempts row for the given fingerprint, or
// ErrNotFound if no row exists.
func (s *Store) GetReproAttempt(ctx context.Context, fingerprint string) (ReproAttempt, error) {
	var ra ReproAttempt
	var state, createdAt, updatedAt string
	err := s.retry(ctx, func() error {
		return s.db.QueryRowContext(ctx,
			`SELECT id, fingerprint, state, attempt_count, max_attempts, last_error, created_at, updated_at
			 FROM repro_attempts WHERE fingerprint = ?`, fingerprint,
		).Scan(&ra.ID, &ra.Fingerprint, &state, &ra.AttemptCount, &ra.MaxAttempts, &ra.LastError, &createdAt, &updatedAt)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return ReproAttempt{}, ErrNotFound
	}
	if err != nil {
		return ReproAttempt{}, annotateErr(s.path, "get_repro_attempt", err)
	}
	ra.State = ReproState(state)
	var perr error
	if ra.CreatedAt, perr = parseTime(createdAt); perr != nil {
		return ReproAttempt{}, perr
	}
	if ra.UpdatedAt, perr = parseTime(updatedAt); perr != nil {
		return ReproAttempt{}, perr
	}
	return ra, nil
}

// ReproStaleLeaseDuration is the maximum time a repro_attempts row may stay
// in state 'running' before it is considered crash-stuck and eligible for
// reclaim. A hard crash between ClaimReproAttempt and Finish/Requeue leaves
// the row 'running' forever; this timeout bounds the permanent-loss window.
const ReproStaleLeaseDuration = 30 * time.Minute

// ClaimReproAttempt atomically transitions a repro_attempts row from
// pending/infra_retry → running, incrementing attempt_count. It also reclaims
// crash-stuck 'running' rows whose updated_at is older than ReproStaleLeaseDuration,
// treating them as infra_retry (the process that held the lease is presumed dead).
// Returns ErrReproAlreadyClaimed if the row is not claimable (already running with
// a fresh lease, done, or abandoned, or attempt budget exhausted).
//
// The UPDATE … WHERE state IN ('pending','infra_retry') AND attempt_count < max_attempts
// is the single atomic claim gate: concurrent writers are serialized by SQLite's
// single-connection MaxOpenConns(1) constraint, so exactly one wins per fingerprint.
func (s *Store) ClaimReproAttempt(ctx context.Context, fingerprint string) (ReproAttempt, error) {
	now := nowUTC()
	staleThreshold := now.Add(-ReproStaleLeaseDuration)

	var rowsAffected int64
	err := s.retry(ctx, func() error {
		// First: reclaim any crash-stuck 'running' row older than the stale threshold.
		// Move it to infra_retry so the normal claim path picks it up immediately below.
		_, err := s.db.ExecContext(ctx, `
			UPDATE repro_attempts
			SET state = ?, last_error = 'stale lease: presumed-dead worker', updated_at = ?
			WHERE fingerprint = ?
			  AND state = 'running'
			  AND updated_at < ?
			  AND attempt_count < max_attempts`,
			string(ReproStateInfraRetry), now.Format(timeLayout), fingerprint,
			staleThreshold.Format(timeLayout),
		)
		if err != nil {
			return err
		}

		// Normal claim: pending or infra_retry, within budget.
		res, err := s.db.ExecContext(ctx, `
			UPDATE repro_attempts
			SET state = ?, attempt_count = attempt_count + 1, updated_at = ?
			WHERE fingerprint = ?
			  AND state IN ('pending', 'infra_retry')
			  AND attempt_count < max_attempts`,
			string(ReproStateRunning), now.Format(timeLayout), fingerprint,
		)
		if err != nil {
			return err
		}
		rowsAffected, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return ReproAttempt{}, annotateErr(s.path, "claim_repro_attempt", err)
	}
	if rowsAffected == 0 {
		// Not claimable: already running (fresh lease), done, abandoned, or exhausted.
		return ReproAttempt{}, ErrReproAlreadyClaimed
	}
	return s.GetReproAttempt(ctx, fingerprint)
}

// ErrReproAlreadyClaimed is returned by ClaimReproAttempt when the row is not
// in a claimable state (already running, done, abandoned, or exhausted retries).
var ErrReproAlreadyClaimed = fmt.Errorf("store: repro attempt already claimed or exhausted")

// FinishReproAttempt transitions a running repro_attempts row to done. It is
// called on both success and definitive failure (finding simply did not
// reproduce). Infra errors must use RequeueReproAttemptOnInfraError instead.
func (s *Store) FinishReproAttempt(ctx context.Context, fingerprint string) error {
	now := nowUTC()
	return s.retry(ctx, func() error {
		res, err := s.db.ExecContext(ctx, `
			UPDATE repro_attempts SET state = ?, updated_at = ?
			WHERE fingerprint = ? AND state = 'running'`,
			string(ReproStateDone), now.Format(timeLayout), fingerprint,
		)
		if err != nil {
			return annotateErr(s.path, "finish_repro_attempt", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return annotateErr(s.path, "finish_repro_attempt", err)
		}
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// RequeueReproAttemptOnInfraError transitions a running repro_attempts row back
// to infra_retry (if attempt_count < max_attempts) or to abandoned (if the
// budget is exhausted). The lastError string is stored for diagnostics.
func (s *Store) RequeueReproAttemptOnInfraError(ctx context.Context, fingerprint, lastError string) error {
	now := nowUTC()
	return s.retry(ctx, func() error {
		_, err := s.db.ExecContext(ctx, `
			UPDATE repro_attempts
			SET state = CASE WHEN attempt_count < max_attempts THEN ? ELSE ? END,
			    last_error = ?,
			    updated_at = ?
			WHERE fingerprint = ? AND state = 'running'`,
			string(ReproStateInfraRetry), string(ReproStateAbandoned),
			lastError, now.Format(timeLayout), fingerprint,
		)
		if err != nil {
			return annotateErr(s.path, "requeue_repro_attempt_infra_error", err)
		}
		return nil
	})
}

// PendingReproAttempts returns all repro_attempts rows in pending or infra_retry
// state with attempt_count < max_attempts, ordered oldest-updated-first. This
// is the queue poll for all three dispatch paths.
func (s *Store) PendingReproAttempts(ctx context.Context) ([]ReproAttempt, error) {
	return queryRows(ctx, s, "pending_repro_attempts", `
		SELECT id, fingerprint, state, attempt_count, max_attempts, last_error, created_at, updated_at
		FROM repro_attempts
		WHERE state IN ('pending', 'infra_retry')
		  AND attempt_count < max_attempts
		ORDER BY updated_at ASC, id ASC`,
		nil,
		func(r *sql.Rows) (ReproAttempt, error) {
			var ra ReproAttempt
			var state, createdAt, updatedAt string
			if err := r.Scan(&ra.ID, &ra.Fingerprint, &state, &ra.AttemptCount, &ra.MaxAttempts, &ra.LastError, &createdAt, &updatedAt); err != nil {
				return ReproAttempt{}, err
			}
			ra.State = ReproState(state)
			var perr error
			if ra.CreatedAt, perr = parseTime(createdAt); perr != nil {
				return ReproAttempt{}, perr
			}
			if ra.UpdatedAt, perr = parseTime(updatedAt); perr != nil {
				return ReproAttempt{}, perr
			}
			return ra, nil
		},
	)
}
