package daemon

import (
	"context"
	"errors"
	"time"

	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/store"
)

// timerKind identifies which scheduler timer won the deadline race in
// schedule.earliest, so the Run loop dispatches the matching cycle.
type timerKind int

const (
	timerPoll timerKind = iota
	timerSweep
	timerBacklog
	timerVerifyDrain
	timerImpactSweep
)

// schedule groups the scheduler's independent timer deadlines. Pointer fields
// are nil when that timer is disabled (nil = "never fire", not a magic
// far-future time):
//   - nextBacklog: nil when EnableRepro is false or repro is nil.
//   - nextVerifyDrain / nextImpactSweep: the two low-priority maintenance
//     drains; nil only when their interval is non-positive (New defaults both
//     to >0, so non-nil in practice).
//
// NAMING: nextVerifyDrain/nextImpactSweep are the verify-drain and impact-sweep
// DRAIN passes (funnel.VerifyDrain / funnel.SweepDrain). They are DISTINCT from
// nextSweep, which is the FULL funnel scan (f.Sweep, cycle.go runSweep). Do not
// conflate the impact-sweep drain with the full sweep.
type schedule struct {
	nextPoll        time.Time
	nextSweep       time.Time
	nextBacklog     *time.Time // nil = disabled (EnableRepro off)
	nextVerifyDrain *time.Time // nil = disabled; verify-drain pass, NOT nextSweep
	nextImpactSweep *time.Time // nil = disabled; impact-sweep drain, NOT nextSweep
}

// timerDeadline pairs an active timer's deadline with its kind for the
// priority-ordered earliest() race.
type timerDeadline struct {
	when time.Time
	kind timerKind
}

// earliest returns the nearest active deadline and which timer it belongs to.
// Candidates are listed in strict priority order — sweep > backlog > verify-drain
// > impact-sweep > poll — then the race seeds the first (highest-priority) one
// and replaces the incumbent only on a STRICTLY-earlier deadline. So the nearest
// deadline always wins, and on an exact tie the higher-priority timer wins. This
// preserves the original sweep>backlog>poll tie-break and slots the two
// maintenance drains after backlog and before poll. Disabled (nil) timers are
// excluded from the race entirely.
func (s schedule) earliest() (time.Time, timerKind) {
	cands := make([]timerDeadline, 0, 5)
	cands = append(cands, timerDeadline{s.nextSweep, timerSweep})
	if s.nextBacklog != nil {
		cands = append(cands, timerDeadline{*s.nextBacklog, timerBacklog})
	}
	if s.nextVerifyDrain != nil {
		cands = append(cands, timerDeadline{*s.nextVerifyDrain, timerVerifyDrain})
	}
	if s.nextImpactSweep != nil {
		cands = append(cands, timerDeadline{*s.nextImpactSweep, timerImpactSweep})
	}
	cands = append(cands, timerDeadline{s.nextPoll, timerPoll})

	best := cands[0]
	for _, c := range cands[1:] {
		if c.when.Before(best.when) {
			best = c
		}
	}
	return best.when, best.kind
}

