package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/daemon"
	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/report"
	"github.com/dpoage/bugbot/internal/repro"
	"github.com/dpoage/bugbot/internal/sandbox"
	"github.com/dpoage/bugbot/internal/store"
)

// newDaemonCmd runs Bugbot continuously: it polls the target repository for new
// commits (driving blast-radius-scoped targeted investigations), runs periodic
// whole-repo sweeps, re-verifies and auto-closes findings whose code changed,
// optionally reproduces verified findings, and emits reports through the
// configured sinks — all under per-cycle and per-day token budgets with idle
// backoff. It runs until SIGINT/SIGTERM, then shuts down gracefully after the
// current cycle's persistence completes.
func newDaemonCmd() *cobra.Command {
	var (
		repoPath string
		doRepro  bool
	)

	cmd := &cobra.Command{
		Use:   "daemon [flags]",
		Short: "Run Bugbot continuously with polling, sweeps, and idle backoff",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// SIGINT/SIGTERM cancel the daemon's context; the loop observes the
			// cancellation at the next cycle boundary and returns gracefully.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}

			st, err := store.Open(ctx, cfg.Storage.Path)
			if err != nil {
				return fmt.Errorf("open store: %w", err)
			}
			defer func() { _ = st.Close() }()

			repo, err := ingest.Open(ctx, repoPath)
			if err != nil {
				return fmt.Errorf("open target: %w", err)
			}

			finder, err := llm.ResolveRole(ctx, &cfg, "finder", llm.Options{})
			if err != nil {
				return fmt.Errorf("build finder client: %w", err)
			}
			verifier, err := llm.ResolveRole(ctx, &cfg, "verifier", llm.Options{})
			if err != nil {
				return fmt.Errorf("build verifier client: %w", err)
			}

			sinks, err := report.SinksFromConfig(cfg)
			if err != nil {
				return err
			}
			// Route stdout sinks through the command writer so output is captured
			// and respects redirection, consistent with `report emit`.
			for _, s := range sinks {
				if ss, ok := s.(*report.StdoutSink); ok {
					ss.W = cmd.OutOrStdout()
				}
			}

			logger := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{Level: slog.LevelInfo}))

			// Activity visibility: bridge progress events onto the daemon's slog
			// logger, and maintain a status.json snapshot beside the state DB so
			// `bugbot status` can report the running daemon's activity, today's
			// spend, and next poll/sweep ETAs from another terminal.
			snap := progress.NewSnapshotSink(storageDir(cfg)).
				WithDaySpend(daySpendGetter(ctx, st))
			progressSink := progress.NewMulti(progress.NewSlogRenderer(logger), snap)

			sbOpts, sbDegraded, sbErr := buildSandboxOpts(cfg)
			if sbErr != nil {
				return sbErr
			}
			if sbDegraded {
				logger.Warn(sandboxDegradedWarning)
			}

			deps := daemon.Deps{
				Repo:    repo,
				Store:   st,
				Clients: funnel.RoleClients{Finder: finder, Verifier: verifier},
				FunnelOpts: funnel.Options{
					Filter:      ingest.ScanFilter{Include: cfg.Scan.Include, Exclude: cfg.Scan.Exclude},
					TokenBudget: cfg.Budgets.PerCycleTokens,
					SandboxOpts: sbOpts,
				},
				Sinks:    sinks,
				Logger:   logger,
				Progress: progressSink,
			}

			// Reproduction is opt-in (--repro) and only wired when a container
			// runtime is available; otherwise the daemon runs without the
			// reproduce stage. Sandbox availability is surfaced in the banner.
			sandboxRuntime, sandboxOK := sandbox.Detect()
			if doRepro && sandboxOK {
				reproducer, rerr := buildReproducer(ctx, &cfg, repo.Root(), sandboxRuntime)
				if rerr != nil {
					return rerr
				}
				deps.ReproClient = reproducer.client
				deps.Reproducer = reproducer.repro
			}

			// Publish hook: wire in when cfg.Publish.Enabled. We do not
			// pre-check for gh on PATH here; a missing gh binary will produce a
			// warning on the first post-cycle run via the Publisher interface.
			if cfg.Publish.Enabled {
				deps.Publisher = NewStorePublisher(realGH, st, cfg.Publish, logger)
			}

			dcfg := daemon.DaemonConfig{
				PollInterval:   cfg.Daemon.PollInterval,
				SweepInterval:  cfg.Daemon.SweepInterval,
				IdleBackoff:    cfg.Daemon.IdleBackoff,
				PerCycleTokens: cfg.Budgets.PerCycleTokens,
				PerDayTokens:   cfg.Budgets.PerDayTokens,
				EnableRepro:    doRepro && sandboxOK && deps.Reproducer != nil,
			}

			d, err := daemon.New(deps, dcfg)
			if err != nil {
				return err
			}

			printDaemonBanner(cmd, cfg, dcfg, sinks, sandboxRuntime, sandboxOK)

			return d.Run(ctx)
		},
	}

	cmd.Flags().StringVar(&repoPath, "repo", ".", "path to the target repository")
	cmd.Flags().BoolVar(&doRepro, "repro", false, "enable the Reproduce stage (promote verified findings to Tier-1 via sandboxed failing tests; requires podman/docker)")

	return cmd
}

