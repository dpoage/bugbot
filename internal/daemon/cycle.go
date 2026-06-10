package daemon

import (
	"context"
	"time"

	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/report"
	"github.com/dpoage/bugbot/internal/store"
)

// cycleResult captures the per-cycle accounting the daemon logs as a one-line
// summary. It is also the natural assertion surface for tests.
type cycleResult struct {
	kind       store.ScanKind
	skipped    bool   // day budget exhausted -> no LLM work
	skipReason string // populated when skipped
	scanRunID  string
	newF       int // findings surfaced by this cycle's scan
	closedF    int // findings auto-closed by re-verification
	promoted   int // findings promoted to T1 this cycle
	inTokens   int64
	outTokens  int64
	cacheRead  int64
}

// runPoll executes one poll cycle: detect new commits since the last-seen tip
// and, if any, run a Targeted investigation of their blast radius. An unchanged
// repo grows the idle backoff and does no LLM work (near-zero spend). The poll
// itself (a git fetch + rev-parse) is cheap and runs even under day-budget
// exhaustion; only the funnel work is budget-gated.
func (d *Daemon) runPoll(ctx context.Context) {
	start := d.clock.now()
	progress.Emit(d.prog, progress.Event{Kind: progress.KindCycleStarted, ScanKind: string(store.ScanTargeted)})

	lastSeen, err := d.loadLastSeen(ctx)
	if err != nil {
		d.log.Error("daemon: poll: load last-seen failed", "err", err)
		return
	}

	pr, err := d.poller.Poll(ctx, lastSeen)
	if err != nil {
		d.log.Error("daemon: poll failed", "err", err)
		return
	}

	// Always advance the watermark to the observed tip, even when establishing
	// the first baseline (lastSeen == "") so we don't replay history next time.
	if err := d.saveLastSeen(ctx, pr.HeadSHA); err != nil {
		d.log.Error("daemon: poll: persist last-seen failed", "err", err)
		// Non-fatal: continue; worst case we re-detect the same commits next tick.
	}

	if !pr.HasNew() {
		// Idle: grow backoff (capped) and skip all LLM work.
		if d.idleMultiplier.Load() < maxBackoffMultiplier {
			d.idleMultiplier.Add(1)
		}
		d.log.Info("daemon: poll idle",
			"head", shortSHA(pr.HeadSHA),
			"backoff_mult", d.idleMultiplier.Load(),
			"next_poll_in", d.nextPollDelay().String(),
		)
		return
	}

	// Activity resets the backoff so we poll promptly while the repo is active.
	d.idleMultiplier.Store(0)

	changed := changedPathsSince(ctx, d.repo, lastSeen, pr.HeadSHA)
	d.log.Info("daemon: new commits",
		"count", len(pr.NewCommits),
		"head", shortSHA(pr.HeadSHA),
		"changed_files", len(changed),
	)

	res := cycleResult{kind: store.ScanTargeted}
	if d.dayBudgetExhausted(ctx, &res) {
		d.logCycle(res, d.clock.now().Sub(start))
		return
	}

	if len(changed) == 0 {
		// New commits but nothing in scope changed (e.g. only excluded files):
		// still run post-cycle re-verification, but no targeted scan.
		d.postCycle(ctx, nil, &res)
		d.finishCycle(ctx, res, start)
		return
	}

	f, err := d.newFunnel()
	if err != nil {
		d.log.Error("daemon: build funnel failed", "err", err)
		return
	}
	fres, err := f.Targeted(ctx, changed)
	if err != nil {
		if ctx.Err() != nil {
			return // graceful shutdown raced the scan; nothing partial persisted to fix
		}
		d.log.Error("daemon: targeted scan failed", "err", err)
		return
	}
	d.recordScanResult(&res, fres)
	d.postCycle(ctx, fres, &res)
	d.finishCycle(ctx, res, start)
}

// runSweep executes one sweep cycle: a whole-snapshot investigation plus the
// shared post-cycle pass. After a successful sweep it refreshes the file_state
// watermarks from the snapshot's fingerprints so incremental change detection
// has a fresh baseline.
func (d *Daemon) runSweep(ctx context.Context) {
	start := d.clock.now()
	progress.Emit(d.prog, progress.Event{Kind: progress.KindCycleStarted, ScanKind: string(store.ScanSweep)})
	res := cycleResult{kind: store.ScanSweep}

	if d.dayBudgetExhausted(ctx, &res) {
		d.logCycle(res, d.clock.now().Sub(start))
		return
	}

	f, err := d.newFunnel()
	if err != nil {
		d.log.Error("daemon: build funnel failed", "err", err)
		return
	}
	fres, err := f.Sweep(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		d.log.Error("daemon: sweep failed", "err", err)
		return
	}
	d.recordScanResult(&res, fres)

	// Refresh watermarks so subsequent incremental logic has a baseline at this
	// sweep's commit. Non-fatal on error.
	d.refreshWatermarks(ctx, fres.Commit)

	d.postCycle(ctx, fres, &res)
	d.finishCycle(ctx, res, start)
}

// recordScanResult folds a funnel.Result into the cycle accounting.
func (d *Daemon) recordScanResult(res *cycleResult, fres *funnel.Result) {
	res.scanRunID = fres.ScanRunID
	res.newF = len(fres.Findings)
	res.inTokens = fres.Stats.InputTokens
	res.outTokens = fres.Stats.OutputTokens
	res.cacheRead = fres.Stats.CacheReadTokens
}

