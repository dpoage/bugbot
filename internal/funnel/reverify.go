package funnel

import (
	"context"

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
	susp, err := f.store.ListFindings(ctx, store.FindingFilter{
		Status: store.StatusOpen,
		Tier:   int(domain.TierSuspected),
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
