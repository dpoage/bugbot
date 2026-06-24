package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// This file is the single execution path every store method routes through.
// Centralizing it gives the package one definition of three things that used
// to be copy-pasted (and occasionally drift) at every call site:
//
//   - error annotation: every failure carries the db path, the op label, and
//     the SQLite result code (see annotateErr);
//   - transient retry: SQLITE_BUSY and the IOERR_SHORT_READ checkpoint-vs-read
//     race clear on their own within milliseconds, so the handler retries them
//     a bounded number of times instead of surfacing a spurious failure;
//   - a chokepoint for future cross-cutting concerns (metrics, tracing).
//
// Writers are serialized across processes by the Open write lock and within a
// process by MaxOpenConns(1); the residual transient class is a reader in one
// process racing a writer's WAL checkpoint in another, which is exactly what
// retry absorbs.

// maxOpAttempts bounds retries of a transient-failed operation. Five attempts
// with linear backoff spans well under the 5s busy_timeout, so a genuinely
// stuck lock still surfaces as an error rather than spinning forever.
const maxOpAttempts = 5

// retryBackoff is the base wait between transient retries; it grows linearly
// with the attempt index (2ms, 4ms, 6ms, 8ms).
const retryBackoff = 2 * time.Millisecond

// isTransient reports whether err is a SQLite class that typically clears on a
// retry: SQLITE_BUSY (5), a lock another connection holds momentarily, or
// SQLITE_IOERR_SHORT_READ (522), the WAL checkpoint-vs-read race. CORRUPT (11)
// and every other class is NOT transient — retrying cannot help and would only
// delay the operator's signal, so those are surfaced on the first try.
func isTransient(err error) bool {
	switch SQLiteCode(err) {
	case sqliteBUSY, sqliteIOERRShortRead:
		return true
	default:
		return false
	}
}

// retry runs op until it returns a non-transient result or the attempt budget
// is exhausted, honoring ctx between attempts. It is the single place the
// store decides what "try again" means, so reads, writes, and transactions all
// share one definition of a transient failure. Retrying is safe for every
// caller because a transiently-failed statement or transaction committed
// nothing (BUSY/SHORT_READ fail before commit; withTx's deferred Rollback
// unwinds a partial transaction), so a clean re-run cannot double-apply.
func (s *Store) retry(ctx context.Context, op func() error) error {
	var err error
	for attempt := 0; attempt < maxOpAttempts; attempt++ {
		if err = op(); err == nil || !isTransient(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retryBackoff * time.Duration(attempt+1)):
		}
	}
	return err
}

// exec runs a non-query statement through the handler: transient retry plus
// annotation. op is a short snake_case label that names the call in error
// messages and operator logs (e.g. "record_spend").
func (s *Store) exec(ctx context.Context, op, query string, args ...any) (sql.Result, error) {
	var res sql.Result
	err := s.retry(ctx, func() error {
		var e error
		res, e = s.db.ExecContext(ctx, query, args...)
		return e
	})
	if err != nil {
		return nil, annotateErr(s.path, op, err)
	}
	return res, nil
}

// query runs a SELECT and returns the raw *sql.Rows for callers that drain into
// something other than a flat slice (e.g. a map). Only the QueryContext call is
// retried; a transient failure surfacing mid-iteration is not, so this is the
// right helper only for reads that run inside the writer process (where
// MaxOpenConns(1) precludes the checkpoint race). Slice and single-row reads
// should use queryRows / queryRow, which retry the whole fetch. The caller owns
// rows.Close().
func (s *Store) query(ctx context.Context, op, query string, args ...any) (*sql.Rows, error) {
	var rows *sql.Rows
	err := s.retry(ctx, func() error {
		var e error
		rows, e = s.db.QueryContext(ctx, query, args...)
		return e
	})
	if err != nil {
		return nil, annotateErr(s.path, op, err)
	}
	return rows, nil
}

// queryRow runs a single-row SELECT and applies scan to the row, retrying the
// whole read on a transient failure. sql.ErrNoRows is returned unwrapped (it is
// not transient and callers branch on it directly); any other error is
// annotated.
func (s *Store) queryRow(ctx context.Context, op, query string, args []any, scan func(*sql.Row) error) error {
	err := s.retry(ctx, func() error {
		return scan(s.db.QueryRowContext(ctx, query, args...))
	})
	if err == nil || errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return annotateErr(s.path, op, err)
}

// queryRows runs a SELECT and drains it through scanFn, retrying the ENTIRE
// fetch on a transient failure. Retrying the whole read (not just the
// QueryContext call) is essential: the IOERR_SHORT_READ checkpoint race usually
// surfaces while iterating rows, after QueryContext has already returned.
// Because a failed read commits nothing and out is reassigned on every attempt,
// re-running is safe and cannot accumulate partial results.
//
// It is a free function rather than a method because Go methods cannot take
// type parameters; it reuses the package's scanRows drain so every list
// accessor shares the same rows.Err() tail-check.
func queryRows[T any](ctx context.Context, s *Store, op, query string, args []any, scanFn func(*sql.Rows) (T, error)) ([]T, error) {
	var out []T
	err := s.retry(ctx, func() error {
		rows, e := s.db.QueryContext(ctx, query, args...)
		if e != nil {
			return e
		}
		defer func() { _ = rows.Close() }()
		out, e = scanRows(rows, scanFn)
		return e
	})
	if err != nil {
		return nil, annotateErr(s.path, op, err)
	}
	return out, nil
}
