package funnel

import (
	"context"

	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/store"
)

// triage applies the cheap, deterministic kills before the expensive verify
// stage, in this order:
//
//  1. Drop candidates whose confidence is "low" (the finder itself is unsure).
//  2. Drop candidates pointing at files outside the snapshot (hallucinated or
//     stale paths — there is nothing to anchor or verify).
//  3. Compute each survivor's store fingerprint and drop within-run duplicates
//     (two lenses reporting the EXACT same bug — identical fingerprint —
//     collapse to one).
//  4. Drop fingerprints the maintainers have already suppressed.
//
// Then, on the survivors of the above, it applies a fifth pass:
//
//  5. Location-based cross-lens dedup: cluster survivors by file + line
//     proximity (ignoring lens and title) and keep only each cluster's primary,
//     recording the other members' lenses on it as corroboration. This catches
//     the same genuine defect reported by different lenses with DIFFERENT titles
//     (and hence different fingerprints), so it survives step 3's exact-match
//     dedup but is still one bug. Only the primary proceeds to verification, so
//     one refuter panel runs per cluster. See mergeClusters.
//
// The order matters for the stats breakdown: a low-confidence candidate is
// counted only as low-confidence, an out-of-scope one only as out-of-scope,
// etc., so the drop counters partition the losses without double-counting. The
// merge counters (MergedWithinLens / MergedCrossLens) are separate from
// DroppedDuplicate: a merged member has a DIFFERENT fingerprint that points at
// the same underlying bug, whereas a dropped duplicate is an exact-fingerprint
// match.
func (f *Funnel) triage(ctx context.Context, candidates []Candidate, snap *ingest.Snapshot, stats *Stats) ([]Candidate, error) {
	inScope := make(map[string]bool, len(snap.Files))
	for _, file := range snap.Files {
		inScope[file.Path] = true
	}

	seen := make(map[string]bool, len(candidates))
	var survivors []Candidate

	for _, c := range candidates {
		if c.Confidence == "low" {
			stats.DroppedLowConfidence++
			continue
		}
		if !inScope[c.File] {
			stats.DroppedOutOfScope++
			continue
		}

		fp := store.Fingerprint(c.Lens, c.File, c.Line, c.Title)
		if seen[fp] {
			stats.DroppedDuplicate++
			continue
		}

		suppressed, err := f.store.IsSuppressed(ctx, fp)
		if err != nil {
			return nil, err
		}
		if suppressed {
			stats.DroppedSuppressed++
			// Mark seen so a later duplicate of a suppressed fingerprint is not
			// re-counted as a separate suppression.
			seen[fp] = true
			continue
		}

		seen[fp] = true
		c.Fingerprint = fp
		survivors = append(survivors, c)
	}

	// Fifth pass: collapse survivors that point at the same underlying defect
	// (same file, nearby line) but slipped past the exact-fingerprint dedup
	// because their lens/title — and therefore fingerprint — differ. Only each
	// cluster's primary proceeds to verification.
	survivors = mergeClusters(survivors, DefaultMergeWindow, stats)

	return survivors, nil
}
