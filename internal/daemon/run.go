package daemon

import (
	"context"
	"errors"
	"time"

	"github.com/dpoage/bugbot/internal/store"
)

// Run drives the scheduler until ctx is cancelled, at which point it returns
// nil (graceful stop). A real error — one that means the loop can no longer make
// progress, such as a failure to read the last-seen watermark — is returned so
// the CLI can surface a nonzero exit.
//
// The scheduler maintains two independent deadlines (next poll, next sweep) and
// at each iteration waits for the nearer one (or ctx). Cancellation is checked
// only between cycles, so an in-flight cycle finishes its persistence before Run
// returns — the daemon never kills work mid-write.
func (d *Daemon) Run(ctx context.Context) error {
	now := d.clock.now()

	// Sweep fires once at startup if the store has no prior sweep run; otherwise
	// it waits a full SweepInterval. Poll always starts one interval out.
	nextPoll := now.Add(d.cfg.PollInterval)

	hadSweep, err := hasSweepRun(ctx, d.store)
	if err != nil {
		// A cancellation racing startup is a graceful stop, not a failure.
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	var nextSweep time.Time
	if hadSweep {
		nextSweep = now.Add(d.cfg.SweepInterval)
	} else {
		nextSweep = now // due immediately
	}

	d.log.Info("daemon: started",
		"poll_interval", d.cfg.PollInterval.String(),
		"sweep_interval", d.cfg.SweepInterval.String(),
		"idle_backoff", d.cfg.IdleBackoff.String(),
		"per_cycle_tokens", d.cfg.PerCycleTokens,
		"per_day_tokens", d.cfg.PerDayTokens,
		"startup_sweep", !hadSweep,
		"sinks", len(d.sinks),
		"repro", d.cfg.EnableRepro && d.repro != nil,
	)

	for {
		now = d.clock.now()
		// Pick the nearer deadline; wait for it (or cancellation).
		fireSweep := !nextSweep.After(nextPoll)
		deadline := nextPoll
		if fireSweep {
			deadline = nextSweep
		}
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

		if fireSweep {
			d.runSweep(ctx)
			nextSweep = d.clock.now().Add(d.cfg.SweepInterval)
		} else {
			d.runPoll(ctx)
			nextPoll = d.clock.now().Add(d.nextPollDelay())
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
