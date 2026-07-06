package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/dpoage/bugbot/internal/domain"
)

// ErrNotFound is returned by Get-style methods when no row matches.
var ErrNotFound = errors.New("store: not found")

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

// encodeSites encodes a slice of domain.Site into a newline-separated list of
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
func encodeSites(sites []domain.Site) string {
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
func decodeSites(s string) []domain.Site {
	if s == "" {
		return nil
	}
	entries := strings.Split(s, "\n")
	out := make([]domain.Site, 0, len(entries))
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
		out = append(out, domain.Site{File: file, Line: line})
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

// UpsertFinding inserts the finding or, if one with the same fingerprint exists,
// updates its mutable fields and bumps updated_at while preserving id and
// created_at. It returns the stored row.
//
// Suppression memory is enforced here: if the fingerprint has ever been
// suppressed, the stored status is forced to StatusDismissed regardless of the
// requested status, so a re-discovered bug never resurfaces as open. The caller
// may pass any status; this method owns the final decision.
func (s *Store) UpsertFinding(ctx context.Context, f domain.Finding) (domain.Finding, error) {
	if f.Fingerprint == "" {
		return domain.Finding{}, fmt.Errorf("store: UpsertFinding requires a fingerprint")
	}
	if f.Status == "" {
		f.Status = domain.StatusOpen
	}
	// Confidence is always derived, never trusted from the caller.
	f.Confidence = findingConfidence(f.Tier, f.Severity, len(f.CorroboratingLenses))

	err := s.withTx(ctx, func(tx *sql.Tx) error {
		suppressed, err := isSuppressedTx(ctx, tx, f.Fingerprint)
		if err != nil {
			return annotateErr(s.path, "upsert_finding", err)
		}
		if suppressed {
			f.Status = domain.StatusDismissed
		}

		now := nowUTC()

		// Does a row already exist for this fingerprint?
		var existingID, existingCreated, existingFileHash string
		var existingTier domain.Tier
		var existingRepro, existingReproWitness, existingSweptAt sql.NullString
		var existingNeedsHuman int
		var existingNeedsHumanReason string
		err = tx.QueryRowContext(ctx,
			`SELECT id, created_at, tier, repro_path, repro_witness, needs_human, needs_human_reason, file_hash, swept_at FROM findings WHERE fingerprint = ?`, f.Fingerprint,
		).Scan(&existingID, &existingCreated, &existingTier, &existingRepro, &existingReproWitness, &existingNeedsHuman, &existingNeedsHumanReason, &existingFileHash, &existingSweptAt)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			if f.ID == "" {
				f.ID = newID()
			}
			f.CreatedAt = now
			f.UpdatedAt = now
			// Validate FSM invariants for new findings (no stored state to preserve).
			if verr := domain.ValidateFindingState(f); verr != nil {
				return verr
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO findings
				  (id, fingerprint, title, description, reasoning, verdict_detail, severity, tier,
				   status, lens, file, line, commit_sha, file_hash, repro_path, repro_witness,
				   fix_patch, needs_human, needs_human_reason,
				   corroborating_lenses, sites, confidence, created_at, updated_at, swept_at, locus_key)
				VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				f.ID, f.Fingerprint, f.Title, f.Description, f.Reasoning, f.VerdictDetail, f.Severity,
				f.Tier, string(f.Status), f.Lens, f.File, f.Line, f.CommitSHA,
				f.FileHash, nullStr(f.ReproPath), f.ReproWitness, f.FixPatch, boolInt(f.NeedsHuman),
				string(f.NeedsHumanReason),
				encodeLenses(f.CorroboratingLenses), encodeSites(f.Sites), f.Confidence,
				f.CreatedAt.Format(timeLayout), f.UpdatedAt.Format(timeLayout), nullTime(f.SweptAt), f.LocusKey,
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
			//   repro_witness — same preservation rule as repro_path: never clear a
			//                  non-empty stored value with an empty incoming one.
			//
			//   needs_human / needs_human_reason — once set, implicit re-scans must not
			//                 clear them. Explicit mutation paths (promoteFinding,
			//                 patch.go) read-then-upsert with the current row so they
			//                 carry the stored values and are unaffected by this guard.
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
				f.Tier = existingTier
			}
			if f.ReproPath == "" && existingRepro.Valid && existingRepro.String != "" {
				f.ReproPath = existingRepro.String
			}
			if f.ReproWitness == "" && existingReproWitness.Valid && existingReproWitness.String != "" {
				f.ReproWitness = existingReproWitness.String
			}
			if existingNeedsHuman != 0 {
				f.NeedsHuman = true
				// Preserve the stored reason; incoming re-scan never has a reason.
				if f.NeedsHumanReason == domain.NeedsHumanReasonNone {
					f.NeedsHumanReason = domain.NeedsHumanReason(existingNeedsHumanReason)
				}
				// Pre-migration rows: stored reason is '' (migration backfill missed or
				// direct-SQL write). Synthesise a plausible reason so ValidateFindingState
				// invariant (d) does not reject this UPDATE.
				if f.NeedsHumanReason == domain.NeedsHumanReasonNone {
					f.NeedsHumanReason = domain.NeedsHumanReasonBelowQuorum
				}
			}
			// swept_at: preserve across idempotent re-discovery; reset when the
			// anchored code (file_hash) changed so the sweep re-evaluates reachability.
			codeChanged := f.FileHash != existingFileHash
			if !codeChanged && existingSweptAt.Valid && existingSweptAt.String != "" {
				f.SweptAt, _ = parseTime(existingSweptAt.String)
			} else {
				f.SweptAt = time.Time{}
			}
			// repro_attempts: mirror the swept_at reset. A done/abandoned queue row
			// is reset to pending when the anchored code changes so the finding is
			// re-eligible for reproduction (code may now be reproducible).
			// Use tx directly (not s.ResetReproAttemptOnCodeChange) to avoid a
			// deadlock: MaxOpenConns(1) means the outer withTx holds the sole
			// connection; a nested s.db.ExecContext would wait for it forever.
			if codeChanged {
				if _, rerr := tx.ExecContext(ctx, `
					UPDATE repro_attempts
					SET state = 'pending', attempt_count = 0, last_error = '', updated_at = ?
					WHERE fingerprint = ? AND state IN ('done', 'abandoned')`,
					now.Format(timeLayout), f.Fingerprint,
				); rerr != nil {
					// Best-effort: a failed reset is not fatal.
					_ = rerr
				}
			}

			// Recompute confidence after promotion-guard resolution (tier may have changed).
			f.Confidence = findingConfidence(f.Tier, f.Severity, len(f.CorroboratingLenses))

			// Validate FSM invariants after promotion-guard resolution: stored values
			// for NeedsHuman/NeedsHumanReason and ReproPath have been applied, so
			// the struct now reflects the final state that will be written.
			if verr := domain.ValidateFindingState(f); verr != nil {
				return verr
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE findings SET
				  title=?, description=?, reasoning=?, verdict_detail=?, severity=?,
				  tier       = CASE WHEN ? < tier THEN ? ELSE tier END,
				  status=?,
				  lens=?, file=?, line=?, commit_sha=?, file_hash=?,
				  repro_path    = CASE WHEN ? IS NULL THEN repro_path    ELSE ? END,
				  repro_witness = CASE WHEN ? = ''  THEN repro_witness ELSE ? END,
				  fix_patch=?,
				  needs_human = CASE WHEN needs_human = 1 THEN 1 ELSE ? END,
				  needs_human_reason = CASE WHEN needs_human = 1 THEN needs_human_reason ELSE ? END,
				  corroborating_lenses=?, sites=?, confidence=?, updated_at=?, locus_key=?,
				  swept_at = CASE WHEN ? = file_hash THEN swept_at ELSE NULL END
				WHERE id=?`,
				f.Title, f.Description, f.Reasoning, f.VerdictDetail, f.Severity,
				f.Tier, f.Tier,
				string(f.Status),
				f.Lens, f.File, f.Line, f.CommitSHA, f.FileHash,
				nullStr(f.ReproPath), nullStr(f.ReproPath),
				f.ReproWitness, f.ReproWitness,
				f.FixPatch,
				boolInt(f.NeedsHuman),
				string(f.NeedsHumanReason),
				encodeLenses(f.CorroboratingLenses), encodeSites(f.Sites), f.Confidence,
				f.UpdatedAt.Format(timeLayout), f.LocusKey, f.FileHash, f.ID,
			); err != nil {
				return annotateErr(s.path, "upsert_finding", err)
			}
		}
		return nil
	})
	if err != nil {
		return domain.Finding{}, err
	}
	return f, nil
}

// GetFinding returns the finding with the given id, or ErrNotFound.
func (s *Store) GetFinding(ctx context.Context, id string) (domain.Finding, error) {
	return s.queryOne(ctx, `WHERE f.id = ?`, id)
}

// GetFindingByFingerprint returns the finding with the given fingerprint, or
// ErrNotFound.
func (s *Store) GetFindingByFingerprint(ctx context.Context, fingerprint string) (domain.Finding, error) {
	return s.queryOne(ctx, `WHERE f.fingerprint = ?`, fingerprint)
}

// OpenFindingsByLocusKey returns every OPEN finding sharing the lens-independent
// locus key, via the idx_findings_locus_key index. Triage's durable cross-lens
// fold uses it as a per-candidate point-lookup: at a single enclosing-symbol (or
// line-fallback) anchor there are at most a handful of findings, so this is a
// bounded indexed read, not a table scan. An empty result is not an error.
func (s *Store) OpenFindingsByLocusKey(ctx context.Context, locusKey string) ([]domain.Finding, error) {
	return queryRows(ctx, s, "open_findings_by_locus_key",
		findingColumns+findingFrom+` WHERE f.status = 'open' AND f.locus_key = ?`,
		[]any{locusKey}, func(r *sql.Rows) (domain.Finding, error) { return scanFinding(r) })
}

// ListFindings returns findings matching the filter, newest-updated first.
//
// ORDER BY updated_at DESC, id ASC: updated_at is the primary key for "newest";
// id (the unique primary key) is the tiebreaker for findings that share an
// updated_at down to the nanosecond.
func (s *Store) ListFindings(ctx context.Context, filter domain.FindingFilter) ([]domain.Finding, error) {
	var where []string
	var args []any
	if filter.Status != "" {
		where = append(where, "f.status = ?")
		args = append(args, string(filter.Status))
	}
	if filter.HasTier {
		where = append(where, "f.tier = ?")
		args = append(args, int(filter.Tier))
	}
	if filter.CommitSHA != "" {
		where = append(where, "f.commit_sha = ?")
		args = append(args, filter.CommitSHA)
	}
	if filter.Lens != "" {
		where = append(where, "f.lens = ?")
		args = append(args, filter.Lens)
	}

	q := findingColumns + findingFrom
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY f.updated_at DESC, f.id ASC"

	return queryRows(ctx, s, "list_findings", q, args, func(r *sql.Rows) (domain.Finding, error) { return scanFinding(r) })
}

// UpdateStatus sets the status of the finding with the given fingerprint. When
// the new status is StatusDismissed it also records a suppression with the
// supplied reason, so the dismissal is remembered across scans. Returns
// ErrNotFound if no finding has that fingerprint.
//
// reason is only consulted for dismissals; pass "" otherwise.
func (s *Store) UpdateStatus(ctx context.Context, fingerprint string, status domain.Status, reason string) error {
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

		if status == domain.StatusDismissed {
			if err := addSuppressionTx(ctx, tx, fingerprint, reason); err != nil {
				return annotateErr(s.path, "update_status", err)
			}
		}
		return nil
	})
}

// MarkFixed sets the finding's status to StatusFixed.
func (s *Store) MarkFixed(ctx context.Context, fingerprint string) error {
	return s.UpdateStatus(ctx, fingerprint, domain.StatusFixed, "")
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
		var corrob, severity string
		var tier domain.Tier
		err := tx.QueryRowContext(ctx,
			`SELECT corroborating_lenses, tier, severity FROM findings WHERE fingerprint = ?`, fingerprint,
		).Scan(&corrob, &tier, &severity)
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

		// Recompute confidence so the stored column reflects the updated corroboration
		// count — direct SQL consumers read the stored value.
		conf := findingConfidence(tier, domain.Severity(severity), len(merged))

		now := nowUTC()
		if _, err := tx.ExecContext(ctx,
			`UPDATE findings SET corroborating_lenses = ?, confidence = ?, updated_at = ? WHERE fingerprint = ?`,
			encodeLenses(merged), conf, now.Format(timeLayout), fingerprint,
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
func (s *Store) UnsweptOpenFindings(ctx context.Context) ([]domain.Finding, error) {
	return queryRows(ctx, s, "unswept_open_findings",
		findingColumns+findingFrom+" WHERE f.status = 'open' AND f.swept_at IS NULL ORDER BY f.updated_at ASC, f.id ASC",
		nil, func(r *sql.Rows) (domain.Finding, error) { return scanFinding(r) })
}

// AppendFindingSites appends sites to the sites column of the finding identified
// by fingerprint, deduplicating entries with the same (file,line). Used by the
// streaming triage consumer when a root-cause-merged member's primary has already
// been persisted before the member arrived. Returns ErrNotFound when no finding
// with that fingerprint exists; callers treat this as a no-op (primary killed).
// No-op when sites is empty.
func (s *Store) AppendFindingSites(ctx context.Context, fingerprint string, sites []domain.Site) error {
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
		merged := make([]domain.Site, 0, len(existing)+len(sites))
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
// It includes a LEFT JOIN against repro_attempts to populate ReproContradicted
// in a single query, using the unique fingerprint index for O(1) lookup per row.
// The COALESCE(ra.exit_zero_count, 0) column is appended last so scanFinding can
// scan it into ReproContradicted without disturbing the existing column offsets.
const findingColumns = `SELECT f.id, f.fingerprint, f.title, f.description, f.reasoning, f.verdict_detail,
	f.severity, f.tier, f.status, f.lens, f.file, f.line, f.commit_sha, f.file_hash, f.repro_path, f.repro_witness,
	f.fix_patch, f.needs_human, f.needs_human_reason,
	f.corroborating_lenses, f.sites, f.confidence, f.created_at, f.updated_at, f.swept_at, f.locus_key,
	COALESCE(ra.exit_zero_count, 0) AS exit_zero_count`

// findingFrom is the FROM clause that pairs with findingColumns. The LEFT JOIN
// on repro_attempts is safe on all three read paths (queryOne, ListFindings,
// OpenFindingsByLocusKey) because the repro_attempts table may have no row for
// a given fingerprint; the COALESCE above handles the NULL.
const findingFrom = ` FROM findings f LEFT JOIN repro_attempts ra ON ra.fingerprint = f.fingerprint`

func (s *Store) queryOne(ctx context.Context, whereClause string, args ...any) (domain.Finding, error) {
	var f domain.Finding
	err := s.queryRow(ctx, "query_finding", findingColumns+findingFrom+" "+whereClause, args, func(row *sql.Row) error {
		var e error
		f, e = scanFinding(row)
		return e
	})
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Finding{}, ErrNotFound
	}
	if err != nil {
		return domain.Finding{}, err
	}
	return f, nil
}

func scanFinding(sc rowScanner) (domain.Finding, error) {
	var (
		f                    domain.Finding
		status               string
		repro                sql.NullString
		reproWitness         sql.NullString
		needsHuman           int
		needsHumanReason     string
		corrob               string
		sitesStr             string
		createdAt, updatedAt string
		sweptAt              sql.NullString
		exitZeroCount        int
	)
	if err := sc.Scan(
		&f.ID, &f.Fingerprint, &f.Title, &f.Description, &f.Reasoning, &f.VerdictDetail,
		&f.Severity, &f.Tier, &status, &f.Lens, &f.File, &f.Line, &f.CommitSHA,
		&f.FileHash, &repro, &reproWitness, &f.FixPatch, &needsHuman, &needsHumanReason,
		&corrob, &sitesStr, &f.Confidence, &createdAt, &updatedAt, &sweptAt, &f.LocusKey,
		&exitZeroCount,
	); err != nil {
		return domain.Finding{}, err
	}
	f.Status = domain.Status(status)
	f.ReproPath = repro.String
	f.ReproWitness = reproWitness.String
	f.NeedsHuman = needsHuman != 0
	f.NeedsHumanReason = domain.NeedsHumanReason(needsHumanReason)
	f.ReproContradicted = exitZeroCount >= domain.ReproContradictionThreshold
	f.CorroboratingLenses = decodeLenses(corrob)
	f.Sites = decodeSites(sitesStr)
	// Confidence: always recompute at read time so it is consistent with the
	// other fields even for rows written before this bead or by direct SQL.
	f.Confidence = findingConfidence(f.Tier, f.Severity, len(f.CorroboratingLenses))
	var err error
	if f.CreatedAt, err = parseTime(createdAt); err != nil {
		return domain.Finding{}, err
	}
	if f.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return domain.Finding{}, err
	}
	if sweptAt.Valid && sweptAt.String != "" {
		if f.SweptAt, err = parseTime(sweptAt.String); err != nil {
			return domain.Finding{}, err
		}
	}
	return f, nil
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
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

// CountFindings aggregates findings by status and tier in one query for the
// status world-state block.
func (s *Store) CountFindings(ctx context.Context) (domain.FindingTallies, error) {
	t := domain.FindingTallies{OpenByTier: map[int]int{}}
	type tally struct {
		status         string
		tier, n, needs int
	}
	rows, err := queryRows(ctx, s, "count_findings", `
		SELECT status, tier, COUNT(*), COALESCE(SUM(needs_human), 0)
		FROM findings GROUP BY status, tier`, nil, func(r *sql.Rows) (tally, error) {
		var v tally
		if err := r.Scan(&v.status, &v.tier, &v.n, &v.needs); err != nil {
			return tally{}, err
		}
		return v, nil
	})
	if err != nil {
		return domain.FindingTallies{}, err
	}
	for _, v := range rows {
		switch domain.Status(v.status) {
		case domain.StatusOpen:
			t.OpenByTier[v.tier] += v.n
			t.NeedsHuman += v.needs
		case domain.StatusFixed:
			t.Fixed += v.n
		case domain.StatusDismissed:
			t.Dismissed += v.n
		default:
			// StatusSuperseded (and any future states) intentionally omitted
			// from the pane: nothing writes superseded today, and the tallies
			// show actionable lifecycle states only.
		}
	}
	return t, nil
}
