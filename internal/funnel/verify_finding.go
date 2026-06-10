package funnel

import (
	"context"

	"github.com/dpoage/bugbot/internal/store"
)

// VerifyFinding re-runs adversarial verification against a single already-stored
// finding, returning whether the refuters now reach a majority "refuted" verdict
// and the verification trace either way.
//
// It is the minimal seam the daemon uses to re-verify findings whose underlying
// code changed since they were recorded: rather than re-discover the bug, it
// reconstructs the verification candidate from the stored finding and reuses the
// exact refuter machinery the Verify stage uses (runRefuters + majorityRefuted).
// The default refuter count is used; a single re-verification is cheap and the
// daemon gates the whole cycle on its token budget, so no per-call degradation
// is applied here.
//
// refuted == true means the refuters now agree the reported bug is wrong against
// the current code (the daemon treats that as "auto-close: fixed"). reasoning is
// the human-legible refuter trace, suitable for recording on the finding.
func (f *Funnel) VerifyFinding(ctx context.Context, fnd store.Finding) (refuted bool, reasoning string, err error) {
	tools, err := f.readOnlyTools()
	if err != nil {
		return false, "", err
	}

	c := candidateFromFinding(fnd)

	n := f.opts.Refuters
	if n <= 0 {
		n = DefaultRefuters
	}

	// No shared budget pool here: re-verifying a single finding is a standalone,
	// cheap operation the daemon gates at the cycle level. A pool-less
	// budgetState makes runnerLimits a pass-through and never triggers a
	// budget-pool stop.
	verdicts, _, _, _, err := f.runRefuters(ctx, f.clients.Verifier, tools, c, n, &budgetState{})
	if err != nil {
		return false, "", err
	}
	return majorityRefuted(verdicts), buildReasoning(verdicts), nil
}

// candidateFromFinding reconstructs the verification Candidate from a stored
// finding so the refuter prompt sees the same shape the Verify stage produced.
// Evidence is sourced from the finding's reasoning (its prior verification
// trace), which is the closest stored analogue to a finder's evidence.
func candidateFromFinding(fnd store.Finding) Candidate {
	return Candidate{
		Lens:        fnd.Lens,
		File:        fnd.File,
		Line:        fnd.Line,
		Title:       fnd.Title,
		Description: fnd.Description,
		Severity:    fnd.Severity,
		Evidence:    fnd.Reasoning,
		Fingerprint: fnd.Fingerprint,
	}
}
