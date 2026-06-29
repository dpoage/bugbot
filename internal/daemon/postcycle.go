package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/store"
	"github.com/dpoage/bugbot/internal/util"
)

// postCycle runs the shared work that follows every scan: re-verify open
// findings whose code changed (auto-closing those whose file/line vanished or
// that the refuters now disprove), then optionally promote this cycle's new
// Tier-2 findings via reproduction.
//
// fres may be nil (a poll cycle that found new commits but nothing in scope to
// scan): re-verification still runs over all open findings, but there are no new
// findings to reproduce.
func (d *Daemon) postCycle(ctx context.Context, fres *funnel.Result, res *cycleResult) {
	closed := d.reverifyOpenFindings(ctx)
	res.closedF += closed
	progress.Emit(d.prog, progress.Event{Kind: progress.KindReverify, Count: closed})

	// Reconcile stranded work every cycle: verify any pending_candidates left by
	// an interrupted run, then re-rank any open finding not yet swept (this
	// cycle's findings included — run() no longer re-ranks severities inline).
	// Ordered verify→sweep, BEFORE the publisher and promote: newly-verified
	// findings and re-ranked severities are reflected before issues are
	// filed/promoted, and the sweep marker survives promotion (promoteFinding's
	// UpsertFinding preserves swept_at when file_hash is unchanged).
	d.runPostCycleDrains(ctx)

	// Publish reconcile: file/close GitHub issues for findings changed this
	// cycle. Runs AFTER reverify so auto-closed findings are already marked
	// fixed before the reconciler evaluates them. A missing gh binary logs a
	// warning but does not abort the daemon loop.
	d.runPublisher(ctx)

	if fres != nil {
		promoted := d.promoteNewFindings(ctx, fres)
		res.promoted += promoted
		progress.Emit(d.prog, progress.Event{Kind: progress.KindPromote, Count: promoted})
	}

	// Regress digest: log this cycle's findings introduced since the last
	// finished-sweep baseline ('last green'), so an operator sees what is
	// genuinely new this period vs pre-existing background. Best-effort.
	d.emitRegressDigest(ctx, fres)
}

// runPostCycleDrains reconciles stranded work after the main cycle: it verifies
// any pending_candidates left by an interrupted run (a no-op on an empty WAL),
// then re-ranks every open finding not yet swept (this cycle's findings
// included — run() no longer re-ranks inline). It builds its own per-cycle
// funnel because postCycle has callers with no funnel in scope (a poll cycle
// with new commits but nothing in scope to scan). Day-budget gated via a
// throwaway sentinel so an exhausted budget skips the drains WITHOUT marking the
// surrounding cycle skipped. Best-effort: a build/verify failure is logged and
// never aborts the sweep drain or the rest of postCycle. Each drain records its
// spend to the store ledger under its own scan run, so it counts toward the day
// budget without polluting the main cycle's one-line summary.
func (d *Daemon) runPostCycleDrains(ctx context.Context) {
	if d.cfg.PerDayTokens > 0 {
		sentinel := cycleResult{kind: store.ScanVerifyDrain}
		if d.dayBudgetExhausted(ctx, &sentinel) {
			d.log.Info("daemon: post-cycle drains skipped: day budget exhausted")
			return
		}
	}

	f, err := d.newFunnel()
	if err != nil {
		d.log.Error("daemon: post-cycle drains: build funnel failed", "err", err)
		return
	}
	defer func() { _ = f.Close() }() // shut down per-cycle language servers

	// Verify drain first so a candidate verified into a finding here is swept by
	// the sweep drain below in the same pass. A failure is logged but must not
	// strand the sweep drain.
	if _, err := f.VerifyDrain(ctx); err != nil {
		if ctx.Err() != nil {
			return
		}
		d.log.Error("daemon: post-cycle verify drain failed", "err", err)
	}

	if _, err := f.SweepDrain(ctx); err != nil {
		if ctx.Err() != nil {
			return
		}
		d.log.Error("daemon: post-cycle impact sweep drain failed", "err", err)
	}
}

// runPublisher calls the wired Publisher, if any. gh-not-found and similar
// transient failures are logged as warnings so the daemon loop continues
// uninterrupted.
func (d *Daemon) runPublisher(ctx context.Context) {
	if d.publisher == nil {
		return
	}
	if err := d.publisher.Publish(ctx); err != nil {
		d.log.Warn("daemon: publish reconcile failed (will retry next cycle)", "err", err)
	}
}

