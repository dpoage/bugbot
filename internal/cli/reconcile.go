package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/engine"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/store"
)

// newReconcileCmd implements `bugbot reconcile`: a one-shot backlog-reconcile
// pass that heals duplicate OPEN findings already persisted in the store. It
// groups OPEN findings by file, nominates candidate pairs under the same
// deterministic pre-gate the live in-scan collision sites use, and confirms
// each nomination through the dedup arbiter before merging — never an
// auto-merge on the deterministic gate alone.
//
// This is the same backlog-reconcile logic the daemon runs automatically on
// its daemon.reconcile_interval timer (default 24h), exposed as a one-shot
// command for manual operation, scripted workflows, or immediate on-demand
// triggering without waiting for the next timer tick.
func newReconcileCmd() *cobra.Command {
	var cap int

	cmd := &cobra.Command{
		Use:   "reconcile [flags]",
		Short: "One-shot backlog reconcile: merge duplicate OPEN findings via the dedup arbiter",
		Long: `reconcile groups currently-OPEN findings by file and nominates duplicate
candidate pairs under the same deterministic pre-gate (file/window, compatible
defect_kind, SimilarFinding-close descriptions) the live in-scan collision
sites use. Every nomination is confirmed by the dedup arbiter before merging;
a "no"/"unsure"/failed verdict always leaves both findings untouched.

The older (earlier-created) row of a confirmed pair survives as canonical:
the newer row's sites and corroborating lenses are folded onto it, then the
newer row is closed StatusSuperseded with a reason referencing the canonical
fingerprint.

--cap bounds the number of dedup-arbiter invocations this pass may spend
(default funnel.DefaultReconcileCap = 25); nominated pairs beyond the cap are
skipped and left open for the next pass. Idempotent: re-running immediately
after a merge nominates nothing (findings closed StatusSuperseded are
excluded from the OPEN query).

The daemon runs this same pass automatically every daemon.reconcile_interval
(default 24h). This command is for triggering a pass on demand — e.g. right
after a scan is suspected to have produced backlog duplicates — without
waiting for the next scheduled cycle.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			// Wire SIGINT/SIGTERM → context cancellation for interrupt-safe
			// finalization, matching scan/verify.
			ctx, stopSignal := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
			defer stopSignal()

			cfg, err := config.Load(configPathFromCmd(cmd))
			if err != nil {
				return err
			}

			// A reconcile pass calls the dedup arbiter (an LLM round-trip per
			// nominated pair) and can take a little while; a plain stdout log
			// renderer gives live per-call feedback without owning a
			// status.json snapshot (which would race a running daemon's).
			sink := progress.NewLogRenderer(cmd.OutOrStdout())

			d, err := engine.Open(ctx, cfg, sink)
			if err != nil {
				return actionableLockError(err)
			}
			defer func() { _ = d.Close() }()

			res, err := d.Reconcile(ctx, engine.ReconcileOpts{
				Cap: cap,
				Out: cmd.OutOrStdout(),
			})
			if err != nil {
				return actionableLockError(err)
			}

			printReconcileResult(cmd.OutOrStdout(), res)
			return nil
		},
	}

	cmd.Flags().IntVar(&cap, "cap", 0,
		"max dedup-arbiter invocations for this pass (0 = funnel.DefaultReconcileCap)")

	return cmd
}

// actionableLockError rewraps a *store.ErrLocked so the operator is told what
// to do instead of just that the database is busy: reconcile is a write verb
// that always needs the writer lock (Dispatcher.Reconcile escalates
// unconditionally), so a daemon already holding it here is not something
// this command can wait out or retry past. Any other error passes through
// unchanged.
func actionableLockError(err error) error {
	var locked *store.ErrLocked
	if !errors.As(err, &locked) {
		return err
	}
	return fmt.Errorf("%w\nthe daemon already runs this pass automatically on its "+
		"reconcile_interval — trigger it on demand instead of running `bugbot reconcile` "+
		"directly: use the TUI command palette (`bugbot tui`, if attached to the running "+
		"daemon) or dispatch a \"reconcile\" verb over the control socket (see "+
		"internal/control), or stop the daemon first if you need this CLI command itself",
		locked)
}

// printReconcileResult writes the supplemental summary line(s) Dispatcher.
// Reconcile does not print itself: the nominated/arbitrated/merged/skipped
// counts are self-printed to opts.Out by Reconcile (mirroring Sweep's
// self-printed one-liner); this adds the failure count and spend, matching
// the division of labor scan.go's printResult uses for the fields Dispatcher.
// Scan does not print.
func printReconcileResult(out io.Writer, res *engine.ReconcileResult) {
	s := res.Result.Stats
	if s.ReconcileFailures > 0 {
		_, _ = fmt.Fprintf(out, "Backlog reconcile: %d arbiter call(s) produced no parseable verdict (treated as unsure, no merge)\n",
			s.ReconcileFailures)
	}
	if s.InputTokens > 0 || s.OutputTokens > 0 {
		_, _ = fmt.Fprintf(out, "Spend: input=%d output=%d total=%d tokens\n",
			s.InputTokens, s.OutputTokens, s.InputTokens+s.OutputTokens)
		if s.CacheReadTokens > 0 || s.CacheCreationTokens > 0 {
			_, _ = fmt.Fprintf(out, "Cache: read=%d created=%d tokens (of input; reads bill at a steep discount)\n",
				s.CacheReadTokens, s.CacheCreationTokens)
		}
	}
}
