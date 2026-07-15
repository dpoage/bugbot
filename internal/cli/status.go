package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/ecosystem"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/util"
)

// staleAfter is how long without a status update before a running scan/daemon is
// treated as stale. The daemon writes at least once per cycle and the snapshot
// sink rate-limits to ~1s during active work; a couple of minutes of silence
// means the writer is wedged or gone.
const staleAfter = 2 * time.Minute

// storageDir returns the directory holding the state DB, which is also where the
// status.json snapshot lives (a sibling of state.db). It mirrors how the
// snapshot sink derives its path from cfg.Storage.Path.
func storageDir(cfg config.Config) string {
	return filepath.Dir(cfg.Storage.Path)
}

// newStatusCmd reports the current activity of a running scan or daemon by
// reading the status.json snapshot beside the state DB. It is purely
// informational: a missing or stale file is reported clearly, and the command
// exits 0 in every informational case (no file, stale, fresh) — only a genuine
// I/O/parse failure is an error.
func newStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the current activity of a running scan or daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			cfg, err := config.Load(configPathFromCmd(cmd))
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			path := progress.StatusPath(storageDir(cfg))

			st, rerr := progress.ReadStatus(path)
			if os.IsNotExist(rerr) {
				// No live activity — but the accumulated world state is still
				// the point of this command. Render it when a store exists.
				_, _ = fmt.Fprintln(out, "no bugbot activity recorded (no daemon or scan running against this config)")
				if ws, ok := fetchWorldState(ctx, cfg); ok {
					renderWorldState(out, ws, time.Now())
				}
				return nil
			}
			if rerr != nil {
				return fmt.Errorf("read status %q: %w", path, rerr)
			}

			renderStatus(ctx, out, cfg, st, time.Now())
			return nil
		},
	}
	return cmd
}

// renderStatus prints a human-readable view of a status snapshot, classifying it
// as fresh or stale. now is injectable for tests.
func renderStatus(ctx context.Context, out io.Writer, cfg config.Config, st progress.Status, now time.Time) {
	stale := isStale(st, now)

	if stale {
		_, _ = fmt.Fprintln(out, "bugbot looks stale or crashed (no recent update; last-known state below)")
	} else {
		_, _ = fmt.Fprintln(out, "bugbot is active")
	}

	_, _ = fmt.Fprintf(out, "  pid:          %d%s\n", st.PID, pidNote(st.PID))
	_, _ = fmt.Fprintf(out, "  started:      %s\n", fmtTime(st.StartedAt))
	_, _ = fmt.Fprintf(out, "  last update:  %s (%s ago)\n", fmtTime(st.LastUpdated), now.Sub(st.LastUpdated).Round(time.Second))
	if st.ScanKind != "" {
		_, _ = fmt.Fprintf(out, "  scan:         kind=%s commit=%s\n", st.ScanKind, util.ShortSHA(st.Commit))
	}
	if st.Stage != "" {
		_, _ = fmt.Fprintf(out, "  stage:        %s\n", st.Stage)
	}
	for _, a := range st.ActiveAgents {
		// Pad to the widest role name ("cartographer"/"patch-prover" = 12) so
		// the label column stays aligned across all agent roles.
		line := fmt.Sprintf("  agent:        %-12s %s", a.Role, a.Label)
		if a.Activity != "" {
			line += "  [" + a.Activity + "]"
		}
		_, _ = fmt.Fprintln(out, line)
	}
	_, _ = fmt.Fprintln(out, "  stages:       "+fmtStageCounts(st))
	_, _ = fmt.Fprintf(out, "  run spend:    in=%d out=%d total=%d tokens%s\n",
		st.SpendInput, st.SpendOutput, st.SpendInput+st.SpendOutput,
		cachedNote(st.SpendCacheRead))
	// Today's spend intentionally lives in the world-state block below (same
	// numbers from the store, plus the day-budget percentage) — printing it
	// here too produced two near-identical lines.
	if !st.NextPoll.IsZero() {
		_, _ = fmt.Fprintf(out, "  next poll:    %s\n", etaString(st.NextPoll, now))
	}
	if !st.NextSweep.IsZero() {
		_, _ = fmt.Fprintf(out, "  next sweep:   %s\n", etaString(st.NextSweep, now))
	}
	if st.LastEvent != "" {
		_, _ = fmt.Fprintf(out, "  last event:   %s\n", st.LastEvent)
	}
	if len(st.ReproBlocked) > 0 {
		ecos := make([]string, 0, len(st.ReproBlocked))
		for eco := range st.ReproBlocked {
			ecos = append(ecos, eco)
		}
		sort.Strings(ecos)
		for _, eco := range ecos {
			binary := ecosystem.BaseMode(ecosystem.Ecosystem(eco))
			if binary == "" {
				binary = eco
			}
			_, _ = fmt.Fprintf(out, "  blocked:      %d finding(s) — image lacks %s\n", st.ReproBlocked[eco], binary)
		}
	}

	// The accumulated world state (findings, blackboard, sync, spend, last
	// run). Best-effort — fetch failures degrade section by section, since
	// status is informational.
	if ws, ok := fetchWorldState(ctx, cfg); ok {
		renderWorldState(out, ws, now)
	}
}