// reverifyOpenFindings walks every open finding and reconciles it with the
// current code:
//
//   - Hash unchanged: the code the finding was anchored to is byte-identical, so
//     the finding stands untouched.
//   - File gone OR the claimed line no longer exists: the implicated code is
//     gone; MarkFixed (auto-close).
//   - File present but content changed: re-run adversarial verification against
//     the new code via the funnel seam. A majority-refuted verdict auto-closes
//     the finding (fixed); otherwise it survives and is re-anchored to the new
//     content hash so it is not re-verified again until the code changes anew.
//
// It returns the number of findings auto-closed this pass. All store/IO errors
// are logged and skip only the offending finding; one bad finding never aborts
// the pass or the loop.
func (d *Daemon) reverifyOpenFindings(ctx context.Context) int {
	open, err := d.store.ListFindings(ctx, store.FindingFilter{Status: store.StatusOpen})
	if err != nil {
		d.log.Error("daemon: list open findings failed", "err", err)
		return 0
	}
	if len(open) == 0 {
		return 0
	}

	root := d.repo.Root()
	var f *funnel.Funnel // built lazily; only needed if some file's content changed
	defer func() {
		if f != nil {
			_ = f.Close() // shut down language servers spawned for re-verification
		}
	}()
	closed := 0

	for _, fnd := range open {
		if ctx.Err() != nil {
			return closed // graceful shutdown: stop, leave the rest for next cycle
		}

		abs := filepath.Join(root, filepath.FromSlash(fnd.File))
		content, rerr := os.ReadFile(abs)

		// File gone -> the code is gone -> fixed.
		if errors.Is(rerr, os.ErrNotExist) {
			d.autoClose(ctx, fnd, "file removed", &closed)
			continue
		}
		if rerr != nil {
			d.log.Error("daemon: reverify: read file failed", "file", fnd.File, "err", rerr)
			continue
		}

		curHash := ingest.HashBytes(content)
		if curHash == fnd.FileHash {
			continue // unchanged: leave untouched
		}

		// Content changed. If the claimed line no longer exists, the implicated
		// code is gone -> fixed.
		if fnd.Line > 0 && fnd.Line > countLines(content) {
			d.autoClose(ctx, fnd, "claimed line no longer exists", &closed)
			continue
		}

		// Content changed and the line still exists: re-verify against new code.
		if f == nil {
			built, berr := d.newFunnel()
			if berr != nil {
				d.log.Error("daemon: reverify: build funnel failed", "err", berr)
				return closed
			}
			f = built
		}
		refuted, reasoning, verr := f.VerifyFinding(ctx, fnd)
		if verr != nil {
			if ctx.Err() != nil {
				return closed
			}
			d.log.Error("daemon: reverify failed", "fingerprint", fnd.Fingerprint, "err", verr)
			continue
		}
		if refuted {
			d.autoClose(ctx, fnd, "refuted on re-verification after code change", &closed)
			continue
		}

		// Survived: re-anchor to the new content hash (and refresh the trace) so we
		// don't re-verify the same change repeatedly. A tier-3 suspected finding
		// (verification originally skipped at budget hard-stop) has now passed a
		// full refuter vote, which is exactly what tier 2 means — promote it.
		fnd.FileHash = curHash
		fnd.Reasoning = reasoning
		if fnd.Tier == domain.TierSuspected {
			fnd.Tier = domain.TierVerified
		}
		if _, uerr := d.store.UpsertFinding(ctx, fnd); uerr != nil {
			d.log.Error("daemon: reverify: re-anchor failed", "fingerprint", fnd.Fingerprint, "err", uerr)
		} else {
			d.log.Info("daemon: finding re-verified, still open",
				"fingerprint", util.ShortSHA(fnd.Fingerprint), "file", fnd.File, "line", fnd.Line)
		}
	}
	return closed
}

// autoClose marks a finding fixed and bumps the cycle's closed counter, logging
// the decision. MarkFixed records "fixed" status; we deliberately do NOT add a
// suppression (the bug was resolved, not dismissed as a false positive), so the
// same fingerprint could legitimately reopen if the bug is reintroduced.
func (d *Daemon) autoClose(ctx context.Context, fnd store.Finding, reason string, closed *int) {
	if err := d.store.MarkFixed(ctx, fnd.Fingerprint); err != nil {
		d.log.Error("daemon: auto-close failed", "fingerprint", fnd.Fingerprint, "err", err)
		return
	}
	*closed++
	d.log.Info("daemon: finding auto-closed (fixed)",
		"fingerprint", util.ShortSHA(fnd.Fingerprint),
		"file", fnd.File, "line", fnd.Line,
		"reason", reason,
	)
}

