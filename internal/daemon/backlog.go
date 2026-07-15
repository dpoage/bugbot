package daemon

import (
	"context"
	"sort"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/store"
)

// OpenBacklog returns open findings that have not yet had a reproduction
// attempt: Tier 2 or 3, ReproPath empty, ReproWitness empty, NeedsHuman
// false. It queries all open findings from the store (the store filter does
// not support ReproPath or NeedsHuman predicates) and filters in Go. A
// non-empty ReproWitness excludes the row: the finding already received its
// non-promoting witness bundle (below-quorum survivor or witness-only
// ecosystem, bugbot-qb4r layer b) and witness-only is a static property of
// the build's ecosystem.WitnessTable — re-dispatching it every firing could
// only re-produce the same non-promoting outcome. NeedsHuman excludes both
// patch-prover-exhausted and below-quorum verifier findings deliberately (see
// the dual-meaning note in funnel/verify_stream.go, bugbot-sw7).
//
// Rotation design: findings are returned oldest-updated-first (updated_at ASC).
// When a repro attempt fails, the caller "touches" the finding via UpsertFinding
// so its updated_at bumps. On the next firing those touched failures sort to the
// BACK of the queue, and un-attempted (or long-ago-attempted) findings rotate to
// the FRONT. This prevents the same unreproducible findings from burning budget
// on every firing while others are never reached.
//
// ListFindings returns DESC; we re-sort here in Go. Backlogs are small (at most
// a few hundred findings in practice) so the in-process sort is negligible.
//
// Callers that want a bounded set should slice the result; the caller is
// responsible for the batch-size cap so this helper stays reusable.
func OpenBacklog(ctx context.Context, st store.StoreReader) ([]domain.Finding, error) {
	all, err := st.ListFindings(ctx, domain.FindingFilter{Status: domain.StatusOpen})
	if err != nil {
		return nil, err
	}
	out := make([]domain.Finding, 0, len(all))
	for _, f := range all {
		if (f.Tier == domain.TierVerified || f.Tier == domain.TierSuspected) && f.ReproPath == "" && f.ReproWitness == "" && !f.NeedsHuman {
			out = append(out, f)
		}
	}
	// Sort oldest-updated-first so rotation works: callers touch failed findings
	// (bumping updated_at) and those failures move to the back of the queue.
	sort.Slice(out, func(i, j int) bool {
		return out[i].UpdatedAt.Before(out[j].UpdatedAt)
	})
	return out, nil
}

// runReproBacklog is the backlog-timer step: it queries open T2/T3 findings
// with no repro attempt, caps the batch to cfg.ReproBacklogBatch, and runs
// them through PromoteAll. It is a no-op unless EnableRepro is true and a
// Promoter is wired in. The day-budget gate is applied by the caller (Run).
//
// After each firing, findings that were attempted but NOT promoted (i.e. repro
// failed) are "touched" via a no-op UpsertFinding that bumps updated_at. On the
// next firing, OpenBacklog's oldest-updated-first ordering pushes those failures
// to the back of the queue, ensuring the batch rotates through the full backlog
// rather than burning budget on the same unreproducible findings forever.
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
	d.emitReproBlocked(ctx)

	// Touch findings that were attempted but not promoted so their updated_at
	// advances. OpenBacklog orders oldest-first, so these failures move to the
	// back of the queue on the next firing, letting the batch rotate to other
	// findings instead of retrying the same unreproducible ones forever.
	//
	// We determine "attempted but not promoted" by re-reading each batch finding
	// from the store: a promoted finding now has a non-empty ReproPath (set by
	// promoteFinding); one that still has an empty ReproPath was not promoted.
	// This avoids depending on PromoteAll's Summary internals.
	TouchBacklogFailures(ctx, d.store, d.log, batch)
}

// TouchBacklogFailures re-reads each finding in batch and, for any whose
// ReproPath is still empty (not promoted), calls UpsertFinding to bump
// updated_at. Errors are logged but do not abort the loop — a missed touch
// means that finding will be re-selected sooner than ideal, not that data is
// corrupted.
//
// This is exported so the CLI `bugbot repro` one-shot drain can apply the same
// rotation logic as the daemon's periodic backlog step.
func TouchBacklogFailures(ctx context.Context, st *store.Store, log interface {
	Info(string, ...any)
	Error(string, ...any)
}, batch []domain.Finding) {
	if ctx.Err() != nil {
		return
	}
	for _, f := range batch {
		current, err := st.GetFinding(ctx, f.ID)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Error("daemon: repro backlog touch: get finding failed",
				"id", f.ID, "err", err)
			continue
		}
		if current.ReproPath != "" {
			// Promoted: UpsertFinding already bumped updated_at; no touch needed.
			continue
		}
		// Not promoted: upsert unchanged to bump updated_at.
		if _, err := st.UpsertFinding(ctx, current); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Error("daemon: repro backlog touch: upsert failed",
				"id", f.ID, "err", err)
		}
	}
}
