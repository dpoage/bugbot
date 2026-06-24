package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// PendingCandidate is one row in the pending_candidates write-ahead log: a
// finder-proposed bug persisted between the hypothesize and verify stages so an
// interrupted run's in-flight hypotheses survive and are replayed next run.
//
// Fidelity mirrors funnel.Candidate: every field the verifier needs to re-judge
// the claim against the current code is captured, so a replayed candidate skips
// re-hypothesize entirely. CorroboratingLenses is empty for a fresh finder
// emission (cross-lens corroboration is assigned later, in triage) but is
// persisted for forward-compatibility and round-trip fidelity.
//
// See migrations/009_pending_candidates.sql for the lifecycle: rows are written
// at the finder emit site, deleted at every terminal fate, and replayed at run
// start. A clean run leaves the table empty; only an interrupt leaves rows.
type PendingCandidate struct {
	ID                  string
	ScanRunID           string
	CommitSHA           string
	Lens                string
	File                string
	Line                int
	Title               string
	Description         string
	Severity            string
	Evidence            string
	Confidence          string
	CorroboratingLenses []string
	CreatedAt           time.Time
}

// AddPendingCandidates inserts a batch of pending candidates in a single
// transaction (the batch is one finder unit's output, matching the per-unit
// coverage-stamp discipline). Each row's ID is generated if empty and written
// back into the slice so the caller can carry it as the candidate's PendingID
// for the eventual terminal-fate delete; CreatedAt defaults to now. An empty
// batch is a no-op.
//
// Atomicity is per-batch: either the whole unit's candidates are durable or none
// are. A caller treats a non-nil error as best-effort (log and continue): a
// missed WAL write degrades to the pre-WAL behavior (that candidate is volatile)
// rather than aborting the scan.
func (s *Store) AddPendingCandidates(ctx context.Context, rows []PendingCandidate) error {
	if len(rows) == 0 {
		return nil
	}
	now := nowUTC()
	return s.withTx(ctx, func(tx *sql.Tx) error {
		for i := range rows {
			if rows[i].ID == "" {
				rows[i].ID = newID()
			}
			if rows[i].CreatedAt.IsZero() {
				rows[i].CreatedAt = now
			}
			lensesJSON, err := json.Marshal(rows[i].CorroboratingLenses)
			if err != nil {
				lensesJSON = []byte("[]")
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO pending_candidates
				  (id, scan_run_id, commit_sha, lens, file, line, title,
				   description, severity, evidence, confidence,
				   corroborating_lenses, created_at)
				VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				rows[i].ID, rows[i].ScanRunID, rows[i].CommitSHA, rows[i].Lens,
				rows[i].File, rows[i].Line, rows[i].Title, rows[i].Description,
				rows[i].Severity, rows[i].Evidence, rows[i].Confidence,
				string(lensesJSON), rows[i].CreatedAt.UTC().Format(timeLayout),
			); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeletePendingCandidate removes one row by id. An empty id is a no-op (returns
// nil) so callers can delete unconditionally at a terminal fate regardless of
// whether the candidate was ever WAL-persisted. Best-effort by convention: a
// lingering row self-heals on the next run (replayed, then deleted again at its
// terminal fate), so callers log and continue rather than abort the scan.
func (s *Store) DeletePendingCandidate(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}
	_, err := s.exec(ctx, "delete_pending_candidate", `DELETE FROM pending_candidates WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return nil
}

// ListPendingCandidates returns every pending candidate across all scan runs in
// insertion order (by rowid). Called at run start to replay an interrupted
// predecessor's surviving hypotheses into triage/verify. The table is empty
// after any clean run, so this is a cheap no-op in the common case.
//
// Ordering is by rowid, not id: newID() is only millisecond-precise plus a
// random suffix, so two candidates inserted in the same millisecond would not
// sort by creation under ORDER BY id. rowid is monotonic with insertion, giving
// a stable, creation-ordered replay.
func (s *Store) ListPendingCandidates(ctx context.Context) ([]PendingCandidate, error) {
	return queryRows(ctx, s, "list_pending_candidates", `
		SELECT id, scan_run_id, commit_sha, lens, file, line, title,
		       description, severity, evidence, confidence,
		       corroborating_lenses, created_at
		FROM pending_candidates
		ORDER BY rowid`,
		nil, scanPendingCandidate,
	)
}

// scanPendingCandidate reads one pending_candidates row from a *sql.Rows cursor.
func scanPendingCandidate(rows *sql.Rows) (PendingCandidate, error) {
	var pc PendingCandidate
	var lensesJSON, createdAt string
	if err := rows.Scan(
		&pc.ID, &pc.ScanRunID, &pc.CommitSHA, &pc.Lens, &pc.File, &pc.Line,
		&pc.Title, &pc.Description, &pc.Severity, &pc.Evidence, &pc.Confidence,
		&lensesJSON, &createdAt,
	); err != nil {
		return PendingCandidate{}, err
	}
	if err := json.Unmarshal([]byte(lensesJSON), &pc.CorroboratingLenses); err != nil {
		pc.CorroboratingLenses = nil
	}
	t, err := parseTime(createdAt)
	if err != nil {
		return PendingCandidate{}, err
	}
	pc.CreatedAt = t
	return pc, nil
}
