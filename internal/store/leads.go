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
// The UNIQUE(target_lens, file, line) constraint is the dedup key: re-posting
// the same target/file/line upserts (note + poster refreshed, created_at
// preserved). If previously consumed, the status is flipped back to 'posted'
// so a re-raised suspicion gets fresh attention — a finder that died after
// consuming its leads loses that claim; the next cycle will re-post if the
// suspicion still applies.
type Lead struct {
	ID         string
	ScanRunID  string
	PosterLens string
	TargetLens string
	File       string
	Line       int
	Note       string
	Status     string // "posted" | "consumed"
	CreatedAt  time.Time
	ConsumedAt time.Time // zero when not yet consumed
}

// AddLead upserts a lead. On conflict on (target_lens, file, line) it refreshes
// the note, poster_lens, and scan_run_id, and flips status back to 'posted' so
// a re-raised suspicion is treated as fresh. The original created_at is always
// preserved.
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
			scan_run_id = excluded.scan_run_id,
			status      = 'posted',
			consumed_at = NULL`,
		l.ID, l.ScanRunID, l.PosterLens, l.TargetLens, l.File, l.Line, l.Note,
		l.CreatedAt.Format(timeLayout),
	)
	return err
}

// PendingLeads returns all leads with status='posted' for the given target
// lens, ordered by created_at then id for determinism.
func (s *Store) PendingLeads(ctx context.Context, targetLens string) ([]Lead, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, scan_run_id, poster_lens, target_lens, file, line, note, status, created_at, consumed_at
		FROM leads
		WHERE target_lens = ? AND status = 'posted'
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

// ConsumeLeads marks the given lead IDs as consumed in a single transaction,
// recording the consumed_at timestamp on each row.
func (s *Store) ConsumeLeads(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	now := nowUTC().Format(timeLayout)
	placeholders := strings.Repeat(",?", len(ids))[1:] // "?,?,?" for len=3
	args := make([]any, len(ids)+1)
	args[0] = now
	for i, id := range ids {
		args[i+1] = id
	}
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`UPDATE leads SET status='consumed', consumed_at=? WHERE id IN (%s)`, placeholders),
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
	var consumedAt sql.NullString
	if err := rows.Scan(
		&l.ID, &l.ScanRunID, &l.PosterLens, &l.TargetLens,
		&l.File, &l.Line, &l.Note, &l.Status,
		&created, &consumedAt,
	); err != nil {
		return Lead{}, err
	}
	var err error
	if l.CreatedAt, err = parseTime(created); err != nil {
		return Lead{}, err
	}
	if consumedAt.Valid && consumedAt.String != "" {
		if l.ConsumedAt, err = parseTime(consumedAt.String); err != nil {
			return Lead{}, err
		}
	}
	return l, nil
}

// ListLeads returns the blackboard, newest-first (created_at DESC, id DESC for
// determinism). pendingOnly restricts to status='posted' — the tips waiting for
// their target lens's next run.
func (s *Store) ListLeads(ctx context.Context, pendingOnly bool) ([]Lead, error) {
	q := `SELECT id, scan_run_id, poster_lens, target_lens, file, line, note, status, created_at, consumed_at
		FROM leads`
	if pendingOnly {
		q += ` WHERE status = 'posted'`
	}
	q += ` ORDER BY created_at DESC, id DESC`

	rows, err := s.db.QueryContext(ctx, q)
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
