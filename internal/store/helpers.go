package store

import (
	"context"
	"database/sql"
	"strings"
)

// withTx runs fn inside a transaction on the store's database, committing on
// nil return and rolling back on any error. The deferred Rollback is a no-op
// after a successful Commit, so callers don't need to track success state.
//
// The helper centralizes the BeginTx / defer-Rollback / Commit dance used by
// every mutating Store method. Without it, each call site had to repeat the
// four-line incantation and any subtle drift (forgetting the defer, calling
// Commit after a partial failure, etc.) would silently leak transactions or
// commit a half-applied state. One place to audit, one place to change.
//
// The whole transaction runs through the shared retry handler, so a transient
// failure (SQLITE_BUSY or the IOERR_SHORT_READ checkpoint race) re-runs fn in a
// fresh transaction rather than surfacing a spurious error. This is safe
// because the failed attempt's deferred Rollback unwound any partial work
// before the retry begins — fn must therefore be free of side effects outside
// the transaction, which every store method already is.
func (s *Store) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
	return s.retry(ctx, func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := fn(tx); err != nil {
			return err
		}
		return tx.Commit()
	})
}

// buildPlaceholders returns a comma-separated string of n '?' placeholders,
// suitable for inlining into an IN (?, ?, ...) clause. Centralizing this kills
// three slightly-different inline implementations (leads.go's
// strings.Repeat(",?", n)[1:], filestate.go's and cartographer.go's
// strings.Repeat("?,", n) + trailing-comma trim) that all computed the same
// string for the same purpose. One place to change, one place to read.
func buildPlaceholders(n int) string {
	return strings.Repeat("?,", n)[:n*2-1]
}

// scanRows drains rows through scanFn, returning the collected slice. It is
// the canonical "for rows.Next() { scan }" loop shared by every list/query
// accessor in the package. Centralizing the loop gives every call site the
// same rows.Err() tail-check (the original duplication had a few methods that
// forgot it, silently masking driver-level iteration errors) and lets the
// signature express the accumulator intent generically.
//
// On any non-nil error the partially-built slice is discarded: a row that
// could not be scanned is a hard failure for the whole list, not "return
// what we have so far". Callers that want partial results can short-circuit
// before calling scanRows.
func scanRows[T any](rows *sql.Rows, scanFn func(*sql.Rows) (T, error)) ([]T, error) {
	var out []T
	for rows.Next() {
		v, err := scanFn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
