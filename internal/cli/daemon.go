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

			cfg, st, err := cmdOpenStore(ctx, configPathFromCmd(cmd))
			if err != nil {
				return err
			}
			defer closeStore(st)

			repo, err := ingest.Open(ctx, repoPath)
			if err != nil {
				return fmt.Errorf("open target: %w", err)
			}

			finder, verifier, cartographer, arbiter, err := buildRoleClients(ctx, &cfg)
			if err != nil {
				return err
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

			funnelOpts, sbDegraded, sbErr := buildFunnelOptions(cfg, FunnelOptionOverrides{})
			if sbErr != nil {
				return sbErr
			}
			if sbDegraded {
				logger.Warn(sandboxDegradedWarning)
			}

			deps := daemon.Deps{
				Repo:       repo,
				Store:      st,
				Clients:    funnel.RoleClients{Finder: finder, Verifier: verifier, Cartographer: cartographer, Arbiter: arbiter},
				FunnelOpts: funnelOpts,
				Sinks:      sinks,
				Logger:     logger,
				Progress:   progressSink,
			}

			// Reproduction is opt-in (--repro) and only wired when a container
			// runtime is available; otherwise the daemon runs without the
			// reproduce stage. Sandbox availability is surfaced in the banner.
			sandboxRuntime, sandboxOK := sandbox.Detect()
			if doRepro && sandboxOK {
				// Preflight: probe the sandbox toolchain once before wiring the
				// reproduce stage; a toolchain-less image would turn every backlog
				// drain into per-finding environment_error burn (bugbot-u6td).
				if verdict, vErr := repro.VerifySandboxOnce(ctx, repo.Root(), cfg); vErr == nil && verdict.BlocksRepro() {
					diag := fmt.Sprintf("repro stage disabled: sandbox toolchain check failed (%s): %s — run `bugbot doctor` and set sandbox.image to a toolchain-capable image",
						verdict.Category, verdict.Detail)
					logger.Error(diag)
					_, _ = fmt.Fprintln(cmd.ErrOrStderr(), diag)
					doRepro = false
				}
			}
			if doRepro && sandboxOK {
				reproducer, rerr := buildReproducer(ctx, &cfg, st, repo.Root(), sandboxRuntime, progressSink)
				if rerr != nil {
					return rerr
				}
				defer reproducer.repro.Close() //nolint:errcheck
				deps.ReproClient = reproducer.client
				deps.Reproducer = reproducer.repro
				deps.ReproTagger = reproducer.spend
			}

			// Analyzer seeding hook: build once and close over it. Like the
			// repro step, seeding requires a container runtime and degrades
			// gracefully when none is available.
			if sandboxOK {
				repoRoot := repo.Root()
				deps.SeedAnalyzers = func(seedCtx context.Context) {
					runAnalyzerSeed(seedCtx, cfg, repoRoot, st, progressSink)
				}
			}

			// Doc-contradiction seeding hook: a pure-Go, in-process miner that
			// needs no container runtime, so it is wired UNCONDITIONALLY (unlike
			// analyzer seeding above). Degrades gracefully on any failure.
			deps.SeedContradictions = func(seedCtx context.Context) {
				runContradictionSeed(seedCtx, cfg, repo, st, progressSink)
			}

			// Publish hook: wire in when cfg.Publish.Enabled. We do not
			// pre-check for gh on PATH here; a missing gh binary will produce a
			// warning on the first post-cycle run via the Publisher interface.
			if cfg.Publish.Enabled {
				deps.Publisher = NewStorePublisherWithProvenance(realGH, st, cfg.Publish, provenanceFromConfig(cfg), logger)
			}

			dcfg := daemon.DaemonConfig{
				PollInterval:         cfg.Daemon.PollInterval,
				SweepInterval:        cfg.Daemon.SweepInterval,
				IdleBackoff:          cfg.Daemon.IdleBackoff,
				ReproBacklogInterval: cfg.Daemon.ReproBacklogInterval,
				VerifyDrainInterval:  cfg.Daemon.VerifyDrainInterval,
				ImpactSweepInterval:  cfg.Daemon.ImpactSweepInterval,
				ReproBacklogBatch:    cfg.Repro.BacklogBatch,
				PerCycleTokens:       cfg.Budgets.PerCycleTokens,
				PerDayTokens:         cfg.Budgets.PerDayTokens,
				CacheReadWeight:      cfg.Budgets.CacheReadWeight,
				EnableRepro:          doRepro && sandboxOK && deps.Reproducer != nil,
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
	// spend ledgers reproducer/patch-prover usage; the daemon retags it with
	// each cycle's scan-run id (bugbot-58c).
	spend *ledgerRecorder
}

// buildReproducer constructs the reproducer-role LLM client, sandbox, and
// Reproducer used by the daemon's post-cycle promotion step. It mirrors the
// wiring in `scan --repro`.
func buildReproducer(ctx context.Context, cfg *config.Config, st *store.Store, repoRoot, runtime string, prog progress.EventSink) (*reproDeps, error) {
	// Ledger repro + patch-prover spend (bugbot-58c). The daemon retags the
	// recorder with each cycle's scan-run id via Deps.ReproTagger.
	rec := newLedgerRecorder(ctx, st)
	client, err := config.ResolveRole(ctx, cfg, "reproducer", llm.Options{Recorder: rec})
	if err != nil {
		return nil, fmt.Errorf("build reproducer client: %w", err)
	}
	sb, err := sandbox.NewCLI(runtime, cfg.Sandbox.Image, sandboxRunOpts(*cfg)...)
	if err != nil {
		return nil, fmt.Errorf("build sandbox: %w", err)
	}
	// Probe image capabilities once; result is cached per image so repeated
	// daemon restarts or re-calls to buildReproducer are free.
	caps := sandbox.ProbeCapabilities(ctx, sb, cfg.Sandbox.Image, repoRoot)
	r, err := repro.New(client, sb, repoRoot, repro.Options{
		MaxAttempts:      cfg.Repro.MaxAttempts,
		Image:            cfg.Sandbox.Image,
		PatchProver:      cfg.Repro.PatchProver,
		PatchMaxAttempts: cfg.Repro.PatchMaxAttempts,
		PatchSuiteCmd:    cfg.Repro.SuiteCmd,
		DepStrategy:      sandbox.DepStrategy(cfg.Sandbox.DepStrategy),
		SetupCmds:        cfg.Sandbox.SetupCmds,
		LocalMounts:      localMountsFromConfig(*cfg),
		Capabilities:     caps,
		Progress:         prog,
		StatusNotes:      cfg.Scan.StatusNotes,
		TranscriptDir:    cfg.Repro.TranscriptDir,
		PackageSummary:   packageSummaryProvider(st),
		Timeout:          time.Duration(cfg.Sandbox.TimeoutSeconds) * time.Second,
		SandboxMaxExecs:  cfg.Repro.SandboxMaxExecs,
	})
	if err != nil {
		return nil, fmt.Errorf("build reproducer: %w", err)
	}
	return &reproDeps{client: client, repro: r, spend: rec}, nil
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