// isStale reports whether the snapshot looks dead: either its last update is
// older than staleAfter, or its writer process is gone.
func isStale(st progress.Status, now time.Time) bool {
	if !st.LastUpdated.IsZero() && now.Sub(st.LastUpdated) > staleAfter {
		return true
	}
	return !processAlive(st.PID)
}

// processAlive reports whether a process with the given pid exists, via signal 0
// (which performs error checking without actually delivering a signal). A pid of
// 0 or a not-found process reports false.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// On Unix, Signal(0) returns nil if the process exists and we may signal it,
	// and an error (ESRCH) if it does not.
	err = proc.Signal(syscall.Signal(0))
	return err == nil
}

// pidNote annotates a pid with whether it is still alive.
func pidNote(pid int) string {
	if processAlive(pid) {
		return " (alive)"
	}
	return " (not running)"
}

// cachedNote annotates a spend line with the cache-read token count, or ""
// when no cache activity was reported.
func cachedNote(cached int64) string {
	if cached == 0 {
		return ""
	}
	return fmt.Sprintf(" (cached %d)", cached)
}

// etaString formats a future deadline relative to now (e.g. "in 42s"), or
// "due now" when it is past.
func etaString(t, now time.Time) string {
	d := t.Sub(now)
	if d <= 0 {
		return "due now"
	}
	return fmt.Sprintf("in %s (%s)", d.Round(time.Second), t.Format("15:04:05"))
}

// fmtStageCounts formats the stages line for renderStatus. When live counters
// are non-zero the line appends parenthetical in-progress notes so operators
// see accumulating candidates, verified, and killed counts before the stage
// finishes and the authoritative Counts fields take over.
//
// Presentation contract:
//   - During hypothesize (LiveCandidates > 0): appends "(candidates so far: N)"
//     after hypothesized=0 so it is obvious the final count is not yet settled.
//   - During verify (LiveVerified > 0 or LiveKilled > 0): appends per-field
//     live notes so operators see verdicts accumulating.
//   - After stage-finish: live fields are zero; the line is the unadorned
//     "hypothesized=H triaged=T verified=V killed=K dup_rate=R" format, R
//     being funnel.Stats.DuplicateRate() carried through Status.Counts.
func fmtStageCounts(st progress.Status) string {
	hyp := fmt.Sprintf("hypothesized=%d", st.Counts.Hypothesized)
	if st.LiveCandidates > 0 {
		hyp += fmt.Sprintf(" (candidates so far: %d)", st.LiveCandidates)
	}

	ver := fmt.Sprintf("verified=%d", st.Counts.Verified)
	if st.LiveVerified > 0 {
		ver += fmt.Sprintf(" (so far: %d)", st.LiveVerified)
	}

	kil := fmt.Sprintf("killed=%d", st.Counts.Killed)
	if st.LiveKilled > 0 {
		kil += fmt.Sprintf(" (so far: %d)", st.LiveKilled)
	}

	tri := fmt.Sprintf("triaged=%d", st.Counts.Triaged)
	if st.LiveTriaged > 0 {
		tri += fmt.Sprintf(" (so far: %d)", st.LiveTriaged)
	}

	dup := fmt.Sprintf("dup_rate=%.2f", st.Counts.DuplicateRate)

	return fmt.Sprintf("%s %s %s %s %s", hyp, tri, ver, kil, dup)
}

// fmtTime renders a timestamp, or "-" when zero.
func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format("2006-01-02 15:04:05")
}
