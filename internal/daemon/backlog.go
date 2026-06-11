package daemon

import (
	"context"

	"github.com/dpoage/bugbot/internal/store"
)

// OpenBacklog returns open findings that have not yet had a reproduction
// attempt: Tier 2 or 3, ReproPath empty, NeedsHuman false. It queries all open
// findings from the store (the store filter does not support ReproPath or
// NeedsHuman predicates) and filters in Go.
//
// Findings are returned in store-default order (newest-updated-first). Callers
// that want a bounded set should slice the result; the caller is responsible for
// the batch-size cap so this helper stays reusable.
func OpenBacklog(ctx context.Context, st *store.Store) ([]store.Finding, error) {
	all, err := st.ListFindings(ctx, store.FindingFilter{Status: store.StatusOpen})
	if err != nil {
		return nil, err
	}
	out := make([]store.Finding, 0, len(all))
	for _, f := range all {
		if (f.Tier == 2 || f.Tier == 3) && f.ReproPath == "" && !f.NeedsHuman {
			out = append(out, f)
		}
	}
	return out, nil
}

// runReproBacklog is the backlog-timer step: it queries open T2/T3 findings
// with no repro attempt, caps the batch to cfg.ReproBacklogBatch, and runs
// them through PromoteAll. It is a no-op unless EnableRepro is true and a
// Promoter is wired in. The day-budget gate is applied by the caller (Run).
func (d *Daemon) runReproBacklog(ctx context.Context) {
	if !d.cfg.EnableRepro || d.repro == nil {
		return
	}

	backlog, err := OpenBacklog(ctx, d.store)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		d.log.Error("daemon: repro backlog query failed", "err", err)
		return
	}
	if len(backlog) == 0 {
		return // nothing to do; no log spam on empty backlog
	}

	// Cap to the configured batch size.
	batch := backlog
	if len(batch) > d.cfg.ReproBacklogBatch {
		batch = batch[:d.cfg.ReproBacklogBatch]
	}

	// Attribute backlog spend to an empty scan-run id. Backlog findings span
	// multiple past runs, so there is no single run to attribute to; an empty
	// id is correct for day-budget totals (TotalsSince does not filter by run)
	// and avoids polluting per-scan-run spend rows with cross-run aggregates.
	if d.reproTag != nil {
		d.reproTag.SetScanRun("")
	}

	d.log.Info("daemon: repro backlog drain: starting",
		"eligible", len(backlog),
		"batch", len(batch),
	)

	summary, err := d.repro.PromoteAll(ctx, d.store, batch)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		d.log.Error("daemon: repro backlog promotion failed", "err", err)
		return
	}

	d.log.Info("daemon: repro backlog drain: complete",
		"attempted", summary.Attempted,
		"promoted", summary.Promoted,
	)
}
