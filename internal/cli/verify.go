package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/dpoage/bugbot/internal/funnel"
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

			cfg, st, err := cmdOpenStore(ctx, configPathFromCmd(cmd))
			if err != nil {
				return err
			}
			defer closeStore(st)

			// Advisory scan-lock: mirrors bugbot scan's checkScanLock so a
			// manual verify does not race the daemon or a concurrent scan.
			if lockErr := checkScanLock(ctx, st, force, os.Getpid()); lockErr != nil {
				return lockErr
			}

			repo, err := openRepoForScan(ctx, target)
			if err != nil {
				return err
			}

			finder, verifier, cartographer, err := buildRoleClients(ctx, &cfg)
			if err != nil {
				return err
			}

			// Default `bugbot verify` stays quiet and never writes a status.json
			// snapshot (which would race a running daemon's single-writer
			// snapshot). With --suspected the re-verify pass runs the verifier on
			// every open Tier-3 finding and can take minutes, so attach a stdout
			// LogRenderer — plain log lines only, no status.json — for live
			// per-stage / per-finding feedback.
			opts, _, sbErr := buildFunnelOptions(cfg, FunnelOptionOverrides{})
			if sbErr != nil {
				return sbErr
			}
			if suspected {
				opts.Progress = progress.NewLogRenderer(cmd.OutOrStdout())
			} else {
				opts.Progress = nil
			}

			f, err := funnel.New(funnel.RoleClients{
				Finder:       finder,
				Verifier:     verifier,
				Cartographer: cartographer,
			}, st, repo, opts)
			if err != nil {
				return err
			}
			defer func() { _ = f.Close() }()

			// Heartbeat goroutine: periodically refreshes our scan_run row so
			// ActiveScanRuns can distinguish us from a crashed/stale process.
			// Mirrors the pattern in runScanCmd (scan.go).
			hbCtx, hbCancel := context.WithCancel(ctx)
			defer hbCancel()
			go runHeartbeat(hbCtx, st, os.Getpid())

			out := cmd.OutOrStdout()

			res, err := f.VerifyDrain(ctx)
			if err != nil {
				return fmt.Errorf("verify drain: %w", err)
			}

			didDrain := res != nil && (res.Stats.Resumed > 0 || len(res.Findings) > 0)
			if didDrain {
				_, _ = fmt.Fprintf(out,
					"\nVerify drain: %d resumed, %d finding(s) persisted.\n",
					res.Stats.Resumed, len(res.Findings),
				)
			} else {
				_, _ = fmt.Fprintln(out, "Verify drain: no pending candidates.")
			}

			// --suspected: second pass over durable open T3 findings (orphans
			// from a hard-budget stop or no-verdict panel). The finder stays
			// off; the verifier re-judges each durable T3 against current code,
			// promoting survivors to Tier 2 or dismissing refuted ones. Only
			// run when the flag is set so the default behaviour is byte-
			// identical to today.
			if suspected {
				_, _ = fmt.Fprintln(out,
					"\nRe-verifying open Tier-3 suspected findings (verifier only, no finder; this can take a few minutes)…")
				rres, rerr := f.ReverifySuspected(ctx)
				if rerr != nil {
					return fmt.Errorf("reverify suspected: %w", rerr)
				}
				if rres == nil {
					rres = &funnel.Result{}
				}
				_, _ = fmt.Fprintf(out,
					"Re-verify suspected: %d re-judged, %d verified, %d killed.\n",
					rres.Stats.Resumed, rres.Stats.Verified, rres.Stats.Killed,
				)
			}
			return nil
		},
	}

	addTargetFlag(cmd, &target)
	cmd.Flags().BoolVar(&force, "force", false,
		"bypass the advisory single-scan lock and proceed even if another scan appears active")
	cmd.Flags().BoolVar(&suspected, "suspected", false,
		"also re-verify open Tier-3 suspected findings (re-runs the verifier without the finder)")

	return cmd
}
