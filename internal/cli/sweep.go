package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/ingest"
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

			cfg, st, err := cmdOpenStore(ctx, configPathFromCmd(cmd))
			if err != nil {
				return err
			}
			defer closeStore(st)

			// Advisory scan lock: mirror bugbot scan's behaviour so concurrent
			// drains and scans are detected and reported gracefully.
			selfPID := os.Getpid()
			if err := checkScanLock(ctx, st, force, selfPID); err != nil {
				return err
			}
			// Heartbeat goroutine: keeps the scan-run row alive so the advisory
			// lock in checkScanLock can distinguish live from stale processes.
			hbCtx, hbCancel := context.WithCancel(ctx)
			defer hbCancel()
			go runHeartbeat(hbCtx, st, selfPID)

			repo, err := ingest.Open(ctx, target)
			if err != nil {
				return fmt.Errorf("open target: %w", err)
			}

			finder, verifier, cartographer, arbiter, err := buildRoleClients(ctx, &cfg)
			if err != nil {
				return err
			}

			// nil progress sink: one-shot sweep prints its summary to stdout and
			// does not own a status.json snapshot (the daemon/scan do).
			opts, sandboxDegraded, err := buildFunnelOptions(cfg, FunnelOptionOverrides{
				Progress: nil,
			})
			if err != nil {
				return err
			}
			if sandboxDegraded {
				printSandboxDegradedWarning(cmd.ErrOrStderr())
			}

			f, err := funnel.New(funnel.RoleClients{
				Finder:       finder,
				Verifier:     verifier,
				Cartographer: cartographer,
				Arbiter:      arbiter,
			}, st, repo, opts)
			if err != nil {
				return err
			}
			defer func() { _ = f.Close() }()

			result, err := f.SweepDrain(ctx)
			if err != nil {
				return fmt.Errorf("sweep drain: %w", err)
			}

			out := cmd.OutOrStdout()
			if len(result.Findings) == 0 {
				_, _ = fmt.Fprintln(out, "Impact sweep: no unswept open findings.")
				return nil
			}
			_, _ = fmt.Fprintf(out, "Impact sweep: %d finding(s) swept (scan_run_id=%s).\n",
				len(result.Findings), result.ScanRunID)
			return nil
		},
	}

	addTargetFlag(cmd, &target)
	cmd.Flags().BoolVar(&force, "force", false,
		"override advisory scan lock (ignore a concurrently running scan)")

	return cmd
}
