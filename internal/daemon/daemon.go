// Package daemon implements Bugbot's continuous-operation scheduler: the final
// component that composes ingest, the detection funnel, reproduction, reporting,
// and the state store into a long-running loop.
//
// # The loop
//
// A single-threaded scheduler interleaves two kinds of work; the work itself is
// internally parallel (the funnel fans out across lenses and refuters):
//
//   - POLL — at PollInterval, detect new commits since the last-seen tip. New
//     commits drive a blast-radius-scoped Targeted investigation; an unchanged
//     repo extends the next poll by IdleBackoff (capped), so an idle repo costs
//     near-zero. Any activity resets the backoff.
//   - SWEEP — at SweepInterval (and once at startup if no prior sweep scan-run
//     exists), run a whole-snapshot Sweep and refresh the file_state watermarks
//     from the snapshot's fingerprints so incremental logic has a baseline.
//
// Every cycle shares a POST-CYCLE pass: re-verify open findings whose code
// changed (auto-closing ones whose file/line is gone), optionally promote new
// Tier-2 findings via reproduction, emit a report through the configured sinks,
// and log a one-line cycle summary.
//
// # Budgets
//
// Before any cycle the daemon compares the day's spend (TotalsSince midnight
// UTC) against PerDayTokens. If the day budget is exhausted it skips the cycle
// entirely — no LLM calls — and logs loudly; it rechecks cheaply next tick.
// PerCycleTokens is passed into the funnel as its TokenBudget so a single cycle
// degrades and then stops within its own allowance.
//
// # Shutdown
//
// Run returns when its context is cancelled. Cancellation is observed only at
// tick boundaries and propagates into the funnel, so an in-flight cycle's
// persistence is allowed to finish rather than being killed mid-write. The CLI
// wires SIGINT/SIGTERM to the context via signal.NotifyContext.
package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/report"
	"github.com/dpoage/bugbot/internal/repro"
	"github.com/dpoage/bugbot/internal/store"
)

// lastSeenSentinel is the file_state path under which the daemon persists the
// last-seen commit tip across restarts. It is a reserved, slash-prefixed path
// that can never collide with a real repo-relative file (repo paths are always
// clean and relative), so it lives in the existing file_state table without a
// new migration. content_hash carries the commit SHA; the path is the key.
//
// Rationale (see the package doc and the build notes): a dedicated sentinel row
// is the simplest persistence that works without schema changes. file_state is
// already keyed by an arbitrary TEXT path, and ChangedSince / incremental logic
// only ever query real snapshot paths, so the sentinel is invisible to them.
const lastSeenSentinel = "@daemon/last-seen"

// Deps are the collaborators the daemon composes. The CLI builds these from
// config and resolved role clients; tests construct them directly with fakes.
// All fields except ReproClient are required.
type Deps struct {
	// Repo is the opened target repository (for polling and change diffs).
	Repo *ingest.Repo
	// Store is the durable state: findings, watermarks, scan runs, spend.
	Store *store.Store
	// Clients are the finder/verifier LLM clients the funnel drives.
	Clients funnel.RoleClients
	// ReproClient is the reproducer-role LLM client. It may be nil; reproduction
	// is only attempted when it is non-nil, Cfg.EnableRepro is set, and a sandbox
	// runtime is available (the CLI wires Reproducer in that case).
	ReproClient llm.Client
	// ReproTagger, when non-nil, is retagged with each cycle's scan-run id
	// before reproduction promotion so the reproducer client's spend ledger
	// attributes usage to the right run. The CLI wires the reproducer's
	// ledgerRecorder here; nil is fine (attribution falls back to empty).
	ReproTagger ScanRunTagger
	// Reproducer, when non-nil, is used by the post-cycle reproduction promotion
	// step. The daemon does not construct it (that needs a sandbox the CLI
	// detects); the CLI injects a ready Reproducer or leaves this nil.
	Reproducer Promoter
	// FunnelOpts is the base funnel configuration. The daemon overrides only
	// TokenBudget per cycle (from Cfg.PerCycleTokens).
	FunnelOpts funnel.Options
	// Sinks receive the post-cycle report. May be empty (no emission).
	Sinks []report.Sink
	// Logger receives structured cycle logs. nil uses slog.Default().
	Logger *slog.Logger
	// Progress, when non-nil, receives activity events (cycle start/finish,
	// schedule, re-verify and promotion outcomes) and is threaded into each
	// cycle's funnel as its progress sink. Emission is best-effort and never
	// blocks or fails the loop. See internal/progress.
	Progress progress.Sink
	// Publisher, when non-nil, is invoked after each post-cycle reverify to
	// keep GitHub issues in sync with the store. Only wired when
	// cfg.Publish.Enabled is true and the gh binary is on PATH.
	Publisher Publisher
}

// ScanRunTagger lets the daemon attribute a long-lived client's spend ledger
// to the current cycle's scan run.
type ScanRunTagger interface {
	SetScanRun(id string)
}

// Promoter is the slice of *repro.Reproducer the daemon needs for post-cycle
// Tier-1 promotion. Keeping it an interface lets tests inject a fake without a
// sandbox or container runtime. *repro.Reproducer satisfies it.
type Promoter interface {
	PromoteAll(ctx context.Context, st *store.Store, findings []store.Finding) (*repro.Summary, error)
}