// finishCycle emits the report and logs the cycle summary. Reporting failures
// are logged but never abort the loop.
func (d *Daemon) finishCycle(ctx context.Context, res cycleResult, start time.Time) {
	d.emitReport(ctx, res.kind)
	d.logCycle(res, d.clock.now().Sub(start))
}

// dayBudgetExhausted reports whether the day's recorded spend has reached the
// per-day cap. When it has, it marks res skipped (with a reason) and logs
// loudly. A zero PerDayTokens means unlimited and never skips.
func (d *Daemon) dayBudgetExhausted(ctx context.Context, res *cycleResult) bool {
	if d.cfg.PerDayTokens <= 0 {
		return false
	}
	midnight := startOfUTCDay(d.clock.now())
	totals, err := d.store.TotalsSince(ctx, midnight)
	if err != nil {
		// If we cannot read spend we conservatively DO NOT skip (failing closed
		// would silently stall the daemon); log and proceed. The per-cycle budget
		// still bounds this cycle.
		d.log.Error("daemon: read day spend failed; proceeding", "err", err)
		return false
	}
	if totals.Total() < d.cfg.PerDayTokens {
		return false
	}
	res.skipped = true
	res.skipReason = "per-day token budget exhausted"
	d.log.Warn("daemon: DAY BUDGET EXHAUSTED — skipping cycle (no LLM calls)",
		"kind", string(res.kind),
		"day_spend", totals.Total(),
		"per_day_tokens", d.cfg.PerDayTokens,
	)
	return true
}

// emitReport renders current open findings through the configured sinks. It is
// best-effort: a sink error is logged, not fatal, and an empty sink set is a
// no-op.
func (d *Daemon) emitReport(ctx context.Context, kind store.ScanKind) {
	if len(d.sinks) == 0 {
		return
	}
	head, _ := d.repo.HeadCommit(ctx)
	rep, err := report.CollectOpen(ctx, d.store, report.Metadata{
		RepoPath:    d.repo.Root(),
		Commit:      head,
		GeneratedAt: d.clock.now().UTC(),
	})
	if err != nil {
		d.log.Error("daemon: collect open findings failed", "err", err)
		return
	}
	for _, sink := range d.sinks {
		if err := sink.Write(ctx, rep); err != nil {
			d.log.Error("daemon: report sink failed", "sink", sink.Name(), "err", err)
		}
	}
}

// logCycle writes the one-line structured cycle summary.
func (d *Daemon) logCycle(res cycleResult, dur time.Duration) {
	progress.Emit(d.prog, progress.Event{
		Kind: progress.KindCycleFinished, ScanKind: string(res.kind),
		Count:           res.newF,
		InputTokens:     res.inTokens,
		OutputTokens:    res.outTokens,
		CacheReadTokens: res.cacheRead,
		Message:         res.skipReason,
		Counts:          &progress.Counts{Verified: res.newF},
	})
	if res.skipped {
		d.log.Info("daemon: cycle skipped",
			"kind", string(res.kind),
			"reason", res.skipReason,
			"duration", dur.String(),
		)
		return
	}
	d.log.Info("daemon: cycle complete",
		"kind", string(res.kind),
		"scan_run", res.scanRunID,
		"findings_new", res.newF,
		"findings_closed", res.closedF,
		"promoted", res.promoted,
		"tokens_in", res.inTokens,
		"tokens_out", res.outTokens,
		"tokens_cached", res.cacheRead,
		"duration", dur.String(),
	)
}

// refreshWatermarks recomputes the snapshot fingerprints and upserts them into
// file_state, anchoring each to commit. Best-effort: errors are logged only.
func (d *Daemon) refreshWatermarks(ctx context.Context, commit string) {
	snap, err := d.repo.Snapshot(ctx, ingest.ScanFilter{})
	if err != nil {
		d.log.Error("daemon: sweep watermark snapshot failed", "err", err)
		return
	}
	fps, err := d.repo.Fingerprints(ctx, snap)
	if err != nil {
		d.log.Error("daemon: sweep watermark fingerprints failed", "err", err)
		return
	}
	states := make([]store.FileState, 0, len(fps))
	for path, hash := range fps {
		states = append(states, store.FileState{
			Path:              path,
			ContentHash:       hash,
			LastScannedCommit: commit,
		})
	}
	if err := d.store.UpsertFileStates(ctx, states); err != nil {
		d.log.Error("daemon: sweep watermark upsert failed", "err", err)
	}
}

// changedPathsSince returns the in-scope changed paths between two commits, or
// nil if lastSeen is empty (no prior baseline) or the diff fails. A diff failure
// is logged-by-omission: the caller still ran the commits' targeted scan path,
// and an empty change set just means "no targeted scan this poll".
func changedPathsSince(ctx context.Context, repo *ingest.Repo, lastSeen, head string) []string {
	if lastSeen == "" || head == "" || lastSeen == head {
		return nil
	}
	changes, err := repo.ChangedFiles(ctx, lastSeen, head)
	if err != nil {
		return nil
	}
	return ingest.ChangedPaths(changes)
}

// startOfUTCDay returns midnight UTC for t's calendar day, the lower bound for
// the per-day spend window.
func startOfUTCDay(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// shortSHA abbreviates a commit SHA for logs.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
