package cli

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/engine"
)

// newSweepCmd implements `bugbot sweep`: a one-shot impact-sweep drain that
// re-ranks unswept open findings by reachability. It mirrors the advisory
// scan-lock and heartbeat pattern of `bugbot scan` so concurrent runs are
// detected gracefully.
//
// The command is defined here but NOT registered in root.go; Main wires it.
func newSweepCmd() *cobra.Command {
	var (
		target string
		force  bool
	)

	cmd := &cobra.Command{
		Use:   "sweep [flags]",
		Short: "One-shot impact-sweep drain: re-rank unswept open findings by reachability",
		Long: `sweep queries the store for open findings whose swept_at marker is NULL
and re-ranks their severity by reachability/impact analysis. It is idempotent:
a second run over already-swept findings is a verified no-op (UnsweptOpenFindings
returns empty, no LLM calls are made).

This is the same impact-sweep logic the daemon runs after every postCycle,
exposed as a one-shot command for manual operation or scripted workflows.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}

			cfg, err := config.Load(configPathFromCmd(cmd))
			if err != nil {
				return err
			}

			// nil progress sink: one-shot sweep prints its summary to stdout
			// and does not own a status.json snapshot (the daemon/scan do).
			d, err := engine.Open(ctx, cfg, nil)
			if err != nil {
				return err
			}
			defer func() { _ = d.Close() }()

			_, err = d.Sweep(ctx, engine.SweepOpts{
				Target: target,
				Force:  force,
				Out:    cmd.OutOrStdout(),
				ErrOut: cmd.ErrOrStderr(),
			})
			return err
		},
	}

	addTargetFlag(cmd, &target)
	cmd.Flags().BoolVar(&force, "force", false,
		"override advisory scan lock (ignore a concurrently running scan)")

	return cmd
}
