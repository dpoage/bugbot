package engine

import (
	"context"
	"fmt"
	"io"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/store"
)

// This file consolidates the command-bootstrap wiring every CLI command
// repeats: config load + store open, the close-store defer, the finder /
// verifier / cartographer LLM client triplet, the funnel.Options field set
// that is cfg-driven, and the sandbox-degraded warning. Relocated here
// (formerly internal/cli/bootstrap.go) so any cobra-free frontend — CLI
// commands via the Dispatcher, and future non-cobra frontends such as the
// Observer TUI — shares exactly one copy of this wiring.
//
// Every function in this file MUST NOT call cobra or read cmd flags
// directly. internal/cli's configPathFromCmd (which does read a cobra flag)
// stays in internal/cli and is the only caller-side glue needed.

// OpenStore loads the user config and opens the state store. Errors from
// store.Open are wrapped with "open store:" so a failed-to-open failure reads
// the same in every command. Errors from config.Load are returned as-is to
// preserve the existing surface for config-validation messages.
//
// Callers MUST close the store.
func OpenStore(ctx context.Context, cfgPath string) (config.Config, *store.Store, error) {
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

// OpenStoreReadOnly is the read-only counterpart of OpenStore: it opens the
// store WITHOUT taking the cross-process writer lock, so read-only commands
// (report, leads, metrics, export, status) run fine while a scan or daemon
// holds the writer lock in another process. WAL permits one writer and many
// concurrent readers, so these commands never block or corrupt.
//
// Callers MUST close the store.
func OpenStoreReadOnly(ctx context.Context, cfgPath string) (config.Config, *store.Store, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return config.Config{}, nil, err
	}
	st, err := store.OpenReadOnly(ctx, cfg.Storage.Path)
	if err != nil {
		return config.Config{}, nil, fmt.Errorf("open store: %w", err)
	}
	return cfg, st, nil
}

// BuildRoleClients constructs the finder, verifier, and (when cartographer is
// enabled in cfg.Scan.Cartographer) cartographer LLM clients via
// config.ResolveRole. The arbiter client is always built: when [roles.arbiter]
// is unset, roleModel returns the verifier mapping, so the unconfigured-arbiter
// = verifier fallback costs nothing. Each role's error is wrapped with the
// role name so a failure identifies the missing piece ("build finder client:
// ...").
func BuildRoleClients(ctx context.Context, cfg *config.Config) (finder, verifier, cartographer, arbiter llm.Client, err error) {
	finder, err = config.ResolveRole(ctx, cfg, "finder", llm.Options{})
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("build finder client: %w", err)
	}
	verifier, err = config.ResolveRole(ctx, cfg, "verifier", llm.Options{})
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("build verifier client: %w", err)
	}
	arbiter, err = config.ResolveRole(ctx, cfg, "arbiter", llm.Options{})
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("build arbiter client: %w", err)
	}
	if cfg.Scan.Cartographer {
		cartographer, err = config.ResolveRole(ctx, cfg, "cartographer", llm.Options{})
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("build cartographer client: %w", err)
		}
	}
	return finder, verifier, cartographer, arbiter, nil
}

// FunnelOptionOverrides carries the per-command fields that vary across scan /
// review / daemon. The cfg-driven fields (Filter, the eight Budgets.* tokens,
// Cartographer, SandboxOpts) are NOT overrides — BuildFunnelOptions populates
// them from cfg so the three commands cannot drift.
type FunnelOptionOverrides struct {
	Lenses      []string
	Refuters    int
	MaxParallel int
	Progress    progress.EventSink
	// Repro is the in-run reproducer hook (scan-only); nil disables in-run
	// reproduction. The hook signature matches funnel.Options.Repro.
	Repro func(ctx context.Context, scanRunID string, finding domain.Finding) error
}

