package daemon

import (
	"context"
	"time"

	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/report"
	"github.com/dpoage/bugbot/internal/store"
	"github.com/dpoage/bugbot/internal/util"
)

// skipInfo carries the reason a cycle was skipped. Its presence (non-nil)
// means the cycle was skipped; a nil pointer means it ran.
type skipInfo struct {
	reason string
}

// cycleResult captures the per-cycle accounting the daemon logs as a one-line
// summary. It is also the natural assertion surface for tests.
type cycleResult struct {
	kind      store.ScanKind
	skip      *skipInfo // non-nil when skipped (e.g. day budget exhausted)
	scanRunID string
	newF      int // findings surfaced by this cycle's scan
	closedF   int // findings auto-closed by re-verification
	promoted  int // findings promoted to T1 this cycle
	inTokens  int64
	outTokens int64
	cacheRead int64
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
			"head", util.ShortSHA(pr.HeadSHA),
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
		"head", util.ShortSHA(pr.HeadSHA),
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

	// Seed the leads blackboard with static-analyzer hits before the finder stage.
	// Best-effort: the hook degrades gracefully on any failure.
	if d.seedAnalyzers != nil {
		d.seedAnalyzers(ctx)
	}

	// Seed the leads blackboard with doc-contradiction leads. Pure-Go, no
	// runtime requirement; best-effort, degrades on any failure.
	if d.seedContradictions != nil {
		d.seedContradictions(ctx)
	}

	// Build ChangeContext for the diff-intent lens. All pieces already exist in
	// ingest; failures here are non-fatal — the scan still runs, just without
	// the diff-intent task. The context window is lastSeen→pr.HeadSHA.
	cc := buildChangeContext(ctx, d.repo, lastSeen, pr.HeadSHA, changed)

	f, err := d.newFunnelWith(cc)
	if err != nil {
		d.log.Error("daemon: build funnel failed", "err", err)
		return
	}
	defer func() { _ = f.Close() }() // shut down per-cycle language servers
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

// buildChangeContext assembles a funnel.ChangeContext from the ingest package
// helpers. All field lookups are best-effort: a failure populates a zero/nil
// field but does not abort the cycle. When fromSHA or toSHA is empty the
// function returns nil (no context to build).
func buildChangeContext(ctx context.Context, repo *ingest.Repo, fromSHA, toSHA string, changed []string) *funnel.ChangeContext {
	if fromSHA == "" || toSHA == "" {
		return nil
	}
	cc := &funnel.ChangeContext{
		FromCommit:   fromSHA,
		ToCommit:     toSHA,
		ChangedFiles: changed,
	}
	msg, err := repo.CommitMessage(ctx, toSHA)
	if err == nil {
		cc.Message = msg
	}
	diff, err := repo.UnifiedDiff(ctx, fromSHA, toSHA)
	if err == nil {
		cc.Diff = diff
	}
	// BlastFiles is intentionally absent: the blast-radius dependent list is
	// derived inside hypothesize from the targets that Targeted already expanded
	// via BlastRadius (run.go). No field to set here.
	return cc
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

	// Seed the leads blackboard with static-analyzer hits before the finder stage.
	// Best-effort: the hook degrades gracefully on any failure.
	if d.seedAnalyzers != nil {
		d.seedAnalyzers(ctx)
	}

	// Seed the leads blackboard with doc-contradiction leads. Pure-Go, no
	// runtime requirement; best-effort, degrades on any failure.
	if d.seedContradictions != nil {
		d.seedContradictions(ctx)
	}

	f, err := d.newFunnel()
	if err != nil {
		d.log.Error("daemon: build funnel failed", "err", err)
		return
	}
	defer func() { _ = f.Close() }() // shut down per-cycle language servers
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

// runVerifyDrain is the verify-drain timer step: it drains the
// pending_candidates write-ahead log left by interrupted runs, verifying each
// WITHOUT re-running the finder/cartographer stage. Cheap when the WAL is empty
// (a single store query inside VerifyDrain). The day-budget gate is applied by
// the caller (Run), mirroring the backlog timer; spend is folded into a
// cycleResult and logged. Returns quietly on context cancellation.
func (d *Daemon) runVerifyDrain(ctx context.Context) {
	start := d.clock.now()
	progress.Emit(d.prog, progress.Event{Kind: progress.KindCycleStarted, ScanKind: string(store.ScanVerifyDrain)})
	res := cycleResult{kind: store.ScanVerifyDrain}

	f, err := d.newFunnel()
	if err != nil {
		d.log.Error("daemon: build funnel failed", "err", err)
		return
	}
	defer func() { _ = f.Close() }() // shut down per-cycle language servers

	fres, err := f.VerifyDrain(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		d.log.Error("daemon: verify drain failed", "err", err)
		return
	}
	// Newly-verified candidates become genuine new Tier-2 findings, so they
	// count as this cycle's new findings (unlike the impact sweep, which only
	// re-ranks existing ones).
	d.foldSpend(&res, fres)
	res.newF = len(fres.Findings)
	d.logCycle(res, d.clock.now().Sub(start))
}

// runImpactSweep is the impact-sweep-drain timer step: it re-ranks every open
// finding not yet swept (swept_at NULL) by reachability/impact — this run's and
// any stranded by an interrupted/older run. Cheap when nothing is unswept
// (SweepDrain early-returns). The day-budget gate is applied by the caller
// (Run). It surfaces no NEW findings (only re-ranks), so newF stays zero; spend
// is folded and logged. Returns quietly on context cancellation.
func (d *Daemon) runImpactSweep(ctx context.Context) {
	start := d.clock.now()
	progress.Emit(d.prog, progress.Event{Kind: progress.KindCycleStarted, ScanKind: string(store.ScanImpactSweep)})
	res := cycleResult{kind: store.ScanImpactSweep}

	f, err := d.newFunnel()
	if err != nil {
		d.log.Error("daemon: build funnel failed", "err", err)
		return
	}
	defer func() { _ = f.Close() }() // shut down per-cycle language servers

	fres, err := f.SweepDrain(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		d.log.Error("daemon: impact sweep drain failed", "err", err)
		return
	}
	d.foldSpend(&res, fres)
	d.logCycle(res, d.clock.now().Sub(start))
}

// foldSpend folds a funnel.Result's scan-run id and token spend into the cycle
// accounting. It does NOT emit the finder-reliability warning: callers whose
// result came from the finder stage (poll/sweep) use recordScanResult, which
// adds that check. The maintenance drains run no finders, so a "no finders ran"
// warning would be a false alarm — they fold spend via this helper directly.
func (d *Daemon) foldSpend(res *cycleResult, fres *funnel.Result) {
	res.scanRunID = fres.ScanRunID
	res.inTokens = fres.Stats.InputTokens
	res.outTokens = fres.Stats.OutputTokens
	res.cacheRead = fres.Stats.CacheReadTokens
}

// recordScanResult folds a finder-stage funnel.Result into the cycle accounting
// (spend + new-finding count) and emits the reliability warning.
func (d *Daemon) recordScanResult(res *cycleResult, fres *funnel.Result) {
	d.foldSpend(res, fres)
	res.newF = len(fres.Findings)
	d.warnIfUnreliable(fres)
}

// warnIfUnreliable logs a prominent warning when the finder stage was not fully
// reliable, mirroring the CLI scan banner (internal/cli/scan.go). The daemon
// runs unattended, so a silent "No findings" on a broken sweep is the trust bug
// this guards against: an empty/sparse finding set when finders failed must be
// loudly flagged as untrustworthy, not treated as clean. This never aborts the
// cycle — the daemon must keep running.
func (d *Daemon) warnIfUnreliable(fres *funnel.Result) {
	s := fres.Stats
	if s.FinderReliable() {
		return
	}
	switch {
	case s.FinderRuns == 0:
		d.log.Warn("daemon: SCAN RELIABILITY WARNING: no finder agents ran — this result says NOTHING about the code's correctness",
			"finder_runs", s.FinderRuns)
	case s.MostFindersFailed():
		d.log.Warn("daemon: SCAN RELIABILITY WARNING: most finders failed — effectively no signal; treat 'no findings' as UNKNOWN, not clean. Check model/output-token settings",
			"finder_failures", s.FinderFailures, "finder_runs", s.FinderRuns)
	default:
		d.log.Warn("daemon: SCAN RELIABILITY WARNING: some finders produced no parseable output — coverage incomplete; do not read a low finding count as a clean bill of health",
			"finder_failures", s.FinderFailures, "finder_runs", s.FinderRuns)
	}
}

// finishCycle emits the report and logs the cycle summary. Reporting failures
// are logged but never abort the loop.
func (d *Daemon) finishCycle(ctx context.Context, res cycleResult, start time.Time) {
	d.emitReport(ctx, res.kind)
	d.logCycle(res, d.clock.now().Sub(start))
}

// dayBudgetExhausted reports whether the day's recorded spend has reached the
// per-day cap. When it has, it marks res skipped (with a reason) and logs
// loudly. A zero (or negative) PerDayTokens means UNLIMITED and never skips.
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
	if totals.Chargeable(d.cfg.CacheReadWeight) < d.cfg.PerDayTokens {
		return false
	}
	res.skip = &skipInfo{reason: "per-day token budget exhausted"}
	d.log.Warn("daemon: DAY BUDGET EXHAUSTED — skipping cycle (no LLM calls)",
		"kind", string(res.kind),
		"day_spend", totals.Chargeable(d.cfg.CacheReadWeight),
		"day_spend_raw", totals.Total(),
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
	var skipReason string
	if res.skip != nil {
		skipReason = res.skip.reason
	}
	progress.Emit(d.prog, progress.Event{
		Kind: progress.KindCycleFinished, ScanKind: string(res.kind),
		Count:           res.newF,
		InputTokens:     res.inTokens,
		OutputTokens:    res.outTokens,
		CacheReadTokens: res.cacheRead,
		Message:         skipReason,
		Counts:          &progress.Counts{Verified: res.newF},
	})
	if res.skip != nil {
		d.log.Info("daemon: cycle skipped",
			"kind", string(res.kind),
			"reason", res.skip.reason,
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

// refreshWatermarks recomputes the snapshot fingerprints and refreshes the
// content_hash and last_scanned_commit columns in file_state, anchored to
// commit. It deliberately uses RefreshContentHashes (not UpsertFileStates) so
// that truthful last_scanned_at timestamps written by TouchScanCoverage are
// never clobbered: a file scanned in this sweep has its scan time recorded by
// the funnel; refreshWatermarks only needs to update the content hashes for
// incremental change detection. Best-effort: errors are logged only.
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
	if err := d.store.RefreshContentHashes(ctx, states); err != nil {
		d.log.Error("daemon: sweep watermark refresh failed", "err", err)
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
