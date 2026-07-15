package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/engine"
	"github.com/dpoage/bugbot/internal/progress"
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
		target        string
		maxN          int
		transcriptDir string
		unsandboxed   bool
	)

	cmd := &cobra.Command{
		Use:   "repro [flags] [finding-id]",
		Short: "Reproduce open findings: backlog batch, or one finding attended (optionally unsandboxed)",
		Long: `repro queries the store for open Tier-2 and Tier-3 findings that have
no reproduction attempt (ReproPath empty, NeedsHuman false) and runs them
through the reproduce+patch-prover pipeline, promoting demonstrated findings
to Tier-1 (or Tier-0 when the patch-prover witnesses a fix).

With no arguments this is the same backlog logic the daemon runs on its
periodic backlog timer, exposed as a one-shot command for manual operation or
scripted workflows.

Given a single finding-id (or unambiguous id prefix), repro instead runs that
ONE finding attended, skipping the backlog entirely. --unsandboxed additionally
opts that single run out of the container: the repro command runs DIRECTLY ON
THE HOST, against a fresh workspace copy (never the live checkout), with no
network policy or resource caps. --unsandboxed requires a finding-id and is
refused for the backlog batch path — it exists for a human actively watching
one rerun, never for unattended use.

Requires a container runtime (podman or docker) on PATH for the sandboxed
paths; --unsandboxed does not. When no runtime is found and --unsandboxed is
not set, the command exits with a graceful message rather than an error.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}

			var findingID string
			if len(args) == 1 {
				findingID = args[0]
			}
			if unsandboxed && findingID == "" {
				return fmt.Errorf("repro: --unsandboxed requires a finding-id argument (single-finding attended rerun only)")
			}

			cfg, err := config.Load(configPathFromCmd(cmd))
			if err != nil {
				return err
			}

			// Live activity, no snapshot: `bugbot repro` wires the same
			// pane-or-log renderer scan uses (TTY -> in-place ANSI pane, else
			// plain log lines) so an operator watching this terminal sees repro
			// attempts as they happen. It deliberately does NOT add a
			// SnapshotSink: status.json has a single writer, and this one-shot
			// command may run alongside a live daemon that already owns it.
			errOut := cmd.ErrOrStderr()
			var (
				pane     *progress.PaneRenderer
				liveSink progress.EventSink
			)
			if progress.IsTerminal(errOut) {
				pane = progress.NewPaneRenderer(errOut, 0)
				liveSink = pane
			} else {
				liveSink = progress.NewLogRenderer(errOut)
			}
			stopPane := func() {
				if pane != nil {
					pane.Stop()
					pane = nil
				}
			}
			defer stopPane()

			d, err := engine.Open(ctx, cfg, liveSink)
			if err != nil {
				return err
			}
			defer func() { _ = d.Close() }()

			_, err = d.Repro(ctx, engine.ReproOpts{
				Target:        target,
				MaxN:          maxN,
				TranscriptDir: transcriptDir,
				Out:           cmd.OutOrStdout(),
				StopProgress:  stopPane,
				FindingID:     findingID,
				Unsandboxed:   unsandboxed,
			})
			return err
		},
	}

	addTargetFlag(cmd, &target)
	cmd.Flags().IntVar(&maxN, "max", 0,
		"maximum findings to attempt (0 = use repro.backlog_batch from config); ignored with a finding-id argument")
	cmd.Flags().StringVar(&transcriptDir, "transcript-dir", "",
		"write each reproducer agent transcript (JSONL) to this directory for "+
			"post-hoc diagnosis; overrides repro.transcript_dir (empty = use config / disabled)")
	cmd.Flags().BoolVar(&unsandboxed, "unsandboxed", false,
		"ATTENDED USE ONLY: run the given finding-id directly on the host (workspace copy, no container "+
			"isolation, full network access) instead of the sandbox; requires a finding-id argument")

	return cmd
}
