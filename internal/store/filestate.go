package store

import (
	"context"
	"database/sql"
	"time"
)

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
	defer stmt.Close()

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
	defer rows.Close()

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
