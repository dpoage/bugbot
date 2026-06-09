// Package store implements Bugbot's embedded, durable state: findings (anchored
// to a commit and file-content hash), suppression memory, incremental-scan
// watermarks, scan-run history, and a token-spend ledger.
//
// It is backed by an embedded SQLite database via modernc.org/sqlite, a pure-Go
// driver (no CGO) registered under the database/sql name "sqlite". A single
// *sql.DB is shared by all methods; modernc.org/sqlite serializes writes
// internally, and WAL mode plus a busy timeout let concurrent readers proceed.
//
// # Findings are anchored to a code version
//
// Every finding records the commit_sha and file_hash of the code it was found
// in. This lets the daemon implement re-verification: after pulling a new
// commit, it loads still-open findings whose file changed (the file's current
// content hash differs from the finding's file_hash) and re-runs verification
// against the new code. Findings whose code is unchanged need not be re-checked.
// Callers can scope queries to a commit with ListFindings + FindingFilter.
//
// # Suppression semantics
//
// Dismissing a finding (UpdateStatus to StatusDismissed, or AddSuppression)
// both flips the finding's status and records the fingerprint in the
// suppressions table. UpsertFinding consults that table: a fingerprint that has
// ever been suppressed is re-inserted/updated as StatusDismissed, never
// StatusOpen. A dismissed bug therefore never resurfaces as open, even if a
// finder re-discovers it on a later scan. This is the "suppression memory"
// described in ARCHITECTURE.md.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" database/sql driver
)

// timeLayout is the canonical on-disk timestamp format: RFC3339 with nanosecond
// precision, in UTC. Stored as TEXT so values sort lexicographically.
const timeLayout = time.RFC3339Nano

// nowUTC is the time source, indirected so tests can pin it if needed.
var nowUTC = func() time.Time { return time.Now().UTC() }

// Store is a handle to the embedded state database. It is safe for concurrent
// use by multiple goroutines.
type Store struct {
	db *sql.DB
}

// Open opens (creating if necessary) the SQLite database at path, creates any
// missing parent directories, applies pragmas (WAL, foreign keys, busy
// timeout), and runs all pending migrations. Calling Open again on an
// already-migrated database is a no-op for the schema, so it is safe to call on
// every process start.
//
// The special path ":memory:" opens a private in-memory database, useful for
// tests.
func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("store: empty database path")
	}

	if path != ":memory:" {
		if dir := filepath.Dir(path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("store: create db dir %q: %w", dir, err)
			}
		}
	}

	// Pragmas are passed as DSN query params so they apply to every pooled
	// connection. _txlock=immediate makes write transactions take the write
	// lock up front, avoiding mid-transaction SQLITE_BUSY upgrade failures.
	dsn := path + "?_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(ON)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_txlock=immediate"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %q: %w", path, err)
	}

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: ping %q: %w", path, err)
	}

	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}

	return &Store{db: db}, nil
}

// DB exposes the underlying *sql.DB for callers that need direct access (e.g.
// advanced reporting queries). Most callers should prefer the typed methods.
func (s *Store) DB() *sql.DB { return s.db }

// Close releases the database handle.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// parseTime parses an on-disk timestamp string.
func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(timeLayout, s)
}
