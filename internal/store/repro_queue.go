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
	// ReproStateBlockedToolchain: the claim-time capability gate (promote.go's
	// promoteOne) found the finding's inferred ecosystem unavailable in the
	// sandbox image's probed CapabilitySet and skipped the claim entirely — no
	// sandbox run happened, so attempt_count is NOT incremented. Retryable:
	// treated as claimable exactly like pending/infra_retry (see
	// ClaimReproAttempt), so the next cycle's capability re-check (fresh probe
	// cache after an image/config change) can promote it straight to running.
	// BlockedEcosystem on the row names which capability was missing.
	ReproStateBlockedToolchain ReproState = "blocked_toolchain"
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
	// BlockedEcosystem is the missing-capability ecosystem name (e.g. "js")
	// when State is ReproStateBlockedToolchain. Empty otherwise.
	BlockedEcosystem string
	// Unsandboxed is true when the attempt that produced this row's current
	// state ran in the fix-C attended escape-hatch mode (workspace copy on
	// the host, no container) rather than the sandbox.
	Unsandboxed bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
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
			  (id, fingerprint, state, attempt_count, max_attempts, last_error, blocked_ecosystem, unsandboxed, created_at, updated_at)
			VALUES (?, ?, ?, 0, ?, '', '', 0, ?, ?)`,
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
	var unsandboxed int
	err := s.retry(ctx, func() error {
		return s.db.QueryRowContext(ctx,
			`SELECT id, fingerprint, state, attempt_count, max_attempts, last_error, blocked_ecosystem, unsandboxed, created_at, updated_at
			 FROM repro_attempts WHERE fingerprint = ?`, fingerprint,
		).Scan(&ra.ID, &ra.Fingerprint, &state, &ra.AttemptCount, &ra.MaxAttempts, &ra.LastError, &ra.BlockedEcosystem, &unsandboxed, &createdAt, &updatedAt)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return ReproAttempt{}, ErrNotFound
	}
	if err != nil {
		return ReproAttempt{}, annotateErr(s.path, "get_repro_attempt", err)
	}
	ra.State = ReproState(state)
	ra.Unsandboxed = unsandboxed != 0
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
// pending/infra_retry/blocked_toolchain → running, incrementing attempt_count.
// It also reclaims crash-stuck 'running' rows whose updated_at is older than
// ReproStaleLeaseDuration, treating them as infra_retry (the process that held
// the lease is presumed dead).
// Returns ErrReproAlreadyClaimed if the row is not claimable (already running with
// a fresh lease, done, or abandoned, or attempt budget exhausted).
//
// blocked_toolchain is claimable here (not just via BlockReproAttemptOnToolchain's
// own re-check) so a row the capability gate blocked on a prior cycle claims
// normally once the caller's own re-check of the (possibly now-fresh) probe
// cache decides to call ClaimReproAttempt instead of re-blocking it — see
// promote.go's promoteOne, which performs that re-check before ever reaching
// here. attempt_count is not incremented while blocked; the claim below is the
// row's first real attempt.
//
// The UPDATE … WHERE state IN ('pending','infra_retry','blocked_toolchain')
// AND attempt_count < max_attempts is the single atomic claim gate: concurrent
// writers are serialized by SQLite's single-connection MaxOpenConns(1)
// constraint, so exactly one wins per fingerprint.
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

		// Normal claim: pending, infra_retry, or blocked_toolchain, within budget.
		res, err := s.db.ExecContext(ctx, `
			UPDATE repro_attempts
			SET state = ?, attempt_count = attempt_count + 1, blocked_ecosystem = '', updated_at = ?
			WHERE fingerprint = ?
			  AND state IN ('pending', 'infra_retry', 'blocked_toolchain')
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

// ReleaseReproAttempt transitions a running repro_attempts row back to pending
// and REFUNDS the attempt the claim consumed (attempt_count - 1, floored at 0).
// It is the interrupt/shutdown counterpart to RequeueReproAttemptOnInfraError:
// an operator interrupt (Ctrl-C, daemon shutdown) is not an infrastructure
// strike against the finding, so it must not consume the bounded retry budget
// — otherwise three interrupted cycles would silently abandon the row and
// every later dispatch would report it "already claimed or exhausted" forever.
// note is stored in last_error for diagnostics. Only the claim holder calls
// this; the state = 'running' guard makes a call on any other state a no-op.
func (s *Store) ReleaseReproAttempt(ctx context.Context, fingerprint, note string) error {
	now := nowUTC()
	return s.retry(ctx, func() error {
		_, err := s.db.ExecContext(ctx, `
			UPDATE repro_attempts
			SET state = ?,
			    attempt_count = MAX(attempt_count - 1, 0),
			    last_error = ?,
			    updated_at = ?
			WHERE fingerprint = ? AND state = 'running'`,
			string(ReproStatePending), note, now.Format(timeLayout), fingerprint,
		)
		if err != nil {
			return annotateErr(s.path, "release_repro_attempt", err)
		}
		return nil
	})
}

