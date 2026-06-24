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
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" database/sql driver
)

// timeLayout is the canonical on-disk timestamp format: RFC3339 with nanosecond
// precision, in UTC. Stored as TEXT so values sort lexicographically.
const timeLayout = time.RFC3339Nano

// nowUTC is the time source, indirected so tests can pin it if needed.
var nowUTC = func() time.Time { return time.Now().UTC() }

// Store is a handle to the embedded state database. It is safe for concurrent
// use by multiple goroutines, but the underlying *sql.DB is configured with
// MaxOpenConns(1) by Open, so callers never observe multiple in-flight
// statements — see Open for the rationale.
type Store struct {
	db   *sql.DB
	path string
	lock *dbLock // cross-process writer lock; nil for OpenReadOnly and ":memory:"
}

// Open opens (creating if necessary) the SQLite database at path, creates any
// missing parent directories, applies pragmas (WAL, foreign keys, busy
// timeout), acquires the cross-process writer lock, and runs all pending
// migrations. Calling Open again on an already-migrated database is a no-op for
// the schema, so it is safe to call on every process start.
//
// Open takes an exclusive advisory write lock on "<path>.lock" and returns
// *ErrLocked if another process already holds it: at most one writer per state
// db. This is what prevents the concurrent-writer page corruption that
// MaxOpenConns(1) alone could not (the conn bound only serializes writers
// within one process). Read-only consumers that must coexist with a running
// writer use OpenReadOnly instead.
//
// The special path ":memory:" opens a private in-memory database (no lock,
// since it is unshareable), useful for tests.
func Open(ctx context.Context, path string) (*Store, error) {
	return open(ctx, path, true)
}

// OpenReadOnly opens the store WITHOUT acquiring the writer lock, so it can run
// concurrently with a writer in another process (WAL permits one writer and
// many readers at once). Use it for read-only commands — report, leads,
// metrics, export, status — and for internal diagnostics that reopen a live db
// (see Diagnose). When the database is absent it creates and migrates it
// (preserving the historical behavior that a read command against a
// never-scanned repo reports empty rather than erroring); when it already
// exists it opens WITHOUT running migrations, so a read-only open never issues
// schema DDL outside the writer lock (the writer owns the schema).
func OpenReadOnly(ctx context.Context, path string) (*Store, error) {
	return open(ctx, path, false)
}

// open is the shared constructor. writeLock selects whether the cross-process
// exclusive writer lock is acquired (Open) or skipped (OpenReadOnly).
func open(ctx context.Context, path string, writeLock bool) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("store: empty database path")
	}

	// '?' and '#' in a bare (non-file:-scheme) DSN inject into or truncate the
	// pragma query string, silently overriding WAL mode, foreign keys, or
	// synchronous level. Percent-encoding was considered and rejected: the
	// driver's unescaping behaviour for bare DSNs is not contractual, so encoding
	// could silently open a differently-named file — worse than refusing. No
	// legitimate database path contains these characters; this is operator config
	// where a loud early error is the correct UX. ":memory:" contains neither,
	// so it is unaffected.
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		return nil, fmt.Errorf("store: database path %q must not contain '?' or '#' (these would corrupt the SQLite connection string and its pragmas)", path)
	}

	if path != ":memory:" {
		if dir := filepath.Dir(path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("store: create db dir %q: %w", dir, err)
			}
		}
	}

	// Whether the database file already exists, checked BEFORE sql.Open (which
	// would create it). A read-only open of an existing db skips migrate() so it
	// never issues DDL outside the writer lock; only the creating path migrates.
	existed := false
	if path != ":memory:" {
		if _, statErr := os.Stat(path); statErr == nil {
			existed = true
		}
	}

	// Acquire the writer lock before opening the handle so two racing writers
	// cannot both reach migrate() and interleave schema writes. ":memory:" is
	// per-handle and unshareable, so it never contends.
	var lock *dbLock
	if writeLock && path != ":memory:" {
		l, err := acquireWriteLock(path)
		if err != nil {
			return nil, err // *ErrLocked, or a real lock-file IO error
		}
		lock = l
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
		_ = lock.release()
		return nil, annotateErr(path, "open", fmt.Errorf("sql.Open: %w", err))
	}

	// Bound the writer pool to 1. Bugbot's state DB is a low-throughput
	// control-plane store (spend ledger, findings, scan-run history) and the
	// driver is pure-Go, which historically has surfaced WAL checkpoint-vs-
	// read races as SQLITE_IOERR_SHORT_READ (522) under concurrent writers
	// (see bugbot-dj7). Serializing the single connection removes the
	// concurrency variable entirely: every read and write goes through the
	// same connection, so a checkpoint cannot be in flight while a read is
	// observing the post-checkpoint file. Read latency is dominated by
	// total token spend, not by row count, so a 1-connection pool does not
	// regress interactive scan wall time.
	db.SetMaxOpenConns(1)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		_ = lock.release()
		return nil, annotateErr(path, "ping", err)
	}

	// Writers always reconcile the schema; a read-only open migrates only when
	// it is the one creating the database (see existed above).
	if writeLock || !existed {
		if err := migrate(ctx, db); err != nil {
			_ = db.Close()
			_ = lock.release()
			return nil, annotateErr(path, "migrate", err)
		}
	}

	return &Store{db: db, path: path, lock: lock}, nil
}

