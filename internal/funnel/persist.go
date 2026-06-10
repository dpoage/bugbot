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

// tierSuspected is the tier for candidates that passed triage but never
// completed adversarial verification because the run hit its hard budget. They
// are persisted (not dropped) so a human can review them, but at lower
// confidence than a verified finding.
const tierSuspected = 3

// budgetStoppedReasoning is the verification trace recorded on a Tier 3
// suspected finding, making clear why it was not verified.
const budgetStoppedReasoning = "Verification skipped: the run reached its hard token budget before this candidate " +
	"could be challenged by refuters. It is recorded as Tier 3 suspected (unverified) so it is not silently " +
	"dropped. Re-run the scan with more budget to verify it."

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

// persistSuspected upserts budget-orphaned candidates as open Tier 3 suspected
// findings. These passed triage but their adversarial verification was skipped
// or cut short when the run hit its hard token budget, so they are recorded at
// lower confidence rather than dropped — surfacing them in the report for human
// review. Like persist, it honors suppression memory (a forced-dismissed
// finding is omitted from the returned set).
func (f *Funnel) persistSuspected(ctx context.Context, orphans []Candidate, commit string, fps map[string]string) ([]store.Finding, error) {
	out := make([]store.Finding, 0, len(orphans))
	for _, c := range orphans {
		finding := store.Finding{
			Fingerprint: c.Fingerprint,
			Title:       c.Title,
			Description: c.Description,
			Reasoning:   budgetStoppedReasoning,
			Severity:    c.Severity,
			Tier:        tierSuspected,
			Status:      store.StatusOpen,
			Lens:        c.Lens,
			File:        c.File,
			Line:        c.Line,
			CommitSHA:   commit,
			FileHash:    fps[c.File],
		}
		stored, err := f.store.UpsertFinding(ctx, finding)
		if err != nil {
			return nil, fmt.Errorf("funnel: upsert suspected finding %q: %w", c.Title, err)
		}
		if stored.Status != store.StatusOpen {
			continue
		}
		out = append(out, stored)
	}
	return out, nil
}
