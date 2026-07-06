// run_sweep.go holds the sweep anti-starvation ordering helpers extracted from run.go for readability.
// Pure code motion: no logic changes.

package funnel

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/store"
)

// applySweepOrder reorders targets in-place using the anti-starvation two-group
// scheme:
//
//   - Group 1 (needs-scan): files absent from watermarks, files whose
//     timestamp equals the epoch sentinel (never actually scanned), and files
//     whose current fingerprint differs from the stored content hash (changed
//     since their last scan). Group 1 is heat-ordered within the group, so a
//     fresh commit's churned files still lead the sweep.
//   - Group 2 (clean): all other files (previously scanned, content
//     unchanged). Sorted by last_scanned_at ascending (stalest first) so the
//     run always picks up the files that were scanned longest ago.
//
// Group 1 precedes Group 2 in the output. Convergence property: a
// budget-truncated sweep covers group 1 plus the head of group 2; covered
// files get a fresh last_scanned_at and rotate to the back of group 2 next
// sweep, so repeated truncated sweeps over an unchanged repo rotate through
// the full set instead of fixating on a hot head.
//
// Returns (neverScanned, changedSinceScan, heatActuallyReordered):
//   - neverScanned: count of group-1 files with no row / epoch timestamp.
//   - changedSinceScan: count of group-1 files admitted by the hash mismatch
//     (scanned before, content changed since).
//   - heatActuallyReordered: true if the heat map produced a non-trivial
//     reordering within group 1.
func applySweepOrder(targets []string, heat map[string]float64, fps map[string]string, watermarks map[string]store.Watermark) (neverScanned, changedSinceScan int, heatActuallyReordered bool) {
	var group1, group2 []string
	for _, t := range targets {
		wm, ok := watermarks[t]
		switch {
		case !ok || wm.LastScannedAt.Equal(epochSentinelParsed):
			neverScanned++
			group1 = append(group1, t)
		case fps[t] != wm.ContentHash:
			changedSinceScan++
			group1 = append(group1, t)
		default:
			group2 = append(group2, t)
		}
	}

	// Group 1: heat-ordered (highest heat first; alphabetical tiebreak).
	g1Before := make([]string, len(group1))
	copy(g1Before, group1)
	sort.SliceStable(group1, func(i, j int) bool {
		hi, hj := heat[group1[i]], heat[group1[j]]
		if hi != hj {
			return hi > hj
		}
		return group1[i] < group1[j]
	})
	for i := range group1 {
		if group1[i] != g1Before[i] {
			heatActuallyReordered = true
			break
		}
	}

	// Group 2: stalest first (ascending last_scanned_at).
	sort.SliceStable(group2, func(i, j int) bool {
		ti, tj := watermarks[group2[i]].LastScannedAt, watermarks[group2[j]].LastScannedAt
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return group2[i] < group2[j]
	})

	// Write the two groups back into targets in-place.
	copy(targets, group1)
	copy(targets[len(group1):], group2)

	return neverScanned, changedSinceScan, heatActuallyReordered
}

// orderSweepTargets applies the Sweep anti-starvation ordering to targets IN
// PLACE (heat-ordered when enabled and data is available; unchanged otherwise)
// and returns the bookkeeping the caller needs for its progress summary. It is
// shared by Funnel.Sweep and Funnel.EstimateScan so both order identically: the
// finder-unit count depends on the post-ordering chunk packing, so any
// divergence would make the estimate drift from the real run on polyglot repos.
func (f *Funnel) orderSweepTargets(ctx context.Context, targets []string, fps map[string]string, fpsErr error) (heat map[string]float64, heatFiles, neverScanned, changedSinceScan int, heatOrdered bool) {
	if f.opts.Features.DisableHeatOrdering {
		return nil, 0, 0, 0, false
	}
	heat, heatErr := ingest.ChurnHeat(ctx, f.repo.Root(), 0)

	// watermarks is a best-effort read; fall back to pure heat if it fails.
	var watermarks map[string]store.Watermark
	if fpsErr == nil {
		paths := make([]string, 0, len(fps))
		for p := range fps {
			paths = append(paths, p)
		}
		watermarks, _ = f.store.ScanWatermarks(ctx, paths)
	}
	if heatErr == nil && len(heat) > 0 {
		heatFiles = len(heat)
	}
	if fpsErr == nil && watermarks != nil {
		neverScanned, changedSinceScan, heatOrdered = applySweepOrder(targets, heat, fps, watermarks)
	} else if heatErr == nil && len(heat) > 0 {
		heatOrdered = applyHeatOrder(targets, heat)
	}
	return heat, heatFiles, neverScanned, changedSinceScan, heatOrdered
}

// applyHeatOrder sorts targets in-place by heat score descending, with
// equal-heat (including zero-heat) files sorted alphabetically as a tiebreak.
// It returns true if the ordering differs from the input (meaning the heat map
// actually reordered something), so callers can decide whether to log.
func applyHeatOrder(targets []string, heat map[string]float64) bool {
	// Snapshot the original order to detect actual reordering.
	original := make([]string, len(targets))
	copy(original, targets)

	sort.SliceStable(targets, func(i, j int) bool {
		hi, hj := heat[targets[i]], heat[targets[j]]
		if hi != hj {
			return hi > hj // higher heat first
		}
		return targets[i] < targets[j] // alphabetical tiebreak
	})

	for i := range targets {
		if targets[i] != original[i] {
			return true
		}
	}
	return false
}

// heatTop5 returns a human-readable summary of the top 5 hottest targets,
// formatted as "path:score" pairs joined by spaces, for use in progress events.
func heatTop5(targets []string, heat map[string]float64) string {
	n := 5
	if len(targets) < n {
		n = len(targets)
	}
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(' ')
		}
		p := targets[i]
		fmt.Fprintf(&b, "%s:%.3f", p, heat[p])
	}
	return b.String()
}