// promoteNewFindings reproduces this cycle's new Tier-2 findings and promotes
// the demonstrated ones to Tier-1. It is a no-op unless reproduction is enabled
// and a Promoter (built by the CLI when a sandbox is available) is wired in. It
// returns the number promoted.
func (d *Daemon) promoteNewFindings(ctx context.Context, fres *funnel.Result) int {
	if !d.cfg.EnableRepro || d.repro == nil {
		return 0
	}
	t2 := make([]store.Finding, 0, len(fres.Findings))
	for _, fnd := range fres.Findings {
		if fnd.Tier == domain.TierVerified {
			t2 = append(t2, fnd)
		}
	}
	if len(t2) == 0 {
		return 0
	}
	// Attribute the long-lived reproducer client's spend to this cycle's run.
	if d.reproTag != nil {
		d.reproTag.SetScanRun(fres.ScanRunID)
	}
	summary, err := d.repro.PromoteAll(ctx, d.store, t2)
	if err != nil {
		if ctx.Err() != nil {
			return 0
		}
		d.log.Error("daemon: reproduce promotion failed", "err", err)
		return 0
	}
	if summary.Promoted > 0 {
		d.log.Info("daemon: reproduced findings promoted to T1",
			"promoted", summary.Promoted, "attempted", summary.Attempted)
	}
	return summary.Promoted
}

// countLines returns the number of lines in b. A file with content but no
// trailing newline still counts its final line; an empty file has zero lines.
// This is used only to detect "the claimed line is now past EOF", a cheap and
// honest proxy for "the implicated code is gone".
func countLines(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	n := 1
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	// A trailing newline means the last counted line is empty; the real last line
	// of content is n-1. Treat trailing-newline files as having n-1 content lines
	// so a finding on the final real line is not spuriously closed.
	if b[len(b)-1] == '\n' {
		n--
	}
	return n
}

// introducedSince returns the subset of findings whose anchor did not exist at
// the baseline commit — i.e. findings introduced after that commit. A per-finding
// git error biases toward INTRODUCED (see ingest.Repo.AnchorAbsentAtRef). It
// stops early on context cancellation, returning what it has gathered.
func introducedSince(ctx context.Context, repo *ingest.Repo, baseline string, findings []store.Finding) []store.Finding {
	var introduced []store.Finding
	for _, fnd := range findings {
		if ctx.Err() != nil {
			break
		}
		if repo.AnchorAbsentAtRef(ctx, baseline, fnd.File, fnd.Line) {
			introduced = append(introduced, fnd)
		}
	}
	return introduced
}

// emitRegressDigest logs a per-cycle "new since last green" digest: this cycle's
// findings whose anchor did not exist at the most recent finished sweep's commit
// (the 'last green' baseline) are reported as introduced-this-period, so an
// operator watching the daemon sees genuine regressions separated from
// pre-existing background. Best-effort and read-only: an empty finding set, a
// missing baseline (no prior sweep), or any store error logs and returns without
// affecting the cycle.
func (d *Daemon) emitRegressDigest(ctx context.Context, fres *funnel.Result) {
	if fres == nil || len(fres.Findings) == 0 {
		return
	}
	baseline, err := d.store.LastFinishedSweepCommit(ctx, fres.ScanRunID)
	if errors.Is(err, store.ErrNotFound) {
		return // no prior sweep to compare against (first sweep ever)
	}
	if err != nil {
		d.log.Error("daemon: regress digest: baseline lookup failed", "err", err)
		return
	}
	introduced := introducedSince(ctx, d.repo, baseline, fres.Findings)
	if len(introduced) == 0 {
		return
	}
	d.log.Info("daemon: regress digest: findings introduced since last green sweep",
		"baseline", util.ShortSHA(baseline),
		"introduced", len(introduced),
		"total", len(fres.Findings))
	for _, fnd := range introduced {
		d.log.Info("daemon: regress digest: introduced finding",
			"file", fnd.File, "line", fnd.Line,
			"severity", fnd.Severity, "title", fnd.Title)
	}
}
