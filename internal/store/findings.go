package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/dpoage/bugbot/internal/domain"
)

// Site is one code location (file + line) contributing to a finding. The
// primary's own location is always Sites[0]; additional entries come from
// same-root-cause members collapsed into the primary during triage.
type Site struct {
	File string
	Line int
}

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
	ID            string
	Fingerprint   string
	Title         string
	Description   string
	Reasoning     string // the adversarial verification trace
	VerdictDetail string // impact-sweep rationale for severity re-rank (empty = not re-ranked)
	Severity      domain.Severity
	Tier          domain.Tier // T0 fix-witnessed, T1 reproduced, T2 verified, T3 suspected
	Status        Status
	Lens          string
	File          string
	Line          int
	CommitSHA     string
	FileHash      string
	ReproPath     string // empty when no reproduction exists
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
	// Sites records every code location this finding represents. Sites[0] is the
	// primary's (File, Line). Additional entries are same-root-cause merge sites.
	// Stored as a pipe-separated "file:line" list; empty when no merge occurred.
	Sites      []Site
	Confidence float64 // derived: findingConfidence(tier, severity, len(corroboratingLenses))
	CreatedAt  time.Time
	UpdatedAt  time.Time
	// SweptAt is the timestamp when UpdateFindingSeverity last recorded a sweep
	// verdict. Zero = not swept (impact-sweep marker; set by UpdateFindingSeverity,
	// preserved across re-upsert when file_hash is unchanged, reset on file_hash change).
	SweptAt time.Time
}

// encodeLenses encodes the lens list as a JSON array for storage in the
// corroborating_lenses column. Nil/empty yields the empty string.
func encodeLenses(lenses []string) string {
	if len(lenses) == 0 {
		return ""
	}
	b, err := json.Marshal(lenses)
	if err != nil {
		// json.Marshal on []string never errors; panic would be a bug.
		panic("store: encodeLenses: unexpected marshal error: " + err.Error())
	}
	return string(b)
}

// decodeLenses parses the corroborating_lenses column back into a slice.
// It is tolerant of the legacy comma-separated encoding: if the stored value
// does not begin with '[' it is split on commas (old rows), otherwise it is
// decoded as a JSON array (new rows). The empty string yields nil.
func decodeLenses(s string) []string {
	if s == "" {
		return nil
	}
	if len(s) > 0 && s[0] == '[' {
		var out []string
		if err := json.Unmarshal([]byte(s), &out); err == nil {
			return out
		}
	}
	// Legacy comma-separated fallback.
	return strings.Split(s, ",")
}

// encodeSites encodes a slice of Site into a newline-separated list of
// "file|line" entries. Format: "file|line\nfile|line\n..."
// Empty/nil yields "".
//
// Encoding limitations (accept these for source-file paths in practice):
//   - Pipe characters ('|') in file paths are replaced with U+FFFD (lossy).
//     Decoding restores U+FFFD to '|' for display, but a file path that
//     genuinely contained U+FFFD would not round-trip correctly.
//   - Newline characters ('\n') in file paths are not escaped; they would
//     split into bogus entries on decode. Source-file paths do not contain
//     newlines in practice.
func encodeSites(sites []Site) string {
	if len(sites) == 0 {
		return ""
	}
	parts := make([]string, len(sites))
	for i, s := range sites {
		safeFile := strings.ReplaceAll(s.File, "|", "\ufffd")
		parts[i] = safeFile + "|" + fmt.Sprintf("%d", s.Line)
	}
	return strings.Join(parts, "\n")
}

