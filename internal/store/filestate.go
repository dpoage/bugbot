package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// epochSentinel is the RFC3339 NOT NULL placeholder written by
// RefreshContentHashes for rows that have never been scanned. It is
// intentionally at the Unix epoch (1970-01-01T00:00:00Z) so it sorts before
// any real scan timestamp and applySweepOrder can identify "never scanned"
// rows as group 1 without a separate NULL check.
const epochSentinel = "1970-01-01T00:00:00Z"

// epochSentinelTime is the parsed form of epochSentinel.
var epochSentinelTime time.Time

func init() {
	var err error
	epochSentinelTime, err = time.Parse(time.RFC3339, epochSentinel)
	if err != nil {
		panic("store: failed to parse epoch sentinel: " + err.Error())
	}
}

// EpochSentinelTime returns the parsed epoch sentinel timestamp used by
// RefreshContentHashes for never-scanned rows. Exported so the funnel package
// can identify "never scanned" entries in LastScannedAt results.
func EpochSentinelTime() time.Time { return epochSentinelTime }

// sqliteMaxVars is the maximum number of host parameters in a single SQLite
// statement (SQLITE_MAX_VARIABLE_NUMBER). IN-clauses must be chunked below
// this limit to avoid "too many SQL variables" errors.
const sqliteMaxVars = 999

// FileState is the scan watermark for a single file: the content hash and the
// commit at which it was last scanned. Incremental scanning compares a file's
// current hash against this to decide whether it needs re-scanning.
type FileState struct {
	Path              string
	ContentHash       string
	LastScannedCommit string
	LastScannedAt     time.Time
}

