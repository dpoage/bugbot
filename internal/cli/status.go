package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/store"
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
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			path := progress.StatusPath(storageDir(cfg))

			st, rerr := progress.ReadStatus(path)
			if os.IsNotExist(rerr) {
				_, _ = fmt.Fprintln(out, "no bugbot activity recorded (no daemon or scan running against this config)")
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
		_, _ = fmt.Fprintf(out, "  scan:         kind=%s commit=%s\n", st.ScanKind, shortSHA(st.Commit))
	}
	if st.Stage != "" {
		_, _ = fmt.Fprintf(out, "  stage:        %s\n", st.Stage)
	}
	for _, a := range st.ActiveAgents {
		_, _ = fmt.Fprintf(out, "  agent:        %-8s %s\n", a.Role, a.Label)
	}
	_, _ = fmt.Fprintf(out, "  stages:       hypothesized=%d triaged=%d verified=%d killed=%d\n",
		st.Counts.Hypothesized, st.Counts.Triaged, st.Counts.Verified, st.Counts.Killed)
	_, _ = fmt.Fprintf(out, "  run spend:    in=%d out=%d total=%d tokens%s\n",
		st.SpendInput, st.SpendOutput, st.SpendInput+st.SpendOutput,
		cachedNote(st.SpendCacheRead))
	if st.SpendTodayInput > 0 || st.SpendTodayOutput > 0 {
		_, _ = fmt.Fprintf(out, "  today spend:  in=%d out=%d total=%d tokens\n",
			st.SpendTodayInput, st.SpendTodayOutput, st.SpendTodayInput+st.SpendTodayOutput)
	}
	if !st.NextPoll.IsZero() {
		_, _ = fmt.Fprintf(out, "  next poll:    %s\n", etaString(st.NextPoll, now))
	}
	if !st.NextSweep.IsZero() {
		_, _ = fmt.Fprintf(out, "  next sweep:   %s\n", etaString(st.NextSweep, now))
	}
	if st.LastEvent != "" {
		_, _ = fmt.Fprintf(out, "  last event:   %s\n", st.LastEvent)
	}

	// Open findings count: open the store read-only-ish (a normal open is fine;
	// we only read) and count. Best-effort — a count failure is noted, not fatal,
	// since status is informational.
	if n, err := openFindingsCount(ctx, cfg); err == nil {
		_, _ = fmt.Fprintf(out, "  open findings: %d\n", n)
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

// openFindingsCount opens the store and returns the number of open findings.
func openFindingsCount(ctx context.Context, cfg config.Config) (int, error) {
	st, err := store.Open(ctx, cfg.Storage.Path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = st.Close() }()
	open, err := st.ListFindings(ctx, store.FindingFilter{Status: store.StatusOpen})
	if err != nil {
		return 0, err
	}
	return len(open), nil
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

// fmtTime renders a timestamp, or "-" when zero.
func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format("2006-01-02 15:04:05")
}
