package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/store"
)

// This file consolidates the command-bootstrap wiring every CLI command
// repeats: config load + store open, the close-store defer, the finder /
// verifier / cartographer LLM client triplet, the funnel.Options field set
// that is cfg-driven, and the sandbox-degraded warning. Migrating every
// command to these helpers eliminates the ~12 verbatim copy sites of the
// open/close pattern, the three-way drift risk in the funnel plumbing, and
// the inconsistent error wrapping that crept in across scan/daemon/review.
//
// Helpers in this file MUST NOT call cobra or read cmd flags directly.
// configPathFromCmd extracts the --config path from the root persistent flag
// and is called at the top of every RunE closure.

// configPathFromCmd returns the --config flag value from the root persistent
// flags. Every RunE closure calls this at the top; the root command registers
// the flag with config.DefaultFileName as the default so the returned value is
// always a valid path.
func configPathFromCmd(cmd *cobra.Command) string {
	p, err := cmd.Root().PersistentFlags().GetString("config")
	if err != nil || p == "" {
		return config.DefaultFileName
	}
	return p
}

// cmdOpenStore loads the user config and opens the state store. Errors from
// store.Open are wrapped with "open store:" so a failed-to-open failure
// reads the same in every command. Errors from config.Load are returned as-is
// to preserve the existing surface for config-validation messages.
//
// Callers MUST close the store, typically via defer closeStore(st).
func cmdOpenStore(ctx context.Context, cfgPath string) (config.Config, *store.Store, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return config.Config{}, nil, err
	}
	st, err := store.Open(ctx, cfg.Storage.Path)
	if err != nil {
		return config.Config{}, nil, fmt.Errorf("open store: %w", err)
	}
	return cfg, st, nil
}

// closeStore closes a store and discards the error. Its sole purpose is to
// replace the verbatim `defer func() { _ = st.Close() }()` pattern that
// otherwise repeats at every store-using command site. Close failures on a
// process-about-to-exit store are never actionable in CLI context.
func closeStore(st *store.Store) {
	_ = st.Close()
}

// buildRoleClients constructs the finder, verifier, and (when cartographer
// is enabled in cfg.Scan.Cartographer) cartographer LLM clients via
// llm.ResolveRole. Each role's error is wrapped with the role name so a
// failure identifies the missing piece ("build finder client: ...") —
// this matches the wrapping every caller had pre-consolidation and is the
// single source of that wording.
func buildRoleClients(ctx context.Context, cfg *config.Config) (finder, verifier, cartographer llm.Client, err error) {
	finder, err = llm.ResolveRole(ctx, cfg, "finder", llm.Options{})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("build finder client: %w", err)
	}
	verifier, err = llm.ResolveRole(ctx, cfg, "verifier", llm.Options{})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("build verifier client: %w", err)
	}
	if cfg.Scan.Cartographer {
		cartographer, err = llm.ResolveRole(ctx, cfg, "cartographer", llm.Options{})
		if err != nil {
			return nil, nil, nil, fmt.Errorf("build cartographer client: %w", err)
		}
	}
	return finder, verifier, cartographer, nil
}

// FunnelOptionOverrides carries the per-command fields that vary across
// scan / review / daemon. The cfg-driven fields (Filter, the eight Budgets.*
// tokens, Cartographer, SandboxOpts) are NOT overrides — buildFunnelOptions
// populates them from cfg so the three commands cannot drift.
type FunnelOptionOverrides struct {
	Lenses      []string
	Refuters    int
	MaxParallel int
	Progress    progress.Sink
	// Repro is the in-run reproducer hook (scan-only); nil disables in-run
	// reproduction. The hook signature matches funnel.Options.Repro.
	Repro func(ctx context.Context, scanRunID string, finding store.Finding) error
}

// buildFunnelOptions returns a fully-populated funnel.Options whose
// config-driven fields (Filter, TokenBudget, CacheReadBudgetWeight,
// FinderBudgetShare, FinderTokenClaim, VerifierTokenClaim, FinderHistoryTokens,
// FinderReadLines, FinderReadBytes, Cartographer, SandboxOpts) are sourced
// from cfg. It is the SINGLE source of truth for the nine-field budget
// plumbing: scan, daemon, and review all flow through here so the
// parity-drift risk (daemon previously copied SandboxOpts from buildSandboxOpts
// while scan and review set it directly into opts) is structurally impossible.
//
// Per-command fields — lenses, refuter count, parallelism, progress sink,
// in-run repro hook — are taken from overrides.
//
// sandboxDegraded is true when verify.sandbox_exec was requested but no
// container runtime is available. Callers handling it should call
// printSandboxDegradedWarning (writers) or log sandboxDegradedWarning
// directly (slog-backed loggers). sandboxErr is non-nil only when
// verify.sandbox_exec was requested and the sandbox backend could not be
// constructed; callers should propagate it.
func buildFunnelOptions(cfg config.Config, overrides FunnelOptionOverrides) (funnel.Options, bool, error) {
	sandboxOpts, sandboxDegraded, sandboxErr := buildSandboxOpts(cfg)
	if sandboxErr != nil {
		return funnel.Options{}, false, sandboxErr
	}
	opts := funnel.Options{
		Filter:                ingest.ScanFilter{Include: cfg.Scan.Include, Exclude: cfg.Scan.Exclude},
		TokenBudget:           cfg.Budgets.PerCycleTokens,
		CacheReadBudgetWeight: cfg.Budgets.CacheReadWeight,
		FinderBudgetShare:     cfg.Budgets.FinderBudgetShare,
		FinderTokenClaim:      cfg.Budgets.FinderTokenClaim,
		VerifierTokenClaim:    cfg.Budgets.VerifierTokenClaim,
		FinderHistoryTokens:   cfg.Budgets.FinderHistoryTokens,
		FinderReadLines:       cfg.Budgets.FinderReadLines,
		FinderReadBytes:       cfg.Budgets.FinderReadBytes,
		Cartographer:          cfg.Scan.Cartographer,
		StatusNotes:           cfg.Scan.StatusNotes,
		DisableHeatOrdering:   !cfg.Scan.HeatOrdering,
		SandboxOpts:           sandboxOpts,
		Lenses:                overrides.Lenses,
		Refuters:              overrides.Refuters,
		MaxParallel:           overrides.MaxParallel,
		Progress:              overrides.Progress,
		Repro:                 overrides.Repro,
	}
	return opts, sandboxDegraded, nil
}

// sandboxDegradedWarning is the text printed (or logged) when
// verify.sandbox_exec is enabled but no container runtime exists: the user
// asked for empirical refutation and must be told it was dropped, mirroring
// the repro stage's skip notice. Lifted from scan.go so every CLI command
// rendering the warning agrees word-for-word.
const sandboxDegradedWarning = "verify.sandbox_exec is enabled but no container runtime (podman/docker) was found on PATH; refuters will argue without sandbox execution"

// printSandboxDegradedWarning writes the uniform "Warning: <text>\n" line to
// w. CLI commands whose sink is a writer (scan, review) call this directly;
// the daemon keeps its slog.Warn(sandboxDegradedWarning) form because its
// sink is a structured logger, not a writer — the underlying text is the
// same constant in both paths.
func printSandboxDegradedWarning(w io.Writer) {
	_, _ = fmt.Fprintf(w, "Warning: %s\n", sandboxDegradedWarning)
}
