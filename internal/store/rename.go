package store

import (
	"context"
	"database/sql"

	"github.com/dpoage/bugbot/internal/domain"
)

// RenameFindingIdentity rewrites the stored identity of every finding whose
// file matches oldFile to reflect newFile: the file column, the fingerprint,
// and the locus_key are recomputed via domain.Fingerprint/domain.LocusKey (the
// same identity helpers a live scan calls), using resolve to re-derive the
// enclosing-symbol locus at the finding's stored line under its new path.
// resolve is normally funnel.NewLocusResolver(repoRoot).Resolve — accepted as
// a plain func so the store package stays free of a dependency on
// funnel/treesitter.
//
// Path participates in Fingerprint and LocusKey (internal/domain), so without
// this a git rename silently mints a fresh identity for unchanged code: the
// old row lingers as an open finding no one ever revisits and any suppression
// on it stops applying, so a re-discovered (but never actually re-introduced)
// bug resurfaces and republishes.
//
// The finding's suppression, repro-attempt queue, and published-issue rows are
// carried forward onto the new fingerprint in the same transaction, so a
// renamed file keeps its dismissal (suppression memory), its
// repro-contradicted signal (repro_attempts join in findingColumns), and its
// GitHub issue link (published_issues) intact rather than orphaning them under
// a fingerprint nothing will ever look up again.
//
// Idempotent by construction: it selects rows WHERE file = oldFile, and once a
// row is rewritten its file column no longer equals oldFile, so a second call
// with the same (oldFile, newFile) pair matches zero rows and is a no-op. This
// is what backs safe watermark crash-replay (bugbot-r4x3): the daemon may
// re-run rename detection over the same commit range after an interrupted
// cycle without double-hashing or erroring.
//
// Returns the number of findings rewritten.
func (s *Store) RenameFindingIdentity(ctx context.Context, oldFile, newFile string, resolve func(file string, line int) string) (int, error) {
	if oldFile == "" || newFile == "" || oldFile == newFile {
		return 0, nil
	}
	n := 0
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		rows, qerr := tx.QueryContext(ctx,
			`SELECT id, fingerprint, lens, line FROM findings WHERE file = ?`, oldFile)
		if qerr != nil {
			return qerr
		}
		type match struct {
			id, fingerprint, lens string
			line                  int
		}
		var matches []match
		for rows.Next() {
			var m match
			if serr := rows.Scan(&m.id, &m.fingerprint, &m.lens, &m.line); serr != nil {
				rows.Close()
				return serr
			}
			matches = append(matches, m)
		}
		if rerr := rows.Err(); rerr != nil {
			rows.Close()
			return rerr
		}
		rows.Close()

		now := nowUTC().Format(timeLayout)
		for _, m := range matches {
			locus := resolve(newFile, m.line)
			newFP := domain.Fingerprint(m.lens, newFile, locus)
			newLK := domain.LocusKey(newFile, locus)

			if _, err := tx.ExecContext(ctx,
				`UPDATE findings SET file = ?, fingerprint = ?, locus_key = ?, updated_at = ? WHERE id = ?`,
				newFile, newFP, newLK, now, m.id,
			); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE suppressions SET fingerprint = ? WHERE fingerprint = ?`,
				newFP, m.fingerprint,
			); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE repro_attempts SET fingerprint = ? WHERE fingerprint = ?`,
				newFP, m.fingerprint,
			); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE published_issues SET fingerprint = ? WHERE fingerprint = ?`,
				newFP, m.fingerprint,
			); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	if err != nil {
		return 0, annotateErr(s.path, "rename_finding_identity", err)
	}
	return n, nil
}