// BuildFunnelOptions returns a fully-populated funnel.Options whose
// config-driven fields (Filter, TokenBudget, CacheReadBudgetWeight,
// FinderBudgetShare, FinderTokenClaim, VerifierTokenClaim, FinderHistoryTokens,
// FinderReadLines, FinderReadBytes, Cartographer, SandboxOpts, TranscriptDir)
// are sourced from cfg. It is the SINGLE source of truth for the nine-field
// budget plumbing:
// scan, daemon, and review all flow through here so the parity-drift risk
// (daemon previously copied SandboxOpts from buildSandboxOpts while scan and
// review set it directly into opts) is structurally impossible.
//
// Per-command fields — lenses, refuter count, parallelism, progress sink,
// in-run repro hook — are taken from overrides.
//
// sandboxDegraded is true when verify.sandbox_exec was requested but no
// container runtime is available. Callers handling it should call
// PrintSandboxDegradedWarning (writers) or log SandboxDegradedWarning
// directly (slog-backed loggers). sandboxErr is non-nil only when
// verify.sandbox_exec was requested and the sandbox backend could not be
// constructed; callers should propagate it.
func BuildFunnelOptions(cfg config.Config, overrides FunnelOptionOverrides) (funnel.Options, bool, error) {
	sandboxOpts, sandboxDegraded, sandboxErr := buildSandboxOpts(cfg)
	if sandboxErr != nil {
		return funnel.Options{}, false, sandboxErr
	}
	opts := funnel.Options{
		Budget: funnel.BudgetConfig{
			TokenBudget:           cfg.Budgets.PerCycleTokens,
			CacheReadBudgetWeight: cfg.Budgets.CacheReadWeight,
			FinderBudgetShare:     cfg.Budgets.FinderBudgetShare,
			FinderTokenClaim:      cfg.Budgets.FinderTokenClaim,
			VerifierTokenClaim:    cfg.Budgets.VerifierTokenClaim,
			ArbiterTokenClaim:     cfg.Budgets.ArbiterTokenClaim,
		},
		Limits: funnel.StageLimits{
			FinderHistoryTokens: cfg.Budgets.FinderHistoryTokens,
			FinderReadLines:     cfg.Budgets.FinderReadLines,
			FinderReadBytes:     cfg.Budgets.FinderReadBytes,
			Refuters:            overrides.Refuters,
			MaxParallel:         overrides.MaxParallel,
		},
		Features: funnel.FeatureFlags{
			Cartographer:        cfg.Scan.Cartographer,
			StatusNotes:         cfg.Scan.StatusNotes,
			ToolComplaints:      cfg.Scan.ToolComplaints,
			DisableHeatOrdering: !cfg.Scan.HeatOrdering,
		},
		Discovery: funnel.DiscoveryConfig{
			Filter: ingest.ScanFilter{Include: cfg.Scan.Include, Exclude: cfg.Scan.Exclude},
			Lenses: overrides.Lenses,
		},
		SandboxOpts:   sandboxOpts,
		Progress:      overrides.Progress,
		Repro:         overrides.Repro,
		TranscriptDir: cfg.TranscriptDir,
	}
	return opts, sandboxDegraded, nil
}

// SandboxDegradedWarning is the text printed (or logged) when
// verify.sandbox_exec is enabled but no container runtime exists: the user
// asked for empirical refutation and must be told it was dropped, mirroring
// the repro stage's skip notice.
const SandboxDegradedWarning = "verify.sandbox_exec is enabled but no container runtime (podman/docker) was found on PATH; refuters will argue without sandbox execution"

// PrintSandboxDegradedWarning writes the uniform "Warning: <text>\n" line to
// w. CLI commands whose sink is a writer (scan, review) call this directly;
// the daemon keeps its slog.Warn(SandboxDegradedWarning) form because its
// sink is a structured logger, not a writer — the underlying text is the same
// constant in both paths.
func PrintSandboxDegradedWarning(w io.Writer) {
	_, _ = fmt.Fprintf(w, "Warning: %s\n", SandboxDegradedWarning)
}