// Run drives the scheduler until ctx is cancelled, at which point it returns
// nil (graceful stop). A real error — one that means the loop can no longer make
// progress, such as a failure to read the last-seen watermark — is returned so
// the CLI can surface a nonzero exit.
//
// The scheduler maintains three independent deadlines (next poll, next sweep,
// next backlog-repro) and at each iteration waits for the nearest one (or ctx).
// Cancellation is checked only between cycles, so an in-flight cycle finishes
// its persistence before Run returns — the daemon never kills work mid-write.
//
// The backlog-repro timer only fires when EnableRepro is set; otherwise it is
// disabled (nil) so it never wins the deadline race.
func (d *Daemon) Run(ctx context.Context) error {
	now := d.clock.now()
	// Close the daemon-lifetime CodeNav (LSP manager) exactly once when Run exits,
	// regardless of the exit path (graceful context cancel, error, or panic-recovery).
	defer func() {
		if err := d.sharedNav.Close(); err != nil {
			d.log.Warn("daemon: codenav close", "err", err)
		}
	}()

	hadSweep, err := hasSweepRun(ctx, d.store)
	if err != nil {
		// A cancellation racing startup is a graceful stop, not a failure.
		if ctx.Err() != nil {
			return nil
		}
		return err
	}

	// Build the initial schedule. Sweep fires immediately at startup if the
	// store has no prior sweep run; otherwise it waits a full SweepInterval.
	// Poll always starts one interval out. The backlog timer is only active
	// when EnableRepro is set; otherwise nextBacklog is nil (disabled).
	sched := schedule{
		nextPoll:  now.Add(d.cfg.PollInterval),
		nextSweep: now, // due immediately; overridden below if hadSweep
	}
	if hadSweep {
		sched.nextSweep = now.Add(d.cfg.SweepInterval)
	}
	reproEnabled := d.cfg.EnableRepro && d.repro != nil
	if reproEnabled {
		t := now.Add(d.cfg.ReproBacklogInterval)
		sched.nextBacklog = &t
	}
	// The two maintenance drains always fire (New defaults their intervals to
	// >0); a non-positive interval disables one (nil = excluded from the race).
	if d.cfg.VerifyDrainInterval > 0 {
		vt := now.Add(d.cfg.VerifyDrainInterval)
		sched.nextVerifyDrain = &vt
	}
	if d.cfg.ImpactSweepInterval > 0 {
		it := now.Add(d.cfg.ImpactSweepInterval)
		sched.nextImpactSweep = &it
	}

	d.log.Info("daemon: started",
		"poll_interval", d.cfg.PollInterval.String(),
		"sweep_interval", d.cfg.SweepInterval.String(),
		"idle_backoff", d.cfg.IdleBackoff.String(),
		"per_cycle_tokens", d.cfg.PerCycleTokens,
		"per_day_tokens", d.cfg.PerDayTokens,
		"startup_sweep", !hadSweep,
		"sinks", len(d.sinks),
		"repro", reproEnabled,
		"repro_backlog_interval", d.cfg.ReproBacklogInterval.String(),
		"verify_drain_interval", d.cfg.VerifyDrainInterval.String(),
		"impact_sweep_interval", d.cfg.ImpactSweepInterval.String(),
	)

	for {
		now = d.clock.now()
		// Emit the schedule event. NextBacklog is zero when repro is disabled,
		// keeping the field absent from JSON for consumers that only care about
		// poll/sweep.
		schedEv := progress.Event{
			Kind: progress.KindCycleScheduled, NextPoll: sched.nextPoll, NextSweep: sched.nextSweep,
		}
		if sched.nextBacklog != nil {
			schedEv.NextBacklog = *sched.nextBacklog
		}
		progress.Emit(d.prog, schedEv)

		// Pick the nearest active deadline and wait for it (or cancellation).
		deadline, fired := sched.earliest()
		wait := deadline.Sub(now)
		if wait < 0 {
			wait = 0
		}

		timerC, stop := d.clock.newTimer(wait)
		select {
		case <-ctx.Done():
			stop()
			d.log.Info("daemon: shutting down", "reason", ctx.Err().Error())
			return nil
		case <-timerC:
		}

		// Re-check cancellation after waking so a cancel that raced the timer is
		// honored before launching a cycle.
		if ctx.Err() != nil {
			d.log.Info("daemon: shutting down", "reason", ctx.Err().Error())
			return nil
		}

		switch fired {
		case timerSweep:
			d.runSweep(ctx)
			sched.nextSweep = d.clock.now().Add(d.cfg.SweepInterval)
		case timerBacklog:
			// Gate backlog on day budget: if the budget is exhausted we skip and
			// reschedule — the backlog will be retried at the next interval.
			if d.cfg.PerDayTokens > 0 {
				sentinel := cycleResult{kind: store.ScanTargeted}
				if d.dayBudgetExhausted(ctx, &sentinel) {
					d.log.Info("daemon: repro backlog skipped: day budget exhausted")
					t := d.clock.now().Add(d.cfg.ReproBacklogInterval)
					sched.nextBacklog = &t
					break
				}
			}
			d.runReproBacklog(ctx)
			t := d.clock.now().Add(d.cfg.ReproBacklogInterval)
			sched.nextBacklog = &t
		case timerVerifyDrain:
			// Same day-budget gate as backlog: skip + reschedule when exhausted.
			if d.cfg.PerDayTokens > 0 {
				sentinel := cycleResult{kind: store.ScanVerifyDrain}
				if d.dayBudgetExhausted(ctx, &sentinel) {
					d.log.Info("daemon: verify drain skipped: day budget exhausted")
					t := d.clock.now().Add(d.cfg.VerifyDrainInterval)
					sched.nextVerifyDrain = &t
					break
				}
			}
			d.runVerifyDrain(ctx)
			t := d.clock.now().Add(d.cfg.VerifyDrainInterval)
			sched.nextVerifyDrain = &t
		case timerImpactSweep:
			// Same day-budget gate as backlog: skip + reschedule when exhausted.
			if d.cfg.PerDayTokens > 0 {
				sentinel := cycleResult{kind: store.ScanImpactSweep}
				if d.dayBudgetExhausted(ctx, &sentinel) {
					d.log.Info("daemon: impact sweep skipped: day budget exhausted")
					t := d.clock.now().Add(d.cfg.ImpactSweepInterval)
					sched.nextImpactSweep = &t
					break
				}
			}
			d.runImpactSweep(ctx)
			t := d.clock.now().Add(d.cfg.ImpactSweepInterval)
			sched.nextImpactSweep = &t
		default: // timerPoll
			d.runPoll(ctx)
			sched.nextPoll = d.clock.now().Add(d.nextPollDelay())
		}
	}
}

