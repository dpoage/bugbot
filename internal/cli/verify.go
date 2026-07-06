package cli

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/engine"
	"github.com/dpoage/bugbot/internal/progress"
)

// newVerifyCmd implements `bugbot verify`: a one-shot verify drain that picks
// up pending_candidates left by any interrupted run and verifies them through
// the normal triage+verify pipeline WITHOUT invoking the finder/cartographer.
//
// Like `bugbot scan`, it acquires an advisory scan-lock and spawns a heartbeat
// goroutine so a concurrently-running daemon does not mistake it for a stale
// process. Pass --force to bypass the lock when you know what you are doing.
//
// Note: newVerifyCmd is defined but NOT registered on the root command here.
// Registration happens in root.go (owned by Main) to avoid cross-agent
// conflicts during the epic.
func newVerifyCmd() *cobra.Command {
	var (
		target    string
		force     bool
		suspected bool
	)

	cmd := &cobra.Command{
		Use:   "verify [flags]",
		Short: "One-shot verify drain: verify pending candidates left by interrupted runs",
		Long: `verify picks up any pending_candidates rows (the write-ahead log of an
interrupted or budget-truncated scan) and runs them through the triage and
verification pipeline without invoking the finder or cartographer.

This is the same verify-drain logic the daemon runs on its periodic verify-drain
timer, exposed as a one-shot command for manual operation or scripted workflows.

The command is idempotent: a second run on a drained store is a no-op (single
store query, no LLM calls).

Pass --suspected to also re-verify every OPEN Tier-3 suspected finding: durable
orphans from a hard-budget stop or no-verdict panel that have no pending_candidates
WAL row. This is a second pass over the same funnel pipeline — finder/cartographer
remain off — but re-judges the durable T3 rows against the current code so they
can be promoted to Tier 2 or dismissed as refuted. Without --suspected the T3
orphans are left untouched.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			// Wire SIGINT/SIGTERM → context cancellation for interrupt-safe
			// finalization (scan_runs row sealed with interrupted=true on Ctrl-C).
			ctx, stopSignal := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
			defer stopSignal()

			cfg, err := config.Load(configPathFromCmd(cmd))

			// Default `bugbot verify` stays quiet and never writes a status.json
			// snapshot (which would race a running daemon's single-writer
			// snapshot). With --suspected the re-verify pass runs the verifier on
			// every open Tier-3 finding and can take minutes, so attach a stdout
			// LogRenderer — plain log lines only, no status.json — for live
			// per-stage / per-finding feedback.
			var sink progress.EventSink
			if suspected {
				sink = progress.NewLogRenderer(cmd.OutOrStdout())
			}

			d, err := engine.Open(ctx, cfg, sink)
			if err != nil {
				return err
			}
			defer func() { _ = d.Close() }()

			_, err = d.Verify(ctx, engine.VerifyOpts{
				Target:    target,
				Force:     force,
				Suspected: suspected,
				Out:       cmd.OutOrStdout(),
			})
			return err
		},
	}

	addTargetFlag(cmd, &target)
	cmd.Flags().BoolVar(&force, "force", false,
		"bypass the advisory single-scan lock and proceed even if another scan appears active")
	cmd.Flags().BoolVar(&suspected, "suspected", false,
		"also re-verify open Tier-3 suspected findings (re-runs the verifier without the finder)")

	return cmd
}
