package store

import (
	"context"
	"database/sql"
	"time"
)

// epochSentinel is the RFC3339 NOT NULL placeholder written by
// RefreshContentHashes for rows that have never been scanned. It is
// intentionally at the Unix epoch (1970-01-01T00:00:00Z) so it sorts before
// any real scan timestamp and applySweepOrder can identify "never scanned"
// rows as group 1 without a separate NULL check. This literal happens to be
// exactly what nowUTC().Format(timeLayout) would produce for the epoch
// (RFC3339Nano drops a zero fractional part), so the column holds a single
// consistent format.
//
// CAUTION for future queries: RFC3339Nano values do NOT sort consistently as
// raw strings across the second boundary ("...:17Z" > "...:17.123Z" because
// 'Z' > '.'). All ordering on last_scanned_at must parse and compare
// time.Time (as applySweepOrder does) — never SQL ORDER BY on the raw column.
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
		return FileState{}, annotateErr(s.path, "get_file_state", err)
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
	return s.withTx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `
			INSERT INTO file_state (path, content_hash, last_scanned_commit, last_scanned_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(path) DO UPDATE SET
			  content_hash = excluded.content_hash,
			  last_scanned_commit = excluded.last_scanned_commit,
			  last_scanned_at = excluded.last_scanned_at`)
		if err != nil {
			return annotateErr(s.path, "upsert_file_states", err)
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
				return annotateErr(s.path, "upsert_file_states", err)
			}
		}
		return nil
	})
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
	return s.withTx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `
			INSERT INTO file_state (path, content_hash, last_scanned_commit, last_scanned_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(path) DO UPDATE SET
			  content_hash = excluded.content_hash,
			  last_scanned_commit = excluded.last_scanned_commit`)
		// Note: last_scanned_at is intentionally absent from the conflict UPDATE so
		// existing rows keep their truthful scan timestamp.
		if err != nil {
			return annotateErr(s.path, "refresh_content_hashes", err)
		}
		defer func() { _ = stmt.Close() }()

		for _, st := range states {
			if _, err := stmt.ExecContext(ctx,
				st.Path, st.ContentHash, st.LastScannedCommit, epochSentinel,
			); err != nil {
				return annotateErr(s.path, "refresh_content_hashes", err)
			}
		}
		return nil
	})
}

// TouchScanCoverage records that the given files were actually covered by a
// completed scan run. It upserts file_state rows with last_scanned_at = now,
// last_scanned_commit = commit, and the file's content hash at coverage time
// (from hashes; missing entries write/keep an empty hash). Only call this for
// files whose finder unit completed with finderOK status.
//
// Recording the hash here — not just in the daemon's RefreshContentHashes —
// matters: CLI-only sweeps (`bugbot scan`) never run the daemon's watermark
// refresh, and the sweep ordering treats a stored-hash mismatch as "changed,
// re-scan first" (group 1). If coverage left the hash empty, every CLI sweep
// would classify previously-covered files as changed forever and the
// anti-starvation rotation would never form a group 2.
//
// An empty hash for a path never clobbers an existing stored hash (the CASE
// below), so a failed fingerprint computation degrades to "treated as
// changed next sweep" rather than corrupting good state.
func (s *Store) TouchScanCoverage(ctx context.Context, paths []string, commit string, hashes map[string]string) error {
	if len(paths) == 0 {
		return nil
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `
			INSERT INTO file_state (path, content_hash, last_scanned_commit, last_scanned_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(path) DO UPDATE SET
			  content_hash = CASE WHEN excluded.content_hash != '' THEN excluded.content_hash ELSE file_state.content_hash END,
			  last_scanned_commit = excluded.last_scanned_commit,
			  last_scanned_at = excluded.last_scanned_at`)
		if err != nil {
			return annotateErr(s.path, "touch_scan_coverage", err)
		}
		defer func() { _ = stmt.Close() }()

		now := nowUTC().Format(timeLayout)
		for _, p := range paths {
			if _, err := stmt.ExecContext(ctx, p, hashes[p], commit, now); err != nil {
				return annotateErr(s.path, "touch_scan_coverage", err)
			}
		}
		return nil
	})
}

// Watermark is the sweep-ordering view of a file_state row: when the file was
// last actually scanned and the content hash recorded at that time. The sweep
// ordering uses the pair to classify files: absent row or epoch timestamp =
// never scanned; stored hash differing from the current fingerprint = changed
// since last scan (both group 1).
type Watermark struct {
	LastScannedAt time.Time
	ContentHash   string
}

// ScanWatermarks returns a map of path → Watermark for every path in paths
// that has a row in file_state. Paths not in the store are absent from the
// result (callers treat absence as "never scanned"). The query is chunked to
// stay under SQLite's sqliteMaxVars host-parameter limit.
func (s *Store) ScanWatermarks(ctx context.Context, paths []string) (map[string]Watermark, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	result := make(map[string]Watermark, len(paths))
	for _, chunk := range chunkStrings(paths, sqliteMaxVars) {
		if err := s.scanWatermarksChunk(ctx, chunk, result); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (s *Store) scanWatermarksChunk(ctx context.Context, paths []string, out map[string]Watermark) error {
	q := `SELECT path, content_hash, last_scanned_at FROM file_state WHERE path IN (` + buildPlaceholders(len(paths)) + `)`
	args := make([]interface{}, len(paths))
	for i, p := range paths {
		args[i] = p
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return annotateErr(s.path, "scan_watermarks", err)
	}
	defer func() { _ = rows.Close() }()
	// Chunk-accumulation into a shared out map: scanRows returns a fresh slice
	// per call and would force the caller to merge slices across chunks.
	// The manual loop stays; annotateErr gives it the same error surface as
	// the rest of the package. Same shape as cartographer.go's
	// getPackageSummariesChunk.
	for rows.Next() {
		var p, hash, ts string
		if err := rows.Scan(&p, &hash, &ts); err != nil {
			return annotateErr(s.path, "scan_watermarks", err)
		}
		t, err := parseTime(ts)
		if err != nil {
			return err
		}
		out[p] = Watermark{LastScannedAt: t, ContentHash: hash}
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
		return nil, annotateErr(s.path, "changed_since", err)
	}
	defer func() { _ = rows.Close() }()
	// Full-table scan (no chunking), so the slice-returning scanRows fits
	// cleanly: read all (path, hash) pairs, then build the lookup map in a
	// single post-pass. This replaces the original manual loop and gives
	// the same rows.Err() tail-check.
	type pathHash struct {
		path string
		hash string
	}
	pairs, err := scanRows(rows, func(r *sql.Rows) (pathHash, error) {
		var p, h string
		if err := r.Scan(&p, &h); err != nil {
			return pathHash{}, err
		}
		return pathHash{path: p, hash: h}, nil
	})
	if err != nil {
		return nil, annotateErr(s.path, "changed_since", err)
	}
	stored := make(map[string]string, len(pairs))
	for _, ph := range pairs {
		stored[ph.path] = ph.hash
	}

	var changed []string
	for p, h := range current {
		if old, ok := stored[p]; !ok || old != h {
			changed = append(changed, p)
		}
	}
	return changed, nil
}
