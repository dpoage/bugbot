package store

import (
	"context"
	"database/sql"
)

// ReproContradictionThreshold is the number of exit-zero (bug-did-not-manifest)
// repro attempts required to set the repro-contradicted signal. Two independent
// attempts, typically across code revisions of the same fingerprint, is the
// minimum meaningful disconfirmation: a single pass could be a transient
// environment issue; two passes suggest the bug is not reliably reproducible.
const ReproContradictionThreshold = 2

// RecordExitZeroAttempt increments the exit_zero_count on the repro_attempts
// row for the given fingerprint. It is called by promote.go's promoteOne
// exactly when the sandbox ran without infrastructure error but exited 0 (the
// test ran, the bug did NOT manifest). Counts accumulate across code revisions
// of the same fingerprint (independent attempts, typically on different file
// versions), so exit_zero_count reflects total disconfirming evidence over the
// lifetime of the finding. It is a no-op when no row exists for the fingerprint
// (the caller already ensures the row exists via EnqueueRepro).
//
// Outcome classification:
//   - exit_zero (ran, no manifest) → this call; counts toward contradiction.
//   - infra error (sandbox/agent crash) → RequeueReproAttemptOnInfraError; does NOT count.
//   - reproduced (Promoted=true) → ZeroExitZeroCount clears prior contradiction; does NOT count.
func (s *Store) RecordExitZeroAttempt(ctx context.Context, fingerprint string) error {
	return s.retry(ctx, func() error {
		_, err := s.db.ExecContext(ctx, `
			UPDATE repro_attempts
			SET exit_zero_count = exit_zero_count + 1,
			    updated_at      = ?
			WHERE fingerprint = ?`,
			nowUTC().Format(timeLayout), fingerprint,
		)
		if err != nil {
			return annotateErr(s.path, "record_exit_zero_attempt", err)
		}
		return nil
	})
}

// IsReproContradicted reports whether the finding identified by fingerprint has
// accumulated at least ReproContradictionThreshold exit-zero attempts, meaning
// independent attempts (across code revisions) did not see the bug manifest.
// Returns false when no repro_attempts row exists for the fingerprint.
func (s *Store) IsReproContradicted(ctx context.Context, fingerprint string) (bool, error) {
	var count int
	err := s.queryRow(ctx, "is_repro_contradicted", `
		SELECT exit_zero_count FROM repro_attempts WHERE fingerprint = ?`,
		[]any{fingerprint},
		func(row *sql.Row) error {
			return row.Scan(&count)
		},
	)
	if err == ErrNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return count >= ReproContradictionThreshold, nil
}

// ReproContradictedFingerprints returns the fingerprints of all findings that
// have accumulated at least ReproContradictionThreshold exit-zero repro
// attempts. This is the accessor for re-verify prioritization: the caller can
// use the returned set to deprioritize or flag these findings for re-examination.
func (s *Store) ReproContradictedFingerprints(ctx context.Context) ([]string, error) {
	return queryRows(ctx, s, "repro_contradicted_fingerprints", `
		SELECT fingerprint FROM repro_attempts
		WHERE exit_zero_count >= ?
		ORDER BY exit_zero_count DESC, updated_at ASC`,
		[]any{ReproContradictionThreshold},
		func(r *sql.Rows) (string, error) {
			var fp string
			return fp, r.Scan(&fp)
		},
	)
}

// ZeroExitZeroCount resets the exit_zero_count to 0 for the given fingerprint.
// It is called by promote.go's promoteOne when a repro attempt SUCCEEDS
// (att.Promoted=true): a successful reproduction is definitive positive evidence
// that supersedes any prior exit-zero disconfirmation. Zeroing prevents the
// incoherent state where a finding is simultaneously Tier<=1 (reproduced) and
// repro-contradicted. It is a no-op when no repro_attempts row exists.
func (s *Store) ZeroExitZeroCount(ctx context.Context, fingerprint string) error {
	return s.retry(ctx, func() error {
		_, err := s.db.ExecContext(ctx, `
			UPDATE repro_attempts
			SET exit_zero_count = 0,
			    updated_at      = ?
			WHERE fingerprint = ?`,
			nowUTC().Format(timeLayout), fingerprint,
		)
		if err != nil {
			return annotateErr(s.path, "zero_exit_zero_count", err)
		}
		return nil
	})
}