// Publisher is the post-cycle GitHub issue reconciler. The daemon calls it
// after reverifyOpenFindings to keep GitHub issues in sync with the store.
// Keeping it an interface lets tests inject a no-op stub without a real gh
// binary or network. The CLI wires in a real publisher when cfg.Publish.Enabled
// is set; otherwise Publisher in Deps is left nil and the hook is skipped.
type Publisher interface {
	// Publish reconciles open findings against published GitHub issues. It is
	// expected to be idempotent and to tolerate a missing gh binary by returning
	// a non-fatal error (the caller logs and continues).
	Publish(ctx context.Context) error
}

// DaemonConfig mirrors the relevant config.Daemon + config.Budgets fields plus
// the daemon-only EnableRepro toggle. The CLI builds it from loaded config;
// tests set short intervals so the loop is exercisable without real wall-time.
type DaemonConfig struct {
	// PollInterval is the cadence of commit polling. Must be > 0.
	PollInterval time.Duration
	// SweepInterval is the cadence of whole-repo sweeps. Must be > 0.
	SweepInterval time.Duration
	// IdleBackoff extends the next poll when the repo is unchanged. Zero disables
	// backoff (poll always at PollInterval). The effective extra delay is capped
	// at maxBackoffMultiplier * PollInterval.
	IdleBackoff time.Duration
	// PerCycleTokens bounds a single cycle's funnel spend (passed as the funnel's
	// TokenBudget). Zero means unlimited per cycle.
	PerCycleTokens int64
	// CacheReadWeight discounts cache-read tokens in the per-day budget check,
	// matching the funnel's per-cycle weighting (0..1; <=0 means raw).
	CacheReadWeight float64
	// PerDayTokens caps total spend per UTC day. A cycle is skipped entirely once
	// the day's recorded spend reaches this. Zero means unlimited per day.
	PerDayTokens int64
	// EnableRepro turns on the post-cycle reproduction-promotion step (only
	// effective when Deps.Reproducer is non-nil).
	EnableRepro bool
}

// maxBackoffMultiplier caps idle backoff: the next poll is delayed by at most
// this many poll intervals beyond the base, so backoff cannot starve polling
// indefinitely.
const maxBackoffMultiplier = 10

// Daemon is the composed scheduler. Construct with New and drive with Run. It is
// not safe for concurrent Run calls; one Daemon owns one loop.
type Daemon struct {
	repo      *ingest.Repo
	store     *store.Store
	clients   funnel.RoleClients
	repro     Promoter
	reproTag  ScanRunTagger
	publisher Publisher
	fopts     funnel.Options
	sinks     []report.Sink
	log       *slog.Logger
	prog      progress.Sink
	cfg       DaemonConfig

	poller *ingest.Poller

	// idleMultiplier is the current backoff multiplier (0 = no backoff). It grows
	// by one each idle poll up to maxBackoffMultiplier and resets to 0 on any
	// activity. Written only by the single scheduler goroutine; stored atomically
	// so tests can observe it from another goroutine race-free.
	idleMultiplier atomic.Int64

	// clock is the injectable time source. Tests substitute a fake to drive the
	// loop without real wall-time; production uses realClock.
	clock clock
}

// New constructs a Daemon from its dependencies and config. It validates the
// required deps and intervals up front so misconfiguration fails loudly at
// startup rather than mid-loop.
func New(deps Deps, cfg DaemonConfig) (*Daemon, error) {
	if deps.Repo == nil {
		return nil, fmt.Errorf("daemon: nil repo")
	}
	if deps.Store == nil {
		return nil, fmt.Errorf("daemon: nil store")
	}
	if deps.Clients.Finder == nil || deps.Clients.Verifier == nil {
		return nil, fmt.Errorf("daemon: funnel requires finder and verifier clients")
	}
	if cfg.PollInterval <= 0 {
		return nil, fmt.Errorf("daemon: poll_interval must be > 0")
	}
	if cfg.SweepInterval <= 0 {
		return nil, fmt.Errorf("daemon: sweep_interval must be > 0")
	}

	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}

	return &Daemon{
		repo:      deps.Repo,
		store:     deps.Store,
		clients:   deps.Clients,
		repro:     deps.Reproducer,
		reproTag:  deps.ReproTagger,
		publisher: deps.Publisher,
		fopts:     deps.FunnelOpts,
		sinks:     deps.Sinks,
		log:       log,
		prog:      deps.Progress,
		cfg:       cfg,
		poller:    ingest.NewPoller(deps.Repo, ""),
		clock:     realClock{},
	}, nil
}

// newFunnel builds a funnel for one cycle, overriding the per-cycle token budget
// from config while preserving the rest of the base FunnelOpts. A fresh funnel
// per cycle keeps each cycle's budget accounting independent.
func (d *Daemon) newFunnel() (*funnel.Funnel, error) {
	opts := d.fopts
	opts.TokenBudget = d.cfg.PerCycleTokens
	opts.Progress = d.prog
	return funnel.New(d.clients, d.store, d.repo, opts)
}
