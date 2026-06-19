package funnel

import (
	"context"

	"github.com/dpoage/bugbot/internal/store"
)

// VerifyDrain verifies all pending_candidates left by interrupted runs WITHOUT
// invoking the finder/cartographer. ListPendingCandidates -> replay -> triage
// re-anchor -> runVerifyAndPersist -> DeletePendingCandidate (all via run()).
//
// The oracle-required pre-check (oracle #4): list pending BEFORE taking the
// full-repo snapshot so an empty-WAL drain is a single store query. This
// mirrors the OpenBacklog-first pattern of the repro template and is important
// because the daemon fires VerifyDrain every postCycle + hourly.
func (f *Funnel) VerifyDrain(ctx context.Context) (*Result, error) {
	pending, err := f.store.ListPendingCandidates(ctx)
	if err != nil {
		return nil, err
	}
	if len(pending) == 0 {
		return &Result{}, nil
	}
	// triage re-anchor needs the current snapshot (scope/dedup checks, file
	// presence verification, fingerprint anchoring).
	snap, err := f.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	// targets=nil, fps=nil: run() recomputes fps at fingerprint step for file_hash
	// anchoring; nil targets means the snapshot drives scope (but in modeVerifyDrain
	// the finder stage is skipped, so targets is unused beyond re-anchor).
	// touchCoverage=false: no finder units run, so no coverage to stamp.
	return f.run(ctx, store.ScanVerifyDrain, snap, nil, nil, false, modeVerifyDrain)
}
