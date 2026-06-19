package daemon

import (
	"context"
	"errors"
	"time"

	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/store"
)

// schedule groups the three independent timer deadlines for the scheduler loop.
// nextBacklog is nil when the backlog-repro timer is disabled (EnableRepro is
// false or repro is nil); nil means "never fire", not a magic far-future time.
type schedule struct {
	nextPoll    time.Time
	nextSweep   time.Time
	nextBacklog *time.Time // nil = disabled
}

// earliest returns the nearest active deadline and flags which timer fired.
// When nextBacklog is nil it is excluded from the race.
func (s schedule) earliest() (deadline time.Time, fireSweep, fireBacklog bool) {
	// Start with poll as the default.
	deadline = s.nextPoll
	fireSweep = false
	fireBacklog = false

	if !s.nextSweep.After(deadline) {
		deadline = s.nextSweep
		fireSweep = true
	}
	if s.nextBacklog != nil && !s.nextBacklog.After(deadline) {
		deadline = *s.nextBacklog
		fireSweep = false
		fireBacklog = true
	}
	return deadline, fireSweep, fireBacklog
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
		deadline, fireSweep, fireBacklog := sched.earliest()
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

		switch {
		case fireSweep:
			d.runSweep(ctx)
			sched.nextSweep = d.clock.now().Add(d.cfg.SweepInterval)
		case fireBacklog:
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
		default:
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