// GetFileState returns the watermark for path, or ErrNotFound.
func (s *Store) GetFileState(ctx context.Context, path string) (FileState, error) {
	var fs FileState
	var scannedAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT path, content_hash, last_scanned_commit, last_scanned_at
		 FROM file_state WHERE path = ?`, path,
	).Scan(&fs.Path, &fs.ContentHash, &fs.LastScannedCommit, &scannedAt)
	if err == sql.ErrNoRows {
		return FileState{}, ErrNotFound
	}
	if err != nil {
		return FileState{}, err
	}
	if fs.LastScannedAt, err = parseTime(scannedAt); err != nil {
		return FileState{}, err
	}
	return fs, nil
}

// UpsertFileStates writes a batch of watermarks in a single transaction,
// inserting new rows and overwriting existing ones by path. LastScannedAt is
// set to now for every row so a zero time from the caller is filled in.
func (s *Store) UpsertFileStates(ctx context.Context, states []FileState) error {
	if len(states) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO file_state (path, content_hash, last_scanned_commit, last_scanned_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
		  content_hash = excluded.content_hash,
		  last_scanned_commit = excluded.last_scanned_commit,
		  last_scanned_at = excluded.last_scanned_at`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()

	now := nowUTC().Format(timeLayout)
	for _, st := range states {
		scannedAt := now
		if !st.LastScannedAt.IsZero() {
			scannedAt = st.LastScannedAt.UTC().Format(timeLayout)
		}
		if _, err := stmt.ExecContext(ctx,
			st.Path, st.ContentHash, st.LastScannedCommit, scannedAt,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RefreshContentHashes upserts a batch of watermarks, updating content_hash and
// last_scanned_commit but deliberately PRESERVING any existing last_scanned_at.
// New rows get the epoch sentinel for last_scanned_at so the anti-starvation
// sweep ordering can identify them as "never scanned" (group 1). Existing rows
// keep their truthful last_scanned_at so a subsequent sweep can correctly
// measure how stale they are.
//
// This is the daemon's refreshWatermarks path. It is distinct from
// UpsertFileStates (which overwrites last_scanned_at) to ensure that the
// watermark refresh after a sweep never clobbers a truthful scan timestamp
// written by TouchScanCoverage.
func (s *Store) RefreshContentHashes(ctx context.Context, states []FileState) error {
	if len(states) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO file_state (path, content_hash, last_scanned_commit, last_scanned_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
		  content_hash = excluded.content_hash,
		  last_scanned_commit = excluded.last_scanned_commit`)
	// Note: last_scanned_at is intentionally absent from the conflict UPDATE so
	// existing rows keep their truthful scan timestamp.
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()

	for _, st := range states {
		if _, err := stmt.ExecContext(ctx,
			st.Path, st.ContentHash, st.LastScannedCommit, epochSentinel,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// TouchScanCoverage records that the given files were actually covered by a
// completed scan run. It upserts file_state rows with last_scanned_at = now
// and last_scanned_commit = commit, inserting new rows (with a zero content
// hash) if the path does not yet exist. Only call this for files whose finder
// unit completed with finderOK status.
//
// paths must not be empty. Content hash is intentionally left empty for
// insert-only rows — RefreshContentHashes will fill it in on the next sweep.
func (s *Store) TouchScanCoverage(ctx context.Context, paths []string, commit string) error {
	if len(paths) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO file_state (path, content_hash, last_scanned_commit, last_scanned_at)
		VALUES (?, '', ?, ?)
		ON CONFLICT(path) DO UPDATE SET
		  last_scanned_commit = excluded.last_scanned_commit,
		  last_scanned_at = excluded.last_scanned_at`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()

	now := nowUTC().Format(timeLayout)
	for _, p := range paths {
		if _, err := stmt.ExecContext(ctx, p, commit, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LastScannedAt returns a map of path → last_scanned_at for every path in
// paths that has a row in file_state. Paths not in the store are absent from
// the result (callers treat absence as "never scanned"). The query is chunked
// to stay under SQLite's sqliteMaxVars host-parameter limit.
func (s *Store) LastScannedAt(ctx context.Context, paths []string) (map[string]time.Time, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	result := make(map[string]time.Time, len(paths))
	for _, chunk := range chunkStrings(paths, sqliteMaxVars) {
		if err := s.lastScannedAtChunk(ctx, chunk, result); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (s *Store) lastScannedAtChunk(ctx context.Context, paths []string, out map[string]time.Time) error {
	placeholders := strings.Repeat("?,", len(paths))
	placeholders = placeholders[:len(placeholders)-1] // trim trailing comma
	q := `SELECT path, last_scanned_at FROM file_state WHERE path IN (` + placeholders + `)`
	args := make([]interface{}, len(paths))
	for i, p := range paths {
		args[i] = p
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var p, ts string
		if err := rows.Scan(&p, &ts); err != nil {
			return err
		}
		t, err := parseTime(ts)
		if err != nil {
			return err
		}
		out[p] = t
	}
	return rows.Err()
}

// chunkStrings splits s into slices of at most size elements.
func chunkStrings(s []string, size int) [][]string {
	if size <= 0 || len(s) <= size {
		if len(s) == 0 {
			return nil
		}
		return [][]string{s}
	}
	var out [][]string
	for i := 0; i < len(s); i += size {
		end := i + size
		if end > len(s) {
			end = len(s)
		}
		out = append(out, s[i:end])
	}
	return out
}

// ChangedSince compares the supplied map of current path→content-hash against
// the stored watermarks and returns the paths whose content has changed or that
// have never been scanned. A path present in the store but absent from current
// is treated as deleted and is NOT returned (the caller already knows which
// files exist); ChangedSince answers "which of the files I have now need
// (re)scanning?".
//
// This is the core of incremental scanning: only changed files are fed to the
// finder agents on each cycle.
func (s *Store) ChangedSince(ctx context.Context, current map[string]string) ([]string, error) {
	if len(current) == 0 {
		return nil, nil
	}

	rows, err := s.db.QueryContext(ctx, `SELECT path, content_hash FROM file_state`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	stored := make(map[string]string)
	for rows.Next() {
		var p, h string
		if err := rows.Scan(&p, &h); err != nil {
			return nil, err
		}
		stored[p] = h
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var changed []string
	for p, h := range current {
		if old, ok := stored[p]; !ok || old != h {
			changed = append(changed, p)
		}
	}
	return changed, nil
}