// MaxOpenConnections returns the configured ceiling on the writer pool. It
// is exposed primarily for tests that assert the bound; production callers
// should treat the pool as opaque.
func (s *Store) MaxOpenConnections() int {
	return s.db.Stats().MaxOpenConnections
}

// Path returns the on-disk path of the underlying database. Used by error
// annotations and by tests; production code rarely needs it.
func (s *Store) Path() string { return s.path }

// Diagnose runs a best-effort triage of an IOERR-class error so the next
// operator log makes the failure mode self-explaining. It returns nil when
// the database passes PRAGMA quick_check AND a separately-opened
// short-lived connection also passes quick_check; otherwise it returns the
// first error encountered with the operation labelled. The caller is
// expected to log every step (a tiny helper in this package formats the
// lines), so an operator can tell at a glance whether the IOERR was a
// transient checkpoint-vs-read race (both quick_checks pass) or a sign
// of on-disk corruption (one or both fail).
//
// Diagnose does NOT mutate the receiver's *sql.DB. It runs quick_check
// on the existing handle (which goes through the same connection the
// caller has been using — its result reflects the in-flight state), and
// then opens a SEPARATE short-lived *sql.DB to the same path. The
// second quick_check catches damage that affects only the original
// connection's view (e.g. a checkpoint wrote bad pages). Neither step
// closes or rebinds s.db, so concurrent callers holding this *Store
// pointer continue to work without interruption.
func (s *Store) Diagnose(ctx context.Context) error {
	if s == nil || s.db == nil {
		return annotateErr(s.path, "diagnose", fmt.Errorf("store not open"))
	}
	// Step 1: quick_check on the existing connection. Goes through
	// the same *sql.DB the caller is using; if the page cache holds
	// a stale view this is what we see, which is exactly the case
	// we want to surface.
	var firstCheck string
	if err := s.db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&firstCheck); err != nil {
		return annotateErr(s.path, "diagnose.quick_check", err)
	}
	if firstCheck != "ok" {
		return annotateErr(s.path, "diagnose.quick_check",
			fmt.Errorf("quick_check on existing connection returned %q — db is likely corrupted", firstCheck))
	}
	// Step 2: open a SEPARATE short-lived connection to the same
	// path and quick_check it. This catches damage that affects a
	// fresh open (permissions, vanished file, on-disk corruption
	// observed by the OS but not yet by the live connection). The
	// handle is closed before returning so we do not leak
	// connections.
	second, err := OpenReadOnly(ctx, s.path)
	if err != nil {
		return annotateErr(s.path, "diagnose.reopen", err)
	}
	defer func() { _ = second.Close() }()
	var secondCheck string
	if err := second.db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&secondCheck); err != nil {
		return annotateErr(s.path, "diagnose.reopen_quick_check", err)
	}
	if secondCheck != "ok" {
		return annotateErr(s.path, "diagnose.reopen_quick_check",
			fmt.Errorf("reopen quick_check returned %q — db is likely corrupted on disk", secondCheck))
	}
	return nil
}

// DB exposes the underlying *sql.DB for callers that need direct access (e.g.
// advanced reporting queries). Most callers should prefer the typed methods.
func (s *Store) DB() *sql.DB { return s.db }

// Close releases the database handle and the cross-process writer lock (if
// held). The lock is also released by the kernel on process exit, so a missed
// Close cannot strand it.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	err := s.db.Close()
	if s.lock != nil {
		if lerr := s.lock.release(); lerr != nil && err == nil {
			err = lerr
		}
		s.lock = nil
	}
	return err
}

// parseTime parses an on-disk timestamp string.
func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(timeLayout, s)
}
