package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/store"
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
	res.closedF += d.reverifyOpenFindings(ctx)

	if fres != nil {
		res.promoted += d.promoteNewFindings(ctx, fres)
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
		// don't re-verify the same change repeatedly.
		fnd.FileHash = curHash
		fnd.Reasoning = reasoning
		if _, uerr := d.store.UpsertFinding(ctx, fnd); uerr != nil {
			d.log.Error("daemon: reverify: re-anchor failed", "fingerprint", fnd.Fingerprint, "err", uerr)
		} else {
			d.log.Info("daemon: finding re-verified, still open",
				"fingerprint", shortSHA(fnd.Fingerprint), "file", fnd.File, "line", fnd.Line)
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
		"fingerprint", shortSHA(fnd.Fingerprint),
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
		if fnd.Tier == 2 {
			t2 = append(t2, fnd)
		}
	}
	if len(t2) == 0 {
		return 0
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
