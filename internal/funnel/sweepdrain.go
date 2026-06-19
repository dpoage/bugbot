package funnel

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/store"
)

// SweepDrain re-ranks unswept open findings (this run's + any stranded by an
// interrupted/older run) by reachability/impact. Idempotent: swept findings
// are excluded by UnsweptOpenFindings; a second drain over already-swept rows
// is a no-op.
func (f *Funnel) SweepDrain(ctx context.Context) (*Result, error) {
	findings, err := f.store.UnsweptOpenFindings(ctx)
	if err != nil {
		return nil, fmt.Errorf("sweep drain: UnsweptOpenFindings: %w", err)
	}
	result := &Result{}
	if len(findings) == 0 {
		return result, nil
	}

	scanRunID, err := f.store.BeginScanRun(ctx, store.ScanImpactSweep, "")
	if err != nil {
		return nil, fmt.Errorf("sweep drain: BeginScanRun: %w", err)
	}
	result.ScanRunID = scanRunID

	rec := &spendRecorder{ctx: ctx, store: f.store, scanRunID: scanRunID}
	verifierClient := llm.WithRecorder(f.clients.Verifier, rec, roleVerifier, "", "")
	cacheWeight := f.opts.Budget.CacheReadBudgetWeight
	budget := newBudgetState(f.opts.Budget.TokenBudget, rec, cacheWeight)

	// Interrupt-safe finalization: seal the scan_runs row on every exit path.
	finalize := func(s *Stats) {
		statsJSON, merr := json.Marshal(s)
		if merr != nil {
			statsJSON = []byte(`{"aborted":true}`)
		}
		fCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if ferr := f.store.FinishScanRun(fCtx, scanRunID, string(statsJSON)); ferr != nil {
			f.note(result, fmt.Sprintf("sweep drain: FinishScanRun failed: %v", ferr))
		}
	}
	defer func() { finalize(&result.Stats) }()

	f.impactSweep(ctx, findings, f.repo.Root(), verifierClient, budget.stopped.Load(), result)
	result.Findings = findings // severities re-ranked in-place by impactSweep

	return result, nil
}
