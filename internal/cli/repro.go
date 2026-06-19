package cli

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/dpoage/bugbot/internal/daemon"
	"github.com/dpoage/bugbot/internal/sandbox"
)

// newReproCmd implements `bugbot repro`: a one-shot backlog drain that queries
// the store for open Tier-2/3 findings with no reproduction attempt and runs
// them through the reproduce+patch-prover pipeline.
//
// It mirrors the wiring of `scan --repro` (config load, store open, sandbox
// availability check with a graceful skip message) but sources its findings
// from daemon.OpenBacklog rather than from a scan result. The --max flag caps
// the batch size, defaulting to repro.backlog_batch from config.
//
// Note: this command does NOT consult the daemon per-day token budget. It is an
// operator-initiated one-shot that runs unconditionally, consistent with
// `scan --repro`. Operators should be aware of token costs when running against
// a large backlog.
func newReproCmd() *cobra.Command {
	var (
		target string
		maxN   int
	)

	cmd := &cobra.Command{
		Use:   "repro [flags]",
		Short: "One-shot backlog drain: reproduce open T2/T3 findings with no prior repro attempt",
		Long: `repro queries the store for open Tier-2 and Tier-3 findings that have
no reproduction attempt (ReproPath empty, NeedsHuman false) and runs them
through the reproduce+patch-prover pipeline, promoting demonstrated findings
to Tier-1 (or Tier-0 when the patch-prover witnesses a fix).

This is the same backlog logic the daemon runs on its periodic backlog timer,
exposed as a one-shot command for manual operation or scripted workflows.

Requires a container runtime (podman or docker) on PATH. When none is found
the command exits with a graceful message rather than an error.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}

			cfg, st, err := cmdOpenStore(ctx)
			if err != nil {
				return err
			}
			defer closeStore(st)

			// --max overrides the config default; 0 means "use config".
			batchSize := cfg.Repro.BacklogBatch
			if maxN > 0 {
				batchSize = maxN
			}

			runtime, ok := sandbox.Detect()
			if !ok {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(),
					"Repro backlog skipped: no container runtime (podman/docker) found on PATH.")
				return nil
			}

			backlog, err := daemon.OpenBacklog(ctx, st)
			if err != nil {
				return fmt.Errorf("query backlog: %w", err)
			}
			if len(backlog) == 0 {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Repro backlog: no eligible findings.")
				return nil
			}

			batch := backlog
			if len(batch) > batchSize {
				batch = batch[:batchSize]
			}

			// Build the reproducer using the same helper as the daemon command.
			// Ledger spend with an empty scan-run id: backlog findings span
			// multiple past runs, so there is no single run to attribute to.
			// This matches the daemon's backlog attribution choice.
			rd, err := buildReproducer(ctx, &cfg, st, target, runtime)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out,
				"\nRepro backlog: %d eligible, attempting %d (max=%d, runtime=%s)...\n",
				len(backlog), len(batch), batchSize, runtime,
			)

			summary, err := rd.repro.PromoteAll(ctx, st, batch)
			if err != nil {
				return fmt.Errorf("reproduce: %w", err)
			}
			printReproSummary(out, summary)

			// Touch attempted-but-not-promoted findings to bump updated_at so that
			// OpenBacklog's oldest-first ordering rotates them to the back of the
			// queue on the next run, preventing unbounded retries on the same
			// unreproducible findings.
			daemon.TouchBacklogFailures(ctx, st, slog.Default(), batch)
			return nil
		},
	}

	cmd.Flags().StringVar(&target, "target", ".", "path to the target repository")
	cmd.Flags().IntVar(&maxN, "max", 0,
		"maximum findings to attempt (0 = use repro.backlog_batch from config)")

	return cmd
}
