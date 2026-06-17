package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Lead is a cross-lens tip posted by one finder agent for another. Its
// persistent row on the leads blackboard lets a suspicion from one lens survive
// to a future run of the target lens, without any direct communication between
// agent goroutines.
//
// Every surviving row is pending: consumed leads are deleted from the
// blackboard at claim time, so a row that still exists has not yet been picked
// up by its target lens.
//
// The UNIQUE(target_lens, file, line) constraint is the dedup key: re-posting
// the same target/file/line upserts (note + poster refreshed, created_at
// preserved). A finder that died after consuming its leads loses that claim;
// the next cycle will re-post as a fresh INSERT (the old row is gone).
type Lead struct {
	ID         string
	ScanRunID  string
	PosterLens string
	TargetLens string
	File       string
	Line       int
	Note       string
	CreatedAt  time.Time
}

// AddLead upserts a lead. On conflict on (target_lens, file, line) it refreshes
// the note, poster_lens, and scan_run_id. The original created_at is always
// preserved. Because consumed leads are deleted, a conflict can only hit a row
// that is still pending — there is no consumed->posted flip to perform.
func (s *Store) AddLead(ctx context.Context, l Lead) error {
	if l.ID == "" {
		l.ID = newID()
	}
	if l.CreatedAt.IsZero() {
		l.CreatedAt = nowUTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO leads (id, scan_run_id, poster_lens, target_lens, file, line, note, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'posted', ?)
		ON CONFLICT(target_lens, file, line) DO UPDATE SET
			note        = excluded.note,
			poster_lens = excluded.poster_lens,
			scan_run_id = excluded.scan_run_id`,
		l.ID, l.ScanRunID, l.PosterLens, l.TargetLens, l.File, l.Line, l.Note,
		l.CreatedAt.Format(timeLayout),
	)
	return err
}

// PendingLeads returns all pending leads for the given target lens, ordered
// by created_at then id for determinism.
func (s *Store) PendingLeads(ctx context.Context, targetLens string) ([]Lead, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, scan_run_id, poster_lens, target_lens, file, line, note, created_at
		FROM leads
		WHERE target_lens = ?
		ORDER BY created_at, id`,
		targetLens,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []Lead
	for rows.Next() {
		l, err := scanLead(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ConsumeLeads deletes the given lead IDs in a single transaction. Once a
// finder has claimed a lead at the start of its run, the row is removed from
// the blackboard; a re-post of the same (target_lens, file, line) later is a
// fresh INSERT rather than a flip back to 'posted'.
func (s *Store) ConsumeLeads(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	placeholders := strings.Repeat(",?", len(ids))[1:] // "?,?,?" for len=3
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM leads WHERE id IN (%s)`, placeholders),
		args...,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// scanLead reads one lead row from a *sql.Rows cursor.
func scanLead(rows *sql.Rows) (Lead, error) {
	var l Lead
	var created string
	if err := rows.Scan(
		&l.ID, &l.ScanRunID, &l.PosterLens, &l.TargetLens,
		&l.File, &l.Line, &l.Note, &created,
	); err != nil {
		return Lead{}, err
	}
	var err error
	if l.CreatedAt, err = parseTime(created); err != nil {
		return Lead{}, err
	}
	return l, nil
}

// ListLeads returns the blackboard, newest-first (created_at DESC, id DESC for
// determinism). Every row in the table is pending, so no filter is needed.
func (s *Store) ListLeads(ctx context.Context) ([]Lead, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, scan_run_id, poster_lens, target_lens, file, line, note, created_at
		FROM leads
		ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []Lead
	for rows.Next() {
		l, err := scanLead(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