// daySpendGetter returns a function the snapshot sink calls to fill in today's
// total token spend (input, output) for the status snapshot. It sums spend since
// midnight UTC, matching the daemon's per-day budget window. Errors yield zeros,
// keeping the getter non-failing per the snapshot sink's contract.
func daySpendGetter(ctx context.Context, st *store.Store) func() (int64, int64) {
	return func() (int64, int64) {
		now := time.Now().UTC()
		midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		totals, err := st.TotalsSince(ctx, midnight)
		if err != nil {
			return 0, 0
		}
		return totals.InputTokens, totals.OutputTokens
	}
}

// reproDeps bundles a constructed reproducer with its LLM client so the daemon
// can record both.
type reproDeps struct {
	client llm.Client
	repro  *repro.Reproducer
}

// buildReproducer constructs the reproducer-role LLM client, sandbox, and
// Reproducer used by the daemon's post-cycle promotion step. It mirrors the
// wiring in `scan --repro`.
func buildReproducer(ctx context.Context, cfg *config.Config, repoRoot, runtime string) (*reproDeps, error) {
	client, err := llm.ResolveRole(ctx, cfg, "reproducer", llm.Options{})
	if err != nil {
		return nil, fmt.Errorf("build reproducer client: %w", err)
	}
	sb, err := sandbox.NewCLI(runtime, cfg.Sandbox.Image)
	if err != nil {
		return nil, fmt.Errorf("build sandbox: %w", err)
	}
	r, err := repro.New(client, sb, repoRoot, repro.Options{Image: cfg.Sandbox.Image})
	if err != nil {
		return nil, fmt.Errorf("build reproducer: %w", err)
	}
	return &reproDeps{client: client, repro: r}, nil
}

// printDaemonBanner prints the startup banner: intervals, budgets, sinks, and
// sandbox availability, so an operator can confirm the configuration at a glance.
func printDaemonBanner(cmd *cobra.Command, cfg config.Config, dcfg daemon.DaemonConfig, sinks []report.Sink, runtime string, sandboxOK bool) {
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintln(out, "bugbot daemon starting")
	_, _ = fmt.Fprintf(out, "  poll interval:    %s\n", dcfg.PollInterval)
	_, _ = fmt.Fprintf(out, "  sweep interval:   %s\n", dcfg.SweepInterval)
	_, _ = fmt.Fprintf(out, "  idle backoff:     %s\n", dcfg.IdleBackoff)
	_, _ = fmt.Fprintf(out, "  per-cycle tokens: %d\n", dcfg.PerCycleTokens)
	_, _ = fmt.Fprintf(out, "  per-day tokens:   %d\n", dcfg.PerDayTokens)
	sinkNames := make([]string, len(sinks))
	for i, s := range sinks {
		sinkNames[i] = s.Name()
	}
	_, _ = fmt.Fprintf(out, "  sinks:            %v\n", sinkNames)
	if sandboxOK {
		_, _ = fmt.Fprintf(out, "  sandbox:          %s (repro %s)\n", runtime, enabledLabel(dcfg.EnableRepro))
	} else {
		_, _ = fmt.Fprintln(out, "  sandbox:          none (no podman/docker; repro disabled)")
	}
	_, _ = fmt.Fprintln(out, "  press Ctrl-C to stop")
}

func enabledLabel(on bool) string {
	if on {
		return "enabled"
	}
	return "available but disabled (pass --repro)"
}
