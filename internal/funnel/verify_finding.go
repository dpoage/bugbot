package funnel

import (
	"context"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/ingest"
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
	// Deliberately read-only: re-verification never gets the sandbox_exec tool
	// even when SandboxOpts is enabled. The daemon re-verifies every open
	// finding on every code change, so sandbox runs here would multiply
	// container spend per cycle; empirical evidence belongs to the initial
	// verify (and repro) stages. Pinned by TestVerifyFinding_NoSandboxTool.
	//
	// post_lead is also absent: refuter independence is the mechanism that kills
	// false positives, and re-verification is a pure adversarial check (same
	// principle as the main verify stage). See verify.go for the fuller rationale.
	tools, err := f.readOnlyTools(agent.ReadCaps{})
	if err != nil {
		return false, "", err
	}

	c := candidateFromFinding(fnd)

	n := f.opts.Limits.Refuters
	if n <= 0 {
		n = DefaultRefuters
	}

	// No shared budget pool here: re-verifying a single finding is a standalone,
	// cheap operation the daemon gates at the cycle level. A pool-less
	// budgetState makes runnerLimits a pass-through and never triggers a
	// budget-pool stop.
	// Re-verification has no snapshot to derive a repo-wide persona from, so the
	// persona is keyed off the finding's own file language — the relevant signal
	// for a single-finding refute (matches the repro stage's per-finding choice).
	// No qualifiers here: re-verification operates on a single stored finding
	// without a repo root, so dialect detection (e.g. C++ standard) is skipped.
	persona := ingest.PersonaLanguages([]ingest.Language{ingest.DetectLanguage(fnd.File)}, nil)

	verdicts, seatNames, _, _, _, err := f.runRefuters(ctx, f.clients.Verifier, tools, persona, c, n, &budgetState{})
	if err != nil {
		return false, "", err
	}
	// VerifyFinding uses majority rule only — no arbiter on re-verification.
	// Re-verifying a single stored finding is a cheap daemon cycle operation;
	// adding an arbiter here would double the cost on every re-check. The
	// majority rule is the conservative default that already existed before the
	// split-panel arbiter was introduced for initial verification.
	//
	// Two DELIBERATE asymmetries vs initial verification follow from that:
	// (1) re-verification DOES get the seat-diverse prompts (n flows through the
	// same runRefuters, so the panel attacks from distinct angles — strictly
	// better evidence for the same spend); (2) a split panel that initial
	// verification would send to an arbiter is decided by majority here, making
	// re-verification more willing to demote on a 2-refute split. That bias is
	// intentional: a re-check happens because the finding's code changed or went
	// stale, where demoting and letting a future scan re-discover is the cheap,
	// safe failure mode.
	return majorityRefuted(verdicts), buildReasoning(verdicts, seatNames, "", false), nil
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
