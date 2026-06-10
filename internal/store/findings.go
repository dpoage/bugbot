package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
)

// Status enumerates the lifecycle states of a finding.
type Status string

const (
	// StatusOpen means the finding is active and may be reported.
	StatusOpen Status = "open"
	// StatusDismissed means a maintainer rejected it; it is suppressed forever.
	StatusDismissed Status = "dismissed"
	// StatusFixed means the underlying bug has been resolved.
	StatusFixed Status = "fixed"
	// StatusSuperseded means a newer finding replaced this one (e.g. the code
	// moved and a new fingerprint was generated).
	StatusSuperseded Status = "superseded"
)

// ErrNotFound is returned by Get-style methods when no row matches.
var ErrNotFound = errors.New("store: not found")

// Finding is a single candidate (or confirmed) bug. It is anchored to a code
// version through CommitSHA and FileHash so the daemon can detect when the
// underlying code has changed and re-verification is warranted.
type Finding struct {
	ID          string
	Fingerprint string
	Title       string
	Description string
	Reasoning   string // the adversarial verification trace
	Severity    string
	Tier        int // 1 reproduced, 2 verified, 3 suspected
	Status      Status
	Lens        string
	File        string
	Line        int
	CommitSHA   string
	FileHash    string
	ReproPath   string // empty when no reproduction exists
	// FixPatch is the unified diff text produced by the patch-prover stage when a
	// minimal fix candidate was witnessed by the sandbox.  Empty when the prover
	// was not run or found no plausible fix.
	FixPatch string
	// NeedsHuman is set when the patch-prover exhausted its attempt budget without
	// finding a minimal fix.  A fix-refusing bug is often misdiagnosed; human
	// review is the appropriate escalation.
	NeedsHuman bool
	// CorroboratingLenses are the OTHER lenses that independently reported this
	// same defect and were collapsed into this finding by triage's location-based
	// cross-lens dedup. It excludes the finding's own Lens. Persisted as a
	// comma-separated text column; it is a reporting signal only and does NOT
	// affect the finding's tier or status.
	CorroboratingLenses []string
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// encodeLenses joins a lens list into the comma-separated form stored in the
// corroborating_lenses column. Lens names must not contain commas for the
// encoding to round-trip; any comma is replaced with a semicolon so a
// nonconforming lens name degrades visibly instead of splitting into phantom
// entries on read-back. Nil/empty yields the empty string.
func encodeLenses(lenses []string) string {
	if len(lenses) == 0 {
		return ""
	}
	safe := make([]string, len(lenses))
	for i, l := range lenses {
		safe[i] = strings.ReplaceAll(l, ",", ";")
	}
	return strings.Join(safe, ",")
}

// decodeLenses parses the comma-separated corroborating_lenses column back into
// a slice. The empty string yields nil (no corroboration).
func decodeLenses(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// Fingerprint computes the stable dedup key for a finding from the fields that
// identify "the same bug" independent of incidental code movement: the lens
// that found it, the file, a normalized location, and a hash of the title.
//
// The location is normalized to the file path (lowercased, slash-cleaned) plus
// the line so small edits elsewhere in the file don't change the key; the title
// is hashed so wording is captured but the key stays a fixed length. Two
// findings with the same fingerprint are treated as the same bug and deduped.
func Fingerprint(lens, file string, line int, title string) string {
	normFile := strings.ToLower(path.Clean(strings.ReplaceAll(file, "\\", "/")))
	normTitle := strings.ToLower(strings.Join(strings.Fields(title), " "))
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\x00%s\x00%d\x00%s", strings.ToLower(lens), normFile, line, normTitle)
	return hex.EncodeToString(h.Sum(nil))
}

// FindingFilter narrows ListFindings. Zero-valued fields are not applied.
type FindingFilter struct {
	Status Status // exact status match
	// Tier is an exact tier match for 1..3. 0 is the "any tier" SENTINEL — it
	// cannot select T0 (fix-witnessed) rows alone; those appear in unfiltered
	// queries. Changing the sentinel would break existing callers; revisit if
	// T0-only queries are ever needed.
	Tier      int
	CommitSHA string // findings anchored to a specific commit
	Lens      string
}

// UpsertFinding inserts the finding or, if one with the same fingerprint exists,
// updates its mutable fields and bumps updated_at while preserving id and
// created_at. It returns the stored row.
//
// Suppression memory is enforced here: if the fingerprint has ever been
// suppressed, the stored status is forced to StatusDismissed regardless of the
// requested status, so a re-discovered bug never resurfaces as open. The caller
// may pass any status; this method owns the final decision.
func (s *Store) UpsertFinding(ctx context.Context, f Finding) (Finding, error) {
	if f.Fingerprint == "" {
		return Finding{}, fmt.Errorf("store: UpsertFinding requires a fingerprint")
	}
	if f.Status == "" {
		f.Status = StatusOpen
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Finding{}, err
	}
	defer func() { _ = tx.Rollback() }()

	suppressed, err := isSuppressedTx(ctx, tx, f.Fingerprint)
	if err != nil {
		return Finding{}, err
	}
	if suppressed {
		f.Status = StatusDismissed
	}

	now := nowUTC()

	// Does a row already exist for this fingerprint?
	var existingID, existingCreated string
	err = tx.QueryRowContext(ctx,
		`SELECT id, created_at FROM findings WHERE fingerprint = ?`, f.Fingerprint,
	).Scan(&existingID, &existingCreated)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		if f.ID == "" {
			f.ID = newID()
		}
		f.CreatedAt = now
		f.UpdatedAt = now
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO findings
			  (id, fingerprint, title, description, reasoning, severity, tier,
			   status, lens, file, line, commit_sha, file_hash, repro_path,
			   fix_patch, needs_human,
			   corroborating_lenses, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			f.ID, f.Fingerprint, f.Title, f.Description, f.Reasoning, f.Severity,
			f.Tier, string(f.Status), f.Lens, f.File, f.Line, f.CommitSHA,
			f.FileHash, nullStr(f.ReproPath), f.FixPatch, boolInt(f.NeedsHuman),
			encodeLenses(f.CorroboratingLenses),
			f.CreatedAt.Format(timeLayout), f.UpdatedAt.Format(timeLayout),
		); err != nil {
			return Finding{}, err
		}

	case err != nil:
		return Finding{}, err

	default:
		// Update in place: keep id and created_at, refresh everything else.
		f.ID = existingID
		created, perr := parseTime(existingCreated)
		if perr != nil {
			return Finding{}, perr
		}
		f.CreatedAt = created
		f.UpdatedAt = now
		if _, err := tx.ExecContext(ctx, `
			UPDATE findings SET
			  title=?, description=?, reasoning=?, severity=?, tier=?, status=?,
			  lens=?, file=?, line=?, commit_sha=?, file_hash=?, repro_path=?,
			  fix_patch=?, needs_human=?,
			  corroborating_lenses=?, updated_at=?
			WHERE id=?`,
			f.Title, f.Description, f.Reasoning, f.Severity, f.Tier,
			string(f.Status), f.Lens, f.File, f.Line, f.CommitSHA, f.FileHash,
			nullStr(f.ReproPath), f.FixPatch, boolInt(f.NeedsHuman),
			encodeLenses(f.CorroboratingLenses),
			f.UpdatedAt.Format(timeLayout), f.ID,
		); err != nil {
			return Finding{}, err
		}
	}

	if err := tx.Commit(); err != nil {
		return Finding{}, err
	}
	return f, nil
}

// GetFinding returns the finding with the given id, or ErrNotFound.
func (s *Store) GetFinding(ctx context.Context, id string) (Finding, error) {
	return s.queryOne(ctx, `WHERE id = ?`, id)
}

// GetFindingByFingerprint returns the finding with the given fingerprint, or
// ErrNotFound.
func (s *Store) GetFindingByFingerprint(ctx context.Context, fingerprint string) (Finding, error) {
	return s.queryOne(ctx, `WHERE fingerprint = ?`, fingerprint)
}

// ListFindings returns findings matching the filter, newest-updated first.
func (s *Store) ListFindings(ctx context.Context, filter FindingFilter) ([]Finding, error) {
	var where []string
	var args []any
	if filter.Status != "" {
		where = append(where, "status = ?")
		args = append(args, string(filter.Status))
	}
	if filter.Tier != 0 {
		where = append(where, "tier = ?")
		args = append(args, filter.Tier)
	}
	if filter.CommitSHA != "" {
		where = append(where, "commit_sha = ?")
		args = append(args, filter.CommitSHA)
	}
	if filter.Lens != "" {
		where = append(where, "lens = ?")
		args = append(args, filter.Lens)
	}

	q := findingColumns + " FROM findings"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY updated_at DESC, id ASC"

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []Finding
	for rows.Next() {
		f, err := scanFinding(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// UpdateStatus sets the status of the finding with the given fingerprint. When
// the new status is StatusDismissed it also records a suppression with the
// supplied reason, so the dismissal is remembered across scans. Returns
// ErrNotFound if no finding has that fingerprint.
//
// reason is only consulted for dismissals; pass "" otherwise.
func (s *Store) UpdateStatus(ctx context.Context, fingerprint string, status Status, reason string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`UPDATE findings SET status = ?, updated_at = ? WHERE fingerprint = ?`,
		string(status), nowUTC().Format(timeLayout), fingerprint,
	)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}

	if status == StatusDismissed {
		if err := addSuppressionTx(ctx, tx, fingerprint, reason); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// MarkFixed sets the finding's status to StatusFixed.
func (s *Store) MarkFixed(ctx context.Context, fingerprint string) error {
	return s.UpdateStatus(ctx, fingerprint, StatusFixed, "")
}

// findingColumns is the SELECT column list shared by single- and multi-row reads.
const findingColumns = `SELECT id, fingerprint, title, description, reasoning,
	severity, tier, status, lens, file, line, commit_sha, file_hash, repro_path,
	fix_patch, needs_human,
	corroborating_lenses, created_at, updated_at`

func (s *Store) queryOne(ctx context.Context, whereClause string, args ...any) (Finding, error) {
	row := s.db.QueryRowContext(ctx, findingColumns+" FROM findings "+whereClause, args...)
	f, err := scanFinding(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Finding{}, ErrNotFound
	}
	return f, err
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanFinding(sc rowScanner) (Finding, error) {
	var (
		f                    Finding
		status               string
		repro                sql.NullString
		needsHuman           int
		corrob               string
		createdAt, updatedAt string
	)
	if err := sc.Scan(
		&f.ID, &f.Fingerprint, &f.Title, &f.Description, &f.Reasoning,
		&f.Severity, &f.Tier, &status, &f.Lens, &f.File, &f.Line, &f.CommitSHA,
		&f.FileHash, &repro, &f.FixPatch, &needsHuman, &corrob, &createdAt, &updatedAt,
	); err != nil {
		return Finding{}, err
	}
	f.Status = Status(status)
	f.ReproPath = repro.String
	f.NeedsHuman = needsHuman != 0
	f.CorroboratingLenses = decodeLenses(corrob)
	var err error
	if f.CreatedAt, err = parseTime(createdAt); err != nil {
		return Finding{}, err
	}
	if f.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return Finding{}, err
	}
	return f, nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// boolInt converts a bool to the SQLite convention: 1 for true, 0 for false.
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// FindingTallies is the aggregate finding picture for the status pane.
type FindingTallies struct {
	// OpenByTier counts StatusOpen findings keyed by tier (0..3).
	OpenByTier map[int]int
	// NeedsHuman counts open findings flagged by the patch-prover for review.
	NeedsHuman int
	// Fixed and Dismissed count terminal-state findings.
	Fixed     int
	Dismissed int
}

// CountFindings aggregates findings by status and tier in one query for the
// status world-state block.
func (s *Store) CountFindings(ctx context.Context) (FindingTallies, error) {
	t := FindingTallies{OpenByTier: map[int]int{}}
	rows, err := s.db.QueryContext(ctx, `
		SELECT status, tier, COUNT(*), COALESCE(SUM(needs_human), 0)
		FROM findings GROUP BY status, tier`)
	if err != nil {
		return FindingTallies{}, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var status string
		var tier, n, needs int
		if err := rows.Scan(&status, &tier, &n, &needs); err != nil {
			return FindingTallies{}, err
		}
		switch Status(status) {
		case StatusOpen:
			t.OpenByTier[tier] += n
			t.NeedsHuman += needs
		case StatusFixed:
			t.Fixed += n
		case StatusDismissed:
			t.Dismissed += n
		}
	}
	return t, rows.Err()
}
