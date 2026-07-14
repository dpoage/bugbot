package funnel

import (
	"context"
	"fmt"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/store"
)

// ReverifySuspected re-runs the verifier on every OPEN Tier-3 suspected finding
// WITHOUT invoking the finder/cartographer. It exists because the hard-budget
// stop in verify_stream.go (~line 66-91) and the no-verdict-panel orphan path
// (~line 220-267) persist their candidates as Tier-3 suspected AND delete the
// corresponding pending_candidates WAL row, leaving an orphan that VerifyDrain
// (which only reads pending_candidates) can never re-process. Absent this
// re-verify path, the only way to re-judge those findings is a full re-scan
// that relies on the finder to rediscover the same defect — fragile, and a
// real precision loss when the orphan is exactly the kind of stale T3 a
// human would otherwise want re-confirmed.
//
// Flow:
//
//  1. ListFindings(OPEN, Tier=Suspected) → the durable T3 orphans.
//  2. Snapshot the repo (triage re-anchors each candidate to current code).
//  3. Replay them through run() in modeReverify: triage re-anchors, verify
//     re-judges, persist promotes survivors to Tier 2, killed candidates
//     transition their durable row to StatusDismissed (see Candidate.Reverify
//     and the verify-stream KILL region).
//
// Best-effort single store query up front: an empty set is a no-op (returns
// an empty Result). Uses ScanVerifyDrain as the scan kind — the underlying
// mode is modeReverify, but the kind label "verify-drain" already means
// "verify-only pass, no finder" and is the closest existing bucket. No new
// ScanKind is introduced.
func (f *Funnel) ReverifySuspected(ctx context.Context) (*Result, error) {
	susp, err := f.store.ListFindings(ctx, domain.FindingFilter{
		Status:  domain.StatusOpen,
		HasTier: true,
		Tier:    domain.TierSuspected,
	})
	if err != nil {
		return nil, err
	}
	if len(susp) == 0 {
		return &Result{}, nil
	}
	snap, err := f.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	// targets=nil, fps=nil: run() recomputes fps at fingerprint step for
	// file_hash anchoring; nil targets means the snapshot drives scope (in
	// modeReverify the finder stage is skipped, so targets is unused beyond
	// re-anchor). touchCoverage=false: no finder units run, so no coverage to
	// stamp.
	return f.run(ctx, store.ScanVerifyDrain, snap, nil, nil, false, modeReverify)
}

// ReverifyUnderValidated re-runs the verifier on every OPEN Tier-2 finding
// whose recorded genuine-verdict count is below MinReviewerValidation, WITHOUT
// invoking the finder/cartographer. It exists because budget degradation
// shrinks the refuter panel to a single seat (degradedRefuters): a survivor of
// such a panel is persisted as a normal open T2 finding even though only one
// reviewer actually judged it — belowQuorum(1,1) is false, so it is not even
// flagged NeedsHuman. This drain is the later, fresh-budget pass that re-judges
// those findings with a full panel until each has been validated by at least
// MinReviewerValidation genuine reviewer verdicts (findings.genuine_verdicts is
// monotone per file_hash, so a degraded rerun never regresses the count and the
// eligible set only shrinks).
//
// Survivors are re-persisted through the normal verify persist path: the new
// panel's genuine-verdict count is recorded, severity is re-ranked inline
// (validateSeverityInline), and a stale below-quorum NeedsHuman flag is
// released when the new panel meets quorum. Refuted findings are dismissed via
// the Candidate.Reverify kill path, exactly like ReverifySuspected.
//
// When Options.Limits.Refuters is explicitly configured below
// MinReviewerValidation the drain is a deliberate no-op: every rerun would
// record a count that can never reach the minimum, re-spending on the same
// findings every invocation without converging.
//
// Pre-migration rows (genuine_verdicts = 0, panel size unknown) are eligible:
// one full-panel pass records a real count and they drop out.
func (f *Funnel) ReverifyUnderValidated(ctx context.Context) (*Result, error) {
	if n := f.opts.Limits.Refuters; n > 0 && n < MinReviewerValidation {
		res := &Result{}
		f.note(res, fmt.Sprintf(
			"revalidate: skipped — configured refuters (%d) below the %d-reviewer minimum; the drain could never converge", n, MinReviewerValidation))
		return res, nil
	}
	under, err := f.store.UnderValidatedOpenFindings(ctx, MinReviewerValidation)
	if err != nil {
		return nil, err
	}
	if len(under) == 0 {
		return &Result{}, nil
	}
	snap, err := f.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	// Same shape as ReverifySuspected: targets=nil, fps=nil (run() recomputes
	// fps for file_hash anchoring), touchCoverage=false (no finder units run),
	// and ScanVerifyDrain as the closest existing kind label.
	return f.run(ctx, store.ScanVerifyDrain, snap, nil, nil, false, modeRevalidate)
}
