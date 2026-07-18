package daemon

import (
	"context"
	"sort"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/store"
)

// OpenBacklog returns open findings still eligible for a reproduction
// dispatch: Tier 2 or 3, ReproPath empty, ReproWitness empty, NeedsHuman
// false, and — via the store's UnclaimableReproFingerprints set — no
// repro_attempts row that can never be claimed again (done, abandoned, or
// attempt budget exhausted). Without the queue check, a finding whose attempt
// completed without reproducing (it stays open/T2 with an empty ReproPath,
// queue row done) would be re-selected on every firing only to be rejected at
// claim time, flooding the summary with spurious "skipped: already claimed"
// lines (bugbot-dyj7).
//
// It queries all open findings from the store (the store filter does not
// support ReproPath or NeedsHuman predicates) and filters in Go. A non-empty
// ReproWitness excludes the row: the finding already received its
// non-promoting witness bundle (below-quorum survivor or witness-only
// ecosystem, bugbot-qb4r layer b) and witness-only is a static property of
// the build's ecosystem.WitnessTable — re-dispatching it every firing could
// only re-produce the same non-promoting outcome. NeedsHuman excludes both
// patch-prover-exhausted and below-quorum verifier findings deliberately (see
// the dual-meaning note in funnel/verify_stream.go, bugbot-sw7).
//
// Rotation design: findings are returned oldest-updated-first (updated_at ASC).
// When a repro attempt ends without finishing its queue row (interrupt release,
// infra_retry within budget), the caller "touches" the finding via UpsertFinding
// so its updated_at bumps. On the next firing those touched rows sort to the
// BACK of the queue, and un-attempted findings rotate to the FRONT. Completed
// attempts need no rotation — the unclaimable-fingerprint filter above removes
// them from the backlog entirely.
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
	unclaimable, err := st.UnclaimableReproFingerprints(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Finding, 0, len(all))
	for _, f := range all {
		if !backlogShape(f) {
			continue
		}
		if _, done := unclaimable[f.Fingerprint]; done {
			continue
		}
		out = append(out, f)
	}
	// Sort oldest-updated-first so rotation works: callers touch failed findings
	// (bumping updated_at) and those failures move to the back of the queue.
	sort.Slice(out, func(i, j int) bool {
		return out[i].UpdatedAt.Before(out[j].UpdatedAt)
	})
	return out, nil
}

// backlogShape reports whether f is, on its finding-side fields alone, a
// reproduction candidate: Tier 2 or 3, no repro artifact (ReproPath and
// ReproWitness empty), not needs-human. Status is NOT checked here — callers
// list with domain.StatusOpen. OpenBacklog additionally excludes findings
// whose queue row is unclaimable-forever; RequeueSettled deliberately uses
// the shape WITHOUT that exclusion (its whole point is resurrecting settled
// rows).
func backlogShape(f domain.Finding) bool {
	return (f.Tier == domain.TierVerified || f.Tier == domain.TierSuspected) &&
		f.ReproPath == "" && f.ReproWitness == "" && !f.NeedsHuman
}

// RequeueSettled resets the settled (done/abandoned) repro_attempts rows of
// every open backlog-shape finding back to pending with a fresh attempt
// budget, returning how many rows were reset. It is the batch half of the
// `bugbot repro --rerun` escape hatch (bugbot-xv20): after a reproducer
// upgrade, an operator re-tests the settled backlog instead of hand-editing
// state.db. Findings that are promoted, witnessed, needs-human, or dismissed
// are untouched (backlogShape excludes them before any store write), and
// live queue rows (pending/running/infra_retry/blocked_toolchain) are left
// alone by ResetReproAttempt's own state guard. The daemon never calls this —
// it is reachable only from the attended CLI path.
func RequeueSettled(ctx context.Context, st *store.Store) (int, error) {
	all, err := st.ListFindings(ctx, domain.FindingFilter{Status: domain.StatusOpen})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, f := range all {
		if !backlogShape(f) {
			continue
		}
		reset, err := st.ResetReproAttempt(ctx, f.Fingerprint)
		if err != nil {
			return n, err
		}
		if reset {
			n++
		}
	}
	return n, nil
}

// runReproBacklog is the backlog-timer step: it queries open T2/T3 findings
// with no repro attempt, caps the batch to cfg.ReproBacklogBatch, and runs
// them through PromoteAll. It is a no-op unless EnableRepro is true and a
// Promoter is wired in. The day-budget gate is applied by the caller (Run).
//
// After each firing, batch findings still lacking a ReproPath are "touched"
// via a no-op UpsertFinding that bumps updated_at. Findings whose queue row
// completed (done/abandoned) drop out of the backlog entirely via OpenBacklog's
// unclaimable-fingerprint filter; the touch matters for rows still claimable
// (interrupt release, infra_retry within budget), which OpenBacklog's
// oldest-updated-first ordering pushes to the back of the queue so the batch
// rotates through the full backlog instead of retrying the same rows first.
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
	// advances. Completed queue rows leave the backlog via OpenBacklog's
	// unclaimable filter; for still-claimable rows (released or infra_retry)
	// the oldest-first ordering moves them to the back of the queue on the
	// next firing, letting the batch rotate to other findings first.
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