// decodeSites parses the pipe/newline encoded sites column. Empty string yields nil.
func decodeSites(s string) []Site {
	if s == "" {
		return nil
	}
	entries := strings.Split(s, "\n")
	out := make([]Site, 0, len(entries))
	for _, e := range entries {
		idx := strings.LastIndex(e, "|")
		if idx < 0 {
			continue // malformed entry; skip
		}
		file := strings.ReplaceAll(e[:idx], "\ufffd", "|")
		var line int
		if _, err := fmt.Sscanf(e[idx+1:], "%d", &line); err != nil {
			continue // malformed line number; skip entry
		}
		out = append(out, Site{File: file, Line: line})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// findingConfidence derives a [0,1] confidence score from the evidence quality
// of a finding. The score is monotonic in both axes:
//
//   - tier: fix-witnessed (T0) is strongest, reproduced (T1) is next, verified
//     (T2) is middle, suspected (T3) is weakest.
//
//   - corroboratingLensCount: each additional corroborating lens adds a fixed
//     increment, capped so the combined value never exceeds 1.
//
// severity contributes a small tie-breaking bonus (critical > high > medium >
// everything else) so that among equally-tiered, equally-corroborated findings
// the more severe ones surface first.
func findingConfidence(tier domain.Tier, severity domain.Severity, corroboratingLensCount int) float64 {
	base := tier.BaseConfidence()

	// Severity bonus: up to 0.08 so it never overrides a tier boundary.
	var sevBonus float64
	switch severity {
	case domain.SeverityCritical:
		sevBonus = 0.08
	case domain.SeverityHigh:
		sevBonus = 0.05
	case domain.SeverityMedium:
		sevBonus = 0.02
	}

	// Corroboration: 0.04 per additional lens, capped at 0.12.
	corrob := float64(corroboratingLensCount) * 0.04
	if corrob > 0.12 {
		corrob = 0.12
	}

	v := base + sevBonus + corrob
	if v > 1.0 {
		v = 1.0
	}
	return v
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

	err := s.withTx(ctx, func(tx *sql.Tx) error {
		suppressed, err := isSuppressedTx(ctx, tx, f.Fingerprint)
		if err != nil {
			return annotateErr(s.path, "upsert_finding", err)
		}
		if suppressed {
			f.Status = StatusDismissed
		}

		now := nowUTC()

		// Does a row already exist for this fingerprint?
		var existingID, existingCreated, existingFileHash string
		var existingTier domain.Tier
		var existingRepro, existingSweptAt sql.NullString
		var existingNeedsHuman int
		err = tx.QueryRowContext(ctx,
			`SELECT id, created_at, tier, repro_path, needs_human, file_hash, swept_at FROM findings WHERE fingerprint = ?`, f.Fingerprint,
		).Scan(&existingID, &existingCreated, &existingTier, &existingRepro, &existingNeedsHuman, &existingFileHash, &existingSweptAt)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			if f.ID == "" {
				f.ID = newID()
			}
			f.CreatedAt = now
			f.UpdatedAt = now
			f.Confidence = findingConfidence(f.Tier, f.Severity, len(f.CorroboratingLenses))
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO findings
				  (id, fingerprint, title, description, reasoning, verdict_detail, severity, tier,
				   status, lens, file, line, commit_sha, file_hash, repro_path,
				   fix_patch, needs_human,
				   corroborating_lenses, sites, confidence, created_at, updated_at, swept_at)
				VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				f.ID, f.Fingerprint, f.Title, f.Description, f.Reasoning, f.VerdictDetail, f.Severity,
				f.Tier, string(f.Status), f.Lens, f.File, f.Line, f.CommitSHA,
				f.FileHash, nullStr(f.ReproPath), f.FixPatch, boolInt(f.NeedsHuman),
				encodeLenses(f.CorroboratingLenses), encodeSites(f.Sites), f.Confidence,
				f.CreatedAt.Format(timeLayout), f.UpdatedAt.Format(timeLayout), nullTime(f.SweptAt),
			); err != nil {
				return annotateErr(s.path, "upsert_finding", err)
			}

		case err != nil:
			return annotateErr(s.path, "upsert_finding", err)

		default:
			// Update in place: keep id and created_at, refresh everything else.
			// Promotion-preserving rules (implicit re-scan must never regress
			// promotion state earned by a prior sandboxed reproduce attempt):
			//
			//   tier       — never increase the tier number via implicit upsert; lower
			//                number is stronger (1=reproduced, 2=verified, 3=suspected).
			//                MIN(stored, incoming) is enforced with a CASE expression so
			//                an incoming T2 re-scan never demotes a stored T1.  An
			//                explicit promotion (incoming T1 on a stored T2) still
			//                promotes correctly because MIN(2,1)=1.
			//
			//   repro_path — never clear a non-empty stored repro_path with an empty
			//                incoming value.  A genuine re-repro (non-empty incoming)
			//                updates it normally.  nullStr converts "" to NULL so the
			//                IS NULL check is sufficient; no separate ''='' check needed.
			//
			//   needs_human — once the patch-prover exhausts its budget and sets this
			//                 flag, implicit re-scans (which do not run the patch-prover)
			//                 must not clear it.  A re-scan always produces
			//                 NeedsHuman=false; without preservation the flag would be
			//                 cleared on every sweep.  Explicit mutation paths (promoteFinding,
			//                 patch.go) read-then-upsert with the current row, so they carry
			//                 the stored value and are unaffected by this guard.
			f.ID = existingID
			created, perr := parseTime(existingCreated)
			if perr != nil {
				return perr
			}
			f.CreatedAt = created
			f.UpdatedAt = now

			// Resolve the promotion-guarded values before executing the UPDATE so that
			// the returned Finding struct accurately reflects what was actually written.
			if f.Tier > existingTier {
				// Incoming tier is weaker (higher number); keep the stored stronger tier.
				f.Tier = existingTier
			}
			if f.ReproPath == "" && existingRepro.Valid && existingRepro.String != "" {
				// Incoming has no repro; preserve the stored artifact path.
				f.ReproPath = existingRepro.String
			}
			if existingNeedsHuman != 0 {
				// Stored needs_human=true; do not let a re-scan clear it.
				f.NeedsHuman = true
			}
			// swept_at: preserve across idempotent re-discovery; reset when the
			// anchored code (file_hash) changed so the sweep re-evaluates reachability.
			if f.FileHash == existingFileHash && existingSweptAt.Valid && existingSweptAt.String != "" {
				f.SweptAt, _ = parseTime(existingSweptAt.String) // preserve stored marker
			} else {
				f.SweptAt = time.Time{} // new code version (or never swept) → re-sweep eligible
			}

			f.Confidence = findingConfidence(f.Tier, f.Severity, len(f.CorroboratingLenses))

			// The CASE expressions mirror the Go logic above to guarantee atomicity —
			// a concurrent writer cannot slip in between the SELECT and this UPDATE.
			// Arg order matches the positional ? placeholders exactly:
			//   tier        : ?, ?  → f.Tier (compare), f.Tier (THEN value)
			//   repro_path  : ?, ?  → nullStr(f.ReproPath) × 2
			//   needs_human : ?     → boolInt(f.NeedsHuman)
			//   swept_at    : ?     → f.FileHash (compare to stored file_hash;
			//                         preserve stored swept_at if unchanged, else NULL)
			if _, err := tx.ExecContext(ctx, `
				UPDATE findings SET
				  title=?, description=?, reasoning=?, verdict_detail=?, severity=?,
				  tier       = CASE WHEN ? < tier THEN ? ELSE tier END,
				  status=?,
				  lens=?, file=?, line=?, commit_sha=?, file_hash=?,
				  repro_path = CASE WHEN ? IS NULL THEN repro_path ELSE ? END,
				  fix_patch=?,
				  needs_human = CASE WHEN needs_human = 1 THEN 1 ELSE ? END,
				  corroborating_lenses=?, sites=?, confidence=?, updated_at=?,
				  swept_at = CASE WHEN ? = file_hash THEN swept_at ELSE NULL END
				WHERE id=?`,
				f.Title, f.Description, f.Reasoning, f.VerdictDetail, f.Severity,
				f.Tier, f.Tier,
				string(f.Status),
				f.Lens, f.File, f.Line, f.CommitSHA, f.FileHash,
				nullStr(f.ReproPath), nullStr(f.ReproPath),
				f.FixPatch,
				boolInt(f.NeedsHuman),
				encodeLenses(f.CorroboratingLenses), encodeSites(f.Sites), f.Confidence,
				f.UpdatedAt.Format(timeLayout), f.FileHash, f.ID,
			); err != nil {
				return annotateErr(s.path, "upsert_finding", err)
			}
		}
		return nil
	})
	if err != nil {
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
//
// ORDER BY updated_at DESC, id ASC: updated_at is the primary key for "newest";
// id (the unique primary key) is the tiebreaker for findings that share an
// updated_at down to the nanosecond.
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
		return nil, annotateErr(s.path, "list_findings", err)
	}
	defer func() { _ = rows.Close() }()
	// scanFinding takes a rowScanner (works for both *sql.Row and *sql.Rows).
	// The closure adapts it to scanRows' *sql.Rows signature.
	return scanRows(rows, func(r *sql.Rows) (Finding, error) { return scanFinding(r) })
}

