package store

import (
	"context"
	"database/sql"
)

// LensStat is the aggregate survival/refutation/repro picture for one lens
// (defect family) across all stored data. It is computed from three tables:
//   - findings: surviving open findings (Survived) and reproduced findings (Reprod).
//   - dead_hypotheses: verifier-killed candidates (Killed).
//   - repro_attempts: exit-zero outcomes (Contradicted).
//
// SurvivalRate = Survived / (Survived + Killed), the fraction of candidates
// this lens reported that passed verifier scrutiny. A lens with a low rate
// finds many things the verifier kills — high false-positive rate.
//
// ReproRate = Reprod / Survived (0 when Survived == 0), the fraction of
// surviving findings that were successfully reproduced (Tier <= 1).
//
// ContradictedCount is the number of surviving findings for this lens whose
// repro test ran >= ReproContradictionThreshold times and exited 0 each time —
// disconfirming evidence that the bug manifests.
type LensStat struct {
	Lens              string
	Survived          int // findings that survived verifier (status open, any tier)
	Killed            int // verifier-killed candidates from dead_hypotheses
	Reprod            int // findings reproduced (tier <= 1, i.e. T0 or T1)
	ContradictedCount int // surviving findings with exit_zero_count >= threshold
}

// SurvivalRate is Survived / (Survived + Killed). Returns 0 when both are 0.
func (l LensStat) SurvivalRate() float64 {
	total := l.Survived + l.Killed
	if total == 0 {
		return 0
	}
	return float64(l.Survived) / float64(total)
}

// ReproRate is Reprod / Survived. Returns 0 when Survived is 0.
func (l LensStat) ReproRate() float64 {
	if l.Survived == 0 {
		return 0
	}
	return float64(l.Reprod) / float64(l.Survived)
}

// LensMetrics returns per-lens survival/refutation/repro statistics computed
// from stored data. It joins three tables:
//
//   - findings (for survived + reproduced counts, keyed by lens)
//   - dead_hypotheses (for killed counts, keyed by lens)
//   - repro_attempts (for exit-zero contradiction counts, joined via fingerprint)
//
// Lenses that appear only in dead_hypotheses (never survived) are included so
// operators can see lenses that are pure false-positive generators. Results are
// ordered by total candidates (survived+killed) descending, so the most active
// lenses appear first.
//
// Only open findings are counted as survived (StatusOpen). Fixed and dismissed
// findings are not included in the survival count — they represent resolved
// signal, not an unresolved finding the lens is "right" about.
func (s *Store) LensMetrics(ctx context.Context) ([]LensStat, error) {
	// SQLite does not support FULL OUTER JOIN prior to 3.39; the modernc.org/sqlite
	// driver ships an older version. Use a UNION ALL of LEFT JOIN + anti-join to
	// achieve the equivalent: the first branch covers lenses in finding_stats
	// (with or without a kill_stats peer); the second branch covers lenses that
	// appear ONLY in kill_stats (never survived the verifier).
	const qCompat = `
		WITH finding_stats AS (
			SELECT
				f.lens,
				COUNT(*) AS survived,
				SUM(CASE WHEN f.tier <= 1 THEN 1 ELSE 0 END) AS reprod,
				SUM(CASE WHEN ra.exit_zero_count >= ? THEN 1 ELSE 0 END) AS contradicted
			FROM findings f
			LEFT JOIN repro_attempts ra ON ra.fingerprint = f.fingerprint
			WHERE f.status = ?
			GROUP BY f.lens
		),
		kill_stats AS (
			SELECT lens, COUNT(*) AS killed
			FROM dead_hypotheses
			GROUP BY lens
		),
		combined AS (
			SELECT fs.lens, fs.survived, fs.reprod, fs.contradicted, COALESCE(ks.killed, 0) AS killed
			FROM finding_stats fs
			LEFT JOIN kill_stats ks ON fs.lens = ks.lens
			UNION ALL
			SELECT ks.lens, 0, 0, 0, ks.killed
			FROM kill_stats ks
			WHERE ks.lens NOT IN (SELECT lens FROM finding_stats)
		)
		SELECT lens, survived, reprod, contradicted, killed
		FROM combined
		ORDER BY (survived + killed) DESC, lens ASC`

	return queryRows(ctx, s, "lens_metrics", qCompat,
		[]any{ReproContradictionThreshold, string(StatusOpen)},
		func(r *sql.Rows) (LensStat, error) {
			var ls LensStat
			return ls, r.Scan(&ls.Lens, &ls.Survived, &ls.Reprod, &ls.ContradictedCount, &ls.Killed)
		},
	)
}