// BlockReproAttemptOnToolchain transitions a repro_attempts row to
// blocked_toolchain, recording the missing capability's ecosystem name. It is
// called by promote.go's promoteOne BEFORE ClaimReproAttempt, when the
// finding's inferred ecosystem is unavailable in the sandbox image's probed
// CapabilitySet — no sandbox run happens, so attempt_count is deliberately
// left untouched (this is not an attempt, successful or otherwise).
//
// Only pending/infra_retry/blocked_toolchain rows transition: a row already
// running, done, or abandoned is left alone (best-effort — the gate check in
// promoteOne runs before any claim, so a race here means another dispatch
// path's claim won; that claim's own outcome stands). The caller is
// responsible for calling EnqueueRepro first so the row exists.
func (s *Store) BlockReproAttemptOnToolchain(ctx context.Context, fingerprint, ecosystem string) (ReproAttempt, error) {
	now := nowUTC()
	err := s.retry(ctx, func() error {
		_, err := s.db.ExecContext(ctx, `
			UPDATE repro_attempts
			SET state = ?, blocked_ecosystem = ?, updated_at = ?
			WHERE fingerprint = ?
			  AND state IN ('pending', 'infra_retry', 'blocked_toolchain')`,
			string(ReproStateBlockedToolchain), ecosystem, now.Format(timeLayout), fingerprint,
		)
		return err
	})
	if err != nil {
		return ReproAttempt{}, annotateErr(s.path, "block_repro_attempt_on_toolchain", err)
	}
	return s.GetReproAttempt(ctx, fingerprint)
}

// MarkReproAttemptUnsandboxed sets the unsandboxed flag on a fingerprint's
// repro_attempts row, so a T1 promoted via the fix-C attended escape-hatch
// (workspace copy on the host, no container) is distinguishable from a
// normally-sandboxed promotion. Called by the unsandboxed single-finding CLI
// path once EnqueueRepro has ensured the row exists; safe to call regardless
// of the row's current state (it does not gate on state — the escape hatch is
// not a claim/skip queue participant, only a provenance marker on it).
func (s *Store) MarkReproAttemptUnsandboxed(ctx context.Context, fingerprint string) error {
	now := nowUTC()
	return s.retry(ctx, func() error {
		_, err := s.db.ExecContext(ctx, `
			UPDATE repro_attempts SET unsandboxed = 1, updated_at = ?
			WHERE fingerprint = ?`,
			now.Format(timeLayout), fingerprint,
		)
		if err != nil {
			return annotateErr(s.path, "mark_repro_attempt_unsandboxed", err)
		}
		return nil
	})
}

// BlockedToolchainCounts returns the number of repro_attempts rows currently
// in the blocked_toolchain state, grouped by their recorded missing
// ecosystem. This is a zero-container query over the queue's persisted state
// (no probe or sandbox run) — the seam `bugbot report emit` and the daemon
// use to surface "N findings blocked: image lacks X" without re-running
// anything (bugbot-14g0 acceptance 2).
func (s *Store) BlockedToolchainCounts(ctx context.Context) (map[string]int, error) {
	type row struct {
		Eco   string
		Count int
	}
	rows, err := queryRows(ctx, s, "blocked_toolchain_counts", `
		SELECT blocked_ecosystem, COUNT(*)
		FROM repro_attempts
		WHERE state = 'blocked_toolchain'
		GROUP BY blocked_ecosystem`,
		nil,
		func(r *sql.Rows) (row, error) {
			var out row
			err := r.Scan(&out.Eco, &out.Count)
			return out, err
		},
	)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	counts := make(map[string]int, len(rows))
	for _, r := range rows {
		counts[r.Eco] = r.Count
	}
	return counts, nil
}

// UnclaimableReproFingerprints returns the set of fingerprints whose
// repro_attempts row can never be claimed again: state done or abandoned, or
// attempt budget exhausted (attempt_count >= max_attempts — such a row is
// rejected by ClaimReproAttempt's gate in every state, including a stale
// 'running' lease, which the reclaim UPDATE also refuses at budget).
//
// This is the backlog-selection complement to ErrReproAlreadyClaimed:
// daemon.OpenBacklog excludes these fingerprints up front so completed
// attempts are not re-dispatched every firing only to be skipped at claim
// time. Transiently unclaimable rows (fresh 'running' lease under budget) are
// deliberately NOT included — they become claimable again via stale-lease
// reclaim or release, so the backlog must keep seeing them. blocked_toolchain
// rows under budget are likewise excluded from the set: they re-claim
// normally once the sandbox image gains the missing toolchain.
func (s *Store) UnclaimableReproFingerprints(ctx context.Context) (map[string]struct{}, error) {
	rows, err := queryRows(ctx, s, "unclaimable_repro_fingerprints", `
		SELECT fingerprint
		FROM repro_attempts
		WHERE state IN ('done', 'abandoned')
		   OR attempt_count >= max_attempts`,
		nil,
		func(r *sql.Rows) (string, error) {
			var fp string
			err := r.Scan(&fp)
			return fp, err
		},
	)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	set := make(map[string]struct{}, len(rows))
	for _, fp := range rows {
		set[fp] = struct{}{}
	}
	return set, nil
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