// UpdateStatus sets the status of the finding with the given fingerprint. When
// the new status is StatusDismissed it also records a suppression with the
// supplied reason, so the dismissal is remembered across scans. Returns
// ErrNotFound if no finding has that fingerprint.
//
// reason is only consulted for dismissals; pass "" otherwise.
func (s *Store) UpdateStatus(ctx context.Context, fingerprint string, status Status, reason string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE findings SET status = ?, updated_at = ? WHERE fingerprint = ?`,
			string(status), nowUTC().Format(timeLayout), fingerprint,
		)
		if err != nil {
			return annotateErr(s.path, "update_status", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return annotateErr(s.path, "update_status", err)
		}
		if n == 0 {
			return ErrNotFound
		}

		if status == StatusDismissed {
			if err := addSuppressionTx(ctx, tx, fingerprint, reason); err != nil {
				return annotateErr(s.path, "update_status", err)
			}
		}
		return nil
	})
}

// MarkFixed sets the finding's status to StatusFixed.
func (s *Store) MarkFixed(ctx context.Context, fingerprint string) error {
	return s.UpdateStatus(ctx, fingerprint, StatusFixed, "")
}

// AddCorroboratingLenses appends lenses to the corroborating_lenses column of
// the finding identified by fingerprint, deduplicating and sorting the result.
// It is used by the streaming triage consumer when a later-arriving cluster
// member's primary has already been persisted (verified + upserted) before the
// member arrived — the primary's row needs the new corroborating lens attached.
// Returns ErrNotFound when no finding with that fingerprint exists (callers
// treat this as a no-op: the primary may have been killed, not just not yet
// persisted). No-op when lenses is empty.
func (s *Store) AddCorroboratingLenses(ctx context.Context, fingerprint string, lenses []string) error {
	if len(lenses) == 0 {
		return nil
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var corrob string
		err := tx.QueryRowContext(ctx,
			`SELECT corroborating_lenses FROM findings WHERE fingerprint = ?`, fingerprint,
		).Scan(&corrob)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return annotateErr(s.path, "add_corroborating_lenses", err)
		}

		// Merge existing + new, deduplicate, sort.
		existing := decodeLenses(corrob)
		seen := make(map[string]bool, len(existing)+len(lenses))
		for _, l := range existing {
			seen[l] = true
		}
		for _, l := range lenses {
			seen[l] = true
		}
		merged := make([]string, 0, len(seen))
		for l := range seen {
			merged = append(merged, l)
		}
		sort.Strings(merged)

		now := nowUTC()
		if _, err := tx.ExecContext(ctx,
			`UPDATE findings SET corroborating_lenses = ?, updated_at = ? WHERE fingerprint = ?`,
			encodeLenses(merged), now.Format(timeLayout), fingerprint,
		); err != nil {
			return annotateErr(s.path, "add_corroborating_lenses", err)
		}
		return nil
	})
}

// UpdateFindingSeverity updates the severity and verdict_detail of a finding by
// id, recomputing Confidence from the new severity (tier and corroborating-lens
// count are read from the current row so the confidence formula is applied
// consistently). Returns ErrNotFound when no finding has that id.
func (s *Store) UpdateFindingSeverity(ctx context.Context, id string, sev domain.Severity, rationale string) error {
	now := nowUTC()
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var tier domain.Tier
		var corrob string
		err := tx.QueryRowContext(ctx,
			`SELECT tier, corroborating_lenses FROM findings WHERE id = ?`, id,
		).Scan(&tier, &corrob)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return annotateErr(s.path, "update_finding_severity", err)
		}
		lenses := decodeLenses(corrob)
		conf := findingConfidence(tier, sev, len(lenses))
		_, err = tx.ExecContext(ctx,
			`UPDATE findings SET severity=?, verdict_detail=?, confidence=?, updated_at=?, swept_at=? WHERE id=?`,
			sev, rationale, conf, now.Format(timeLayout), now.UTC().Format(timeLayout), id,
		)
		if err != nil {
			return annotateErr(s.path, "update_finding_severity", err)
		}
		return nil
	})
}

// UnsweptOpenFindings returns all open findings whose swept_at marker is NULL,
// ordered oldest-updated-first. This is the WorkRemaining query for the
// impact-sweep pass: it drives SweepDrain and ensures rotation parity with
// OpenBacklog (deferred items have their updated_at bumped and move to the back).
func (s *Store) UnsweptOpenFindings(ctx context.Context) ([]Finding, error) {
	rows, err := s.db.QueryContext(ctx,
		findingColumns+" FROM findings WHERE status = 'open' AND swept_at IS NULL ORDER BY updated_at ASC, id ASC",
	)
	if err != nil {
		return nil, annotateErr(s.path, "unswept_open_findings", err)
	}
	defer func() { _ = rows.Close() }()
	return scanRows(rows, func(r *sql.Rows) (Finding, error) { return scanFinding(r) })
}

// AppendFindingSites appends sites to the sites column of the finding identified
// by fingerprint, deduplicating entries with the same (file,line). Used by the
// streaming triage consumer when a root-cause-merged member's primary has already
// been persisted before the member arrived. Returns ErrNotFound when no finding
// with that fingerprint exists; callers treat this as a no-op (primary killed).
// No-op when sites is empty.
func (s *Store) AppendFindingSites(ctx context.Context, fingerprint string, sites []Site) error {
	if len(sites) == 0 {
		return nil
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var sitesStr string
		err := tx.QueryRowContext(ctx,
			`SELECT sites FROM findings WHERE fingerprint = ?`, fingerprint,
		).Scan(&sitesStr)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return annotateErr(s.path, "append_finding_sites", err)
		}

		existing := decodeSites(sitesStr)
		// Deduplicate by (file,line).
		type key struct {
			f string
			l int
		}
		seen := make(map[key]bool, len(existing)+len(sites))
		merged := make([]Site, 0, len(existing)+len(sites))
		for _, s := range existing {
			k := key{s.File, s.Line}
			if !seen[k] {
				seen[k] = true
				merged = append(merged, s)
			}
		}
		for _, s := range sites {
			k := key{s.File, s.Line}
			if !seen[k] {
				seen[k] = true
				merged = append(merged, s)
			}
		}

		now := nowUTC()
		if _, err := tx.ExecContext(ctx,
			`UPDATE findings SET sites = ?, updated_at = ? WHERE fingerprint = ?`,
			encodeSites(merged), now.Format(timeLayout), fingerprint,
		); err != nil {
			return annotateErr(s.path, "append_finding_sites", err)
		}
		return nil
	})
}

// findingColumns is the SELECT column list shared by single- and multi-row reads.
const findingColumns = `SELECT id, fingerprint, title, description, reasoning, verdict_detail,
	severity, tier, status, lens, file, line, commit_sha, file_hash, repro_path,
	fix_patch, needs_human,
	corroborating_lenses, sites, confidence, created_at, updated_at, swept_at`

func (s *Store) queryOne(ctx context.Context, whereClause string, args ...any) (Finding, error) {
	row := s.db.QueryRowContext(ctx, findingColumns+" FROM findings "+whereClause, args...)
	f, err := scanFinding(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Finding{}, ErrNotFound
	}
	if err != nil {
		return Finding{}, annotateErr(s.path, "query_finding", err)
	}
	return f, nil
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
		sitesStr             string
		createdAt, updatedAt string
		sweptAt              sql.NullString
	)
	if err := sc.Scan(
		&f.ID, &f.Fingerprint, &f.Title, &f.Description, &f.Reasoning, &f.VerdictDetail,
		&f.Severity, &f.Tier, &status, &f.Lens, &f.File, &f.Line, &f.CommitSHA,
		&f.FileHash, &repro, &f.FixPatch, &needsHuman, &corrob, &sitesStr, &f.Confidence, &createdAt, &updatedAt, &sweptAt,
	); err != nil {
		return Finding{}, err
	}
	f.Status = Status(status)
	f.ReproPath = repro.String
	f.NeedsHuman = needsHuman != 0
	f.CorroboratingLenses = decodeLenses(corrob)
	f.Sites = decodeSites(sitesStr)
	var err error
	if f.CreatedAt, err = parseTime(createdAt); err != nil {
		return Finding{}, err
	}
	if f.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return Finding{}, err
	}
	if sweptAt.Valid && sweptAt.String != "" {
		if f.SweptAt, err = parseTime(sweptAt.String); err != nil {
			return Finding{}, err
		}
	}
	return f, nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(timeLayout)
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
		return FindingTallies{}, annotateErr(s.path, "count_findings", err)
	}
	defer func() { _ = rows.Close() }()
	_, err = scanRows(rows, func(r *sql.Rows) (struct{}, error) {
		var status string
		var tier, n, needs int
		if err := r.Scan(&status, &tier, &n, &needs); err != nil {
			return struct{}{}, err
		}
		switch Status(status) {
		case StatusOpen:
			t.OpenByTier[tier] += n
			t.NeedsHuman += needs
		case StatusFixed:
			t.Fixed += n
		case StatusDismissed:
			t.Dismissed += n
		default:
			// StatusSuperseded (and any future states) intentionally omitted
			// from the pane: nothing writes superseded today, and the tallies
			// show actionable lifecycle states only.
		}
		return struct{}{}, nil
	})
	return t, err
}