// nextPollDelay returns the interval until the next poll, applying idle backoff.
// idleMultiplier grows on idle polls (handled in runPoll) and is folded in here:
// the extra delay is idleMultiplier * PollInterval, on top of the base interval,
// capped so the total never exceeds (1 + maxBackoffMultiplier) * PollInterval.
func (d *Daemon) nextPollDelay() time.Duration {
	mult := int(d.idleMultiplier.Load())
	if d.cfg.IdleBackoff <= 0 || mult == 0 {
		return d.cfg.PollInterval
	}
	if mult > maxBackoffMultiplier {
		mult = maxBackoffMultiplier
	}
	extra := time.Duration(mult) * d.cfg.IdleBackoff
	maxExtra := time.Duration(maxBackoffMultiplier) * d.cfg.PollInterval
	if extra > maxExtra {
		extra = maxExtra
	}
	return d.cfg.PollInterval + extra
}

// hasSweepRun reports whether the store already holds at least one sweep
// scan-run, used to decide the startup sweep. There is no typed store helper for
// this, so it queries via the exported *sql.DB. The query is cheap (indexed scan
// over a tiny table) and runs once at startup.
func hasSweepRun(ctx context.Context, st *store.Store) (bool, error) {
	var n int
	err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(1) FROM scan_runs WHERE kind = ?`, string(store.ScanSweep),
	).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// loadLastSeen reads the persisted last-seen commit tip from the file_state
// sentinel row. A missing row means "no baseline yet" and returns "".
func (d *Daemon) loadLastSeen(ctx context.Context) (string, error) {
	fs, err := d.store.GetFileState(ctx, lastSeenSentinel)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return fs.ContentHash, nil
}

// saveLastSeen persists the last-seen commit tip into the file_state sentinel
// row. The commit SHA is stored in content_hash; last_scanned_commit mirrors it
// for legibility when inspecting the table.
func (d *Daemon) saveLastSeen(ctx context.Context, sha string) error {
	if sha == "" {
		return nil
	}
	return d.store.UpsertFileStates(ctx, []store.FileState{{
		Path:              lastSeenSentinel,
		ContentHash:       sha,
		LastScannedCommit: sha,
	}})
}
