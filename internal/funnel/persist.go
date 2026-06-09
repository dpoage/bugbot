package funnel

import (
	"context"
	"fmt"

	"github.com/dpoage/bugbot/internal/store"
)

// tierVerified is the confidence tier assigned to candidates that survive
// adversarial verification but have not been reproduced. Tier 1 requires a
// sandboxed failing test (a later stage); this stage tops out at Tier 2.
const tierVerified = 2

// persist upserts each verified survivor into the store as an open Tier 2
// finding, anchored to the snapshot commit and the file's content hash so the
// daemon can later detect when the underlying code changed. It returns the
// stored rows.
//
// UpsertFinding owns the final status decision: a fingerprint that was ever
// suppressed is stored as dismissed regardless of the open status requested
// here. Triage already drops suppressed candidates, so this is a belt-and-
// suspenders guarantee rather than the primary suppression path.
func (f *Funnel) persist(ctx context.Context, survivors []verified, commit string, fps map[string]string) ([]store.Finding, error) {
	out := make([]store.Finding, 0, len(survivors))
	for _, s := range survivors {
		c := s.cand
		finding := store.Finding{
			Fingerprint: c.Fingerprint,
			Title:       c.Title,
			Description: c.Description,
			Reasoning:   s.reasoning,
			Severity:    c.Severity,
			Tier:        tierVerified,
			Status:      store.StatusOpen,
			Lens:        c.Lens,
			File:        c.File,
			Line:        c.Line,
			CommitSHA:   commit,
			FileHash:    fps[c.File],
		}
		stored, err := f.store.UpsertFinding(ctx, finding)
		if err != nil {
			return nil, fmt.Errorf("funnel: upsert finding %q: %w", c.Title, err)
		}
		// A finding forced to dismissed by suppression memory must not be reported
		// as an open survivor; skip it from the returned set.
		if stored.Status != store.StatusOpen {
			continue
		}
		out = append(out, stored)
	}
	return out, nil
}
