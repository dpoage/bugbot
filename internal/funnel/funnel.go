// Package funnel implements Bugbot's precision-first detection pipeline: the
// staged core that turns a repository snapshot into a small set of
// adversarially-verified findings.
//
// The pipeline is deliberately tuned for precision over recall (see
// ARCHITECTURE.md): it is better to surface three real bugs than ten probable
// ones. Finder agents WANT to find bugs; the verifier's job is to kill false
// positives. Known-clean code must produce zero findings.
//
// # Stages
//
//	A. Hypothesize — parallel finder agents, one per lens, each auditing the
//	   target files (chunked) for one class of defect via RunJSON.
//	B. Triage      — drop low-confidence candidates, dedup by store fingerprint,
//	   drop suppressed fingerprints, drop candidates outside the snapshot.
//	C. Verify      — per surviving candidate, run N adversarial refuter agents;
//	   a majority "refuted" verdict kills the candidate. Survivors are Tier 2.
//	D. Persist     — upsert survivors (status open, tier 2, anchored to the
//	   commit + file content hash) inside a scan run, record per-role spend, and
//	   return a ranked Result.
//
// # Budget degradation
//
// Options.TokenBudget (0, or any value <= 0, = UNLIMITED) bounds the whole run. The funnel tracks
// cumulative spend and degrades gracefully rather than truncating silently:
// past 70% it runs only the two highest-yield lenses and a single refuter; past
// 100% it launches no new agents and finishes with what is already verified.
// Everything skipped is surfaced on the Result.
package funnel

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/sandbox"
	"github.com/dpoage/bugbot/internal/store"
)

// Defaults for Options. Exported so callers (CLI flags) can present them.
const (
	// DefaultRefuters is the number of adversarial refuter agents run per
	// candidate. Three gives a meaningful majority vote without tripling cost
	// versus one.
	DefaultRefuters = 3
	// DefaultCacheReadBudgetWeight is the fraction at which cache-read input
	// tokens count against the token budget when Options leaves it unset. ~0.1
	// approximates Anthropic's cache-read discount; a conservative, cost-favoring
	// default that undercounts slightly on milder-discount providers.
	DefaultCacheReadBudgetWeight = 0.1
	// DefaultMaxParallel bounds concurrently-running agents across the run.
	DefaultMaxParallel = 4
	// DefaultChunkSize is the number of target files handed to a single finder
	// invocation. Chunking keeps each finder's context focused and lets large
	// repos parallelize within a lens.
	//
	// Sized so a finder can plausibly read every file in a chunk and still emit
	// JSON within DefaultMaxIterations (20). Calibration found 30 files structurally
	// impossible inside 20 turns: the agent ran out of iterations mid-read on a
	// 16-file chunk. With one read per file plus a few cross-reference lookups, a
	// budget of ~8 files leaves ~12 turns of headroom before forced finalization,
	// which keeps each finder coherent rather than perpetually truncated. Forced
	// finalization (see agent.RunJSON) is the safety net; the smaller chunk is the
	// structural fix so finalization is the exception, not the norm.
	DefaultChunkSize = 8

	// DefaultFinderHistoryTokens is the per-finder history-compaction threshold
	// applied when a caller asks for compaction but leaves the exact threshold
	// unset. Once a finder's estimated message history exceeds this many tokens,
	// older tool-result content is compacted to stubs once per crossing (see
	// agent.Limits.HistoryTokenBudget), preserving the task, the reasoning chain,
	// the most recent results, and tool-call pairing.
	//
	// IMPORTANT — compaction is OFF by default. The bugbot-3nf offline measurement
	// showed that under a strong ~0.1x prompt cache (the cache weight the budget
	// uses), mutating the message prefix to prune old results forfeits cache hits
	// worth MORE than the bytes reclaimed over the few remaining turns: raw input
	// tokens drop ~10-37% but CACHE-WEIGHTED cost rises ~1-43%. So compaction is a
	// raw-token / context-window-pressure / weak-cache lever, not the cache-cost
	// win the bead targeted; the cache-safe finder default is instead tighter
	// per-read caps (DefaultFinderReadLines/Bytes). Compaction stays available for
	// callers on providers with little or no prompt-cache discount, where the
	// raw-token reduction IS the real-dollar reduction. This threshold is the
	// value used when such a caller sets budgets.finder_history_tokens to 0 while
	// having explicitly opted in via Options; a negative value disables it.
	DefaultFinderHistoryTokens int64 = 60_000

	// DefaultFinderReadLines / DefaultFinderReadBytes are the per-read_file caps a
	// finder uses, tighter than the agent package defaults (2000 lines / 256 KB).
	//
	// This is the PRIMARY cache-safe lever for the finder token-burn finding
	// (bugbot-3nf). A finder re-sends its whole growing history every turn, so a
	// fat read_file result is re-billed on every later turn — quadratic in turns.
	// Tightening the cap shrinks each result at the SOURCE, before it ever enters
	// the conversation, so it never mutates an earlier message and never forfeits
	// a prompt-cache prefix. The offline measurement (internal/eval, the
	// bugbot-3nf harness) shows this cuts CACHE-WEIGHTED input ~55-77% on a
	// SYNTHETIC runaway profile (every read assumed cap-saturated; a BEST-CASE UPPER
	// BOUND, not a corpus measurement — the recorded corpus never exercises this
	// lever because its files are well below the caps). Threshold history compaction
	// — which mutates the prefix — REDUCES raw tokens but INCREASES cache-weighted
	// cost under the same ~0.1x cache, and is therefore left opt-in/off (see
	// DefaultFinderHistoryTokens).
	//
	// 800 lines / 96 KB comfortably covers a focused source file (most files under
	// analysis are far smaller); larger files are line-windowed with offset/limit,
	// and the truncation note tells the model to page if it genuinely needs more.
	//
	// NOTE — the "~55-77%" figure cited in code comments and the offline harness
	// (internal/eval/compact_measure_test.go) is a SYNTHETIC BEST-CASE UPPER BOUND,
	// not a corpus measurement. It assumes every read_file call saturates the cap
	// (the savings ARE the truncated content), and the recorded eval corpus never
	// exercises this lever because its files are well below the caps. Real savings
	// depend on whether files in the target repo actually exceed the cap.
	DefaultFinderReadLines = 800
	DefaultFinderReadBytes = 96 * 1024

	// DefaultMaxOutputTokens caps each finder/verifier completion's VISIBLE output.
	// Reasoning models (e.g. MiniMax M3) spend most of their completion allowance
	// inside <think> blocks; without an explicit, generous cap the provider default
	// can be exhausted before any JSON is emitted, yielding "empty model output".
	// 8k visible tokens comfortably fits a candidate list or a refutation verdict
	// with reasoning headroom to spare.
	DefaultMaxOutputTokens = 8192

	// softBudgetFraction is the fraction of TokenBudget past which the run
	// degrades to the highest-yield lenses and a single refuter.
	softBudgetNumer = 7
	softBudgetDenom = 10
	// degradedLensCount is how many top-yield lenses survive degradation.
	degradedLensCount = 2
	// degradedRefuters is the refuter count under degradation.
	degradedRefuters = 1

	// DefaultFinderBudgetShare is the fraction of a run's TokenBudget the finder
	// stage may consume when Options leaves FinderBudgetShare unset. The
	// remainder is RESERVED for downstream verification (and, transitively,
	// reproduction, which can only run on verified Tier-2 survivors). Without a
	// reservation the finder stage — which launches first and in bulk — routinely
	// drains the entire shared budget pool before any candidate reaches the
	// verifier, orphaning every finding as Tier-3 and starving reproduction of
	// input (bugbot-3lt live evidence: 769 finder units hard-skipped, 0 Tier-1
	// promotions across 15 runs). 0.7 keeps the breadth-heavy finder stage the
	// majority shareholder while guaranteeing the verifier a meaningful slice it
	// cannot be starved out of. Tune via budgets.finder_budget_share.
	DefaultFinderBudgetShare = 0.7

	// DefaultTokenClaim is the per-task token claim a finder or verifier run is
	// capped at when Options leaves the role's claim size unset. It bounds any
	// single agent run's per-run TokenBudget (see budgetState.runnerLimitsForPool)
	// so one breadth-heavy run cannot be granted a whole stage's reserve at
	// launch. The shared pool is charged only for tokens actually spent, so the
	// unspent part of a claim stays available to sibling runs. 1M matches
	// agent.DefaultTokenBudget so a claimed run's cap equals the historical
	// default when the sub-pool has at least one claim's worth of headroom. Tune
	// via budgets.finder_token_claim / budgets.verifier_token_claim.
	DefaultTokenClaim int64 = 1_000_000

	// DefaultCartographerMaxFiles bounds the number of member files in a
	// package fed to the cartographer's per-package summary completion. The
	// summary is cached by content fingerprint and regenerated only when the
	// package changes, so it is worth feeding a generous view of a large
	// package once; 64 covers all but the largest packages in full.
	DefaultCartographerMaxFiles = 64
	// DefaultCartographerHeadLines caps the lines read from each member file
	// when building a package's summary input. The head carries the
	// highest-signal material (package doc, imports, exported declarations),
	// but a good summary often needs the bodies too, so the cap is generous;
	// 400 covers most Go files in full.
	DefaultCartographerHeadLines = 400
	// DefaultCartographerInputBytes caps the total bytes of member-file
	// content fed to a single summary completion — the hard ceiling that
	// binds first for a large package. Reasoning models have large context
	// windows and the summary is cached, so a generous 128 KB buys a
	// well-grounded summary at a one-time-per-change cost.
	DefaultCartographerInputBytes = 128 * 1024
)

// cartographyInjectMaxPkgs and cartographyInjectMaxBytes bound the size of
// the REPO CONTEXT block injected into a finder task. The bound is per-unit
// (not per-run) so a single chunk with many distinct packages cannot bloat
// one finder's task. 12 packages × ~500 bytes per summary line fits inside
// the byte cap; overflow drops the tail and notes the truncation so the
// agent knows the context is partial.
const (
	cartographyInjectMaxPkgs  = 12
	cartographyInjectMaxBytes = 6 * 1024
)

// RoleClients holds the per-role LLM clients the funnel drives. Tests inject
// fakes; the CLI builds these via llm.ResolveRole. Finder and Verifier are
// required (New rejects nil). Cartographer is optional: when nil, the
// package-summary pass reuses Finder. Reproducer is not used by this stage
// (Tier 1 reproduction is a later stage) and is intentionally absent.
type RoleClients struct {
	Finder   llm.Client
	Verifier llm.Client
	// Cartographer is the client for the package-summary pass (optional; nil
	// reuses Finder). Configured via the [roles.cartographer] mapping.
	Cartographer llm.Client
}

// roleFinder / roleVerifier / roleCartographer are the spend-ledger role tags
// wired into the per-role recorder clients (see run.go). The recorder routes
// finder AND cartographer spend to the finder sub-pool and everything else to
// the verify sub-pool under a downstream-budget reservation, so these MUST
// match the strings passed to llm.WithRecorder.
const (
	roleFinder       = "finder"
	roleVerifier     = "verifier"
	roleCartographer = "cartographer"
)

// SandboxOpts bundles the sandbox-execution knobs that gate and bound the
// sandbox_exec tool offered to refuter agents. The zero value means the
// feature is disabled.
type SandboxOpts struct {
	// Sandbox is the sandbox backend to use. Nil means the feature is
	// unconditionally unavailable regardless of other fields.
	Sandbox sandbox.Sandbox
	// Enabled gates the feature: if false (default), no refuter receives the
	// tool.
	Enabled bool
	// MinSeverity is the minimum candidate severity that qualifies for the
	// tool. Candidates below this threshold use only rhetorical reasoning.
	// Valid values: "critical", "high", "medium", "low". Empty defaults to
	// "high".
	MinSeverity string
	// MaxExecs is the per-candidate execution budget. Zero defaults to 3.
	MaxExecs int
	// DepStrategy selects how external module dependencies are made available to
	// a refuter's network-none probe (see sandbox.DepStrategy). Empty/"off"
	// keeps the current behavior; "host"/"fetch" mount a module cache. Vendored
	// repos are always detected. The one-time online prefetch for "fetch" runs
	// once, lazily, the first time a sandbox tool is built.
	DepStrategy sandbox.DepStrategy
}

// ChangeContext carries commit-scoped context for a targeted (commit-triggered)
// funnel run. It is optional: nil on sweep runs and targeted runs without a
// specific commit window. When non-nil, the diff-intent lens uses it to look
// for gaps between the commit's stated intent and its implementation.
//
// ChangeContext is only meaningful on ScanTargeted runs; it is ignored on
// ScanOneshot (Sweep) and ScanSweep runs even if set.
type ChangeContext struct {
	// FromCommit and ToCommit are the inclusive range of the change (e.g. the
	// parent and the new HEAD). Both must be non-empty for the lens to fire.
	FromCommit string
	// ToCommit is the tip commit of the change window.
	ToCommit string
	// Message is the full commit message of ToCommit (from CommitMessage).
	// Truncated at 4KB in the task prompt.
	Message string
	// Diff is the raw unified diff between FromCommit and ToCommit
	// (from UnifiedDiff). May be nil if no diff is available.
	Diff []byte
	// ChangedFiles is the list of repo-relative paths modified by the change
	// (from ChangedFiles + ChangedPaths). Used in the task for context.
	ChangedFiles []string
	// BlastFiles is intentionally absent: the blast-radius dependent list is
	// derived inside hypothesize from the targets already expanded by Targeted
	// (run.go calls BlastRadius before hypothesize). Callers must not set it.
}

// Options configures a single funnel run. The zero value is valid: every field
// resolves to a sensible default.
type Options struct {
	// Lenses, when non-empty, restricts the finder stage to the named built-in
	// lenses (see BuiltinLenses). Empty means all lenses.
	Lenses []string
	// Filter scopes the snapshot to the configured include/exclude globs
	// (config.Scan maps directly onto it). The zero value scans every tracked
	// file.
	Filter ingest.ScanFilter
	// Refuters is the number of adversarial refuter agents per candidate. Zero
	// uses DefaultRefuters.
	Refuters int
	// MaxParallel bounds concurrently-running agents across all roles (finder
	// breadth, verifier candidate panels). Zero uses DefaultMaxParallel;
	// negative is treated as 1. The global slot pool (slotPool) enforces this
	// bound; per-stage local semaphores have been removed.
	MaxParallel int
	// ChunkSize is the number of files per finder invocation. Zero uses
	// DefaultChunkSize.
	ChunkSize int
	// TokenBudget bounds cumulative input+output tokens for the whole run. Zero
	// means unlimited (the funnel never degrades or stops on budget).
	TokenBudget int64
	// CacheReadBudgetWeight discounts cache-read input tokens against
	// TokenBudget (0..1). Cache reads bill at a fraction of full price, so
	// counting them at full weight makes a cache-heavy run exhaust the budget
	// long before its real cost warrants. Zero resolves to
	// DefaultCacheReadBudgetWeight; set to 1.0 to restore raw-token accounting.
	CacheReadBudgetWeight float64
	// FinderBudgetShare is the fraction of TokenBudget (0..1) the finder stage
	// may consume; the remainder is RESERVED for downstream verification so the
	// breadth-heavy finder stage cannot drain the whole pool and orphan every
	// candidate before it is verified (see DefaultFinderBudgetShare). Zero (or
	// negative) resolves to DefaultFinderBudgetShare. A value >= 1 disables the
	// reservation (finders may use the whole budget — the legacy single-pool
	// behavior). Ignored when TokenBudget is unlimited.
	FinderBudgetShare float64
	// FinderTokenClaim / VerifierTokenClaim are the per-task token claims for the
	// claimant budget system. Each finder/refuter/arbiter run is capped at its
	// role's claim (bounding the run's per-run TokenBudget) so a single
	// breadth-heavy run cannot be granted a whole stage's reserve at launch. The
	// shared per-cycle pool is charged only for tokens actually spent, so a run
	// that finishes under its claim leaves the remainder in the pool for its
	// siblings — the claim is "returned to the pool" by never being removed.
	// Zero resolves to DefaultTokenClaim (1M); a negative value removes the
	// per-task cap (a run may use its sub-pool's full remainder, the pre-claimant
	// behavior). Ignored when TokenBudget is unlimited (no pool to cap against).
	FinderTokenClaim   int64
	VerifierTokenClaim int64
	// FinderLimits / VerifierLimits bound each individual agent run (iterations
	// and per-run token budget). Zero-value fields resolve to agent defaults.
	FinderLimits   agent.Limits
	VerifierLimits agent.Limits
	// FinderHistoryTokens controls opt-in finder history compaction (see
	// agent.Limits.HistoryTokenBudget and DefaultFinderHistoryTokens for why it is
	// OFF by default). Zero AND negative both leave compaction DISABLED — the
	// cache-safe finder default is tighter per-read caps, not prefix-mutating
	// compaction. A POSITIVE value opts in at that token threshold (a common
	// choice on weak-/no-cache providers, where the raw-token reduction is the
	// real-dollar reduction). It is folded into FinderLimits.HistoryTokenBudget at
	// resolve time; set this field, not the nested limit, to control it.
	FinderHistoryTokens int64
	// FinderReadLines / FinderReadBytes tighten the finder's per-read_file caps,
	// the primary cache-safe lever for finder token burn (bugbot-3nf). Zero uses
	// DefaultFinderReadLines / DefaultFinderReadBytes. A negative value restores
	// the looser agent-package read defaults (2000 lines / 256 KB) for the finder.
	FinderReadLines int
	FinderReadBytes int
	// TranscriptDir, when non-empty, makes every agent auto-save its transcript
	// there.
	TranscriptDir string
	// Progress, when non-nil, receives activity events as the run proceeds
	// (stage boundaries, agent start/finish, spend ticks, budget degradation).
	// Emission is best-effort and must never block or fail the run; a nil sink
	// disables emission. See internal/progress for the contract.
	Progress progress.Sink
	// SandboxOpts configures the sandbox_exec tool offered to refuter agents.
	// The zero value disables the feature.
	SandboxOpts SandboxOpts
	// DisableHeatOrdering suppresses churn-heat reordering in Sweep. By
	// default (false) Sweep sorts targets by churn-weighted recency heat so
	// finder budget flows to files that have changed recently and frequently —
	// where bugs statistically cluster. Set to true to restore fully
	// alphabetical Sweep ordering (e.g. for deterministic testing or when
	// the repository has no git history). Targeted scans are always
	// alphabetical regardless of this setting.
	DisableHeatOrdering bool
	// ChangeContext, when non-nil, provides commit-scoped information for a
	// targeted (commit-triggered) run. It enables the diff-intent lens, which
	// hunts for gaps between the commit's stated intent and its implementation
	// and for existing callers whose assumptions the change breaks. Nil on
	// sweep runs and targeted runs without a specific commit window.
	//
	// ChangeContext is only honoured on ScanTargeted runs. It is silently
	// ignored on ScanOneshot (Sweep) and ScanSweep runs even if set.
	ChangeContext *ChangeContext

	// Repro, when non-nil, is invoked in-run for each Tier-2 finding that
	// survives verification. It is called from an IDLE-priority goroutine (one
	// slot per finding) so reproduction runs concurrently with discovery. The
	// funnel does NOT import internal/repro; callers (e.g. the CLI) build a
	// Reproducer and pass a closure here.
	//
	// The hook must be safe for concurrent use (it may be called by multiple
	// goroutines simultaneously). Errors are logged best-effort and never abort
	// the scan. Nil disables in-run reproduction (default).
	Repro func(ctx context.Context, scanRunID string, finding store.Finding) error

	// Cartographer enables the per-package summary pass (bugbot-mi5.7).
	// When true, the funnel runs a one-shot LLM pass per uncached package
	// before the finder stage, persists the results keyed by the package's
	// content fingerprint, and injects the relevant summaries (the chunk's
	// own packages plus their direct dependents) into every finder task
	// message. When false (the default) the feature is OFF and behavior is
	// BYTE-IDENTICAL to the pre-cartographer funnel: no summary-table
	// reads, no summary-generation completions, no task-string changes.
	//
	// Any failure in the pass — store read/write error, LLM error, or hard
	// budget exhaustion — degrades gracefully: the scan proceeds with the
	// summaries it has (possibly none), and finder tasks receive no
	// injection when nothing is available.
	Cartographer bool
}

// resolve fills in defaults without mutating the caller's Options.
func (o Options) resolve() Options {
	// DisableHeatOrdering is intentionally NOT touched here: the zero value
	// (false) already means "heat ordering enabled", which is the default.
	// No resolution needed.
	if o.Refuters <= 0 {
		o.Refuters = DefaultRefuters
	}
	if o.MaxParallel == 0 {
		o.MaxParallel = DefaultMaxParallel
	}
	if o.MaxParallel < 0 {
		o.MaxParallel = 1
	}
	if o.ChunkSize <= 0 {
		o.ChunkSize = DefaultChunkSize
	}
	// A non-positive share means "unset": apply the default downstream
	// reservation. A value >= 1 is preserved as an explicit "no reservation"
	// (legacy single-pool) request and handled by reserveForDownstream.
	if o.FinderBudgetShare <= 0 {
		o.FinderBudgetShare = DefaultFinderBudgetShare
	}
	// Per-task token claims: zero means "unset" → DefaultTokenClaim. A NEGATIVE
	// value is preserved as an explicit "no per-task cap" request and honored by
	// runnerLimitsForPool (the run may use its sub-pool's full remainder).
	if o.FinderTokenClaim == 0 {
		o.FinderTokenClaim = DefaultTokenClaim
	}
	if o.VerifierTokenClaim == 0 {
		o.VerifierTokenClaim = DefaultTokenClaim
	}
	// Fold the (opt-in) history-compaction threshold into FinderLimits so the
	// per-finder runner picks it up. Compaction is OFF by default because the
	// bugbot-3nf measurement showed it raises cache-weighted cost: only a POSITIVE
	// request arms it; zero and negative both leave it disabled.
	if o.FinderHistoryTokens > 0 {
		o.FinderLimits.HistoryTokenBudget = o.FinderHistoryTokens
	} else {
		o.FinderLimits.HistoryTokenBudget = 0
	}
	return o
}

// finderReadCaps resolves the per-read_file caps for finder agents from Options,
// substituting the funnel finder defaults for unset fields and honoring a
// negative request as "use the looser agent-package defaults".
func (o Options) finderReadCaps() agent.ReadCaps {
	caps := agent.ReadCaps{}
	switch {
	case o.FinderReadLines < 0:
		caps.MaxLines = 0 // 0 -> agent default (looser) at the tool layer
	case o.FinderReadLines == 0:
		caps.MaxLines = DefaultFinderReadLines
	default:
		caps.MaxLines = o.FinderReadLines
	}
	switch {
	case o.FinderReadBytes < 0:
		caps.MaxBytes = 0
	case o.FinderReadBytes == 0:
		caps.MaxBytes = DefaultFinderReadBytes
	default:
		caps.MaxBytes = o.FinderReadBytes
	}
	return caps
}

// Funnel runs the staged detection pipeline against a repository. It is
// constructed once per process (or per daemon) and is safe for sequential reuse
// across scans; a single scan parallelizes internally. Call [Funnel.Close] when
// done to shut down any language servers the code-navigation tools spawned.
type Funnel struct {
	clients RoleClients
	store   *store.Store
	repo    *ingest.Repo
	opts    Options
	lenses  []Lens
	// slots is the funnel-wide agent concurrency pool. Every LLM agent —
	// finder unit (low priority) or verifier candidate panel (high priority)
	// — holds one slot for its entire duration. Options.MaxParallel means
	// "concurrent agents across all roles".
	slots *slotPool

	// navOnce lazily builds the shared code-navigation tool bundle (and its
	// language-server manager) on first use, so funnels that never run agents
	// spawn nothing.
	navOnce sync.Once
	nav     *agent.CodeNav
	navErr  error

	// deps is the resolved dependency strategy for the sandbox_exec tool,
	// computed in New from SandboxOpts. depPrefetchOnce ensures the one-time
	// online prefetch (DepStrategyFetch) runs at most once across all candidates.
	deps            sandbox.Resolution
	depPrefetchOnce sync.Once
	depPrefetchErr  error
}

// New constructs a Funnel. clients supplies the finder/verifier LLM clients,
// st is the state store, repo is the opened target repository, and opts tunes
// the run. clients.Finder and clients.Verifier must be non-nil; st and repo
// must be non-nil.
func New(clients RoleClients, st *store.Store, repo *ingest.Repo, opts Options) (*Funnel, error) {
	if clients.Finder == nil {
		return nil, fmt.Errorf("funnel: nil finder client")
	}
	if clients.Verifier == nil {
		return nil, fmt.Errorf("funnel: nil verifier client")
	}
	if st == nil {
		return nil, fmt.Errorf("funnel: nil store")
	}
	if repo == nil {
		return nil, fmt.Errorf("funnel: nil repo")
	}
	resolved := opts.resolve()

	// Resolve the dependency strategy for the sandbox_exec tool up front so
	// every refuter probe carries the same module-cache mount/env. Only relevant
	// when the sandbox feature is enabled; vendored/off repos resolve to empty.
	var deps sandbox.Resolution
	if resolved.SandboxOpts.Enabled && resolved.SandboxOpts.Sandbox != nil {
		d, err := sandbox.ResolveDeps(repo.Root(), sandbox.DepOptions{
			Strategy:     resolved.SandboxOpts.DepStrategy,
			FetchSandbox: resolved.SandboxOpts.Sandbox,
		})
		if err != nil {
			return nil, fmt.Errorf("funnel: resolve dependency strategy: %w", err)
		}
		deps = d
	}

	return &Funnel{
		clients: clients,
		store:   st,
		repo:    repo,
		opts:    resolved,
		lenses:  selectLenses(resolved.Lenses),
		deps:    deps,
		slots:   newSlotPool(resolved.MaxParallel),
	}, nil
}

// codeNav returns the shared code-navigation tool bundle, creating it (and its
// lazy language-server manager — no processes are spawned until a tool's first
// query) on first call.
func (f *Funnel) codeNav() (*agent.CodeNav, error) {
	f.navOnce.Do(func() {
		f.nav, f.navErr = agent.NewCodeNav(f.repo.Root())
	})
	return f.nav, f.navErr
}

// Close shuts down any language servers the code-navigation tools spawned.
// Safe to call multiple times, on a funnel that never ran, and on a nil receiver
// (so deferred Close calls on a partially-initialised funnel never panic).
func (f *Funnel) Close() error {
	if f == nil {
		return nil
	}
	// Synchronize with codeNav() so a Close racing the lazy init still sees
	// the bundle.
	f.navOnce.Do(func() {})
	if f.nav == nil {
		return nil
	}
	return f.nav.Close()
}

// Candidate is a finder-proposed bug after it has been associated with a lens
// and a fingerprint. It is the unit that flows from hypothesize through triage
// into verification.
type Candidate struct {
	Lens        string
	File        string
	Line        int
	Title       string
	Description string
	Severity    string
	Evidence    string
	Confidence  string
	// Fingerprint is the store dedup key (lens+file+line+title). Set in triage.
	Fingerprint string
	// CorroboratingLenses lists the OTHER lenses that independently reported the
	// same underlying defect (same file, nearby line) and were collapsed into this
	// candidate during triage's location-based cross-lens dedup. It excludes this
	// candidate's own Lens and is deduplicated and sorted. Empty when no other
	// lens corroborated the finding. It is recorded for reporting only — it does
	// NOT raise the candidate's confidence.
	CorroboratingLenses []string
}

// Stats is the per-stage funnel accounting recorded on the scan run.
type Stats struct {
	// Hypothesized is the raw candidate count emitted by all finder agents.
	Hypothesized int `json:"hypothesized"`
	// Triaged is the candidate count surviving triage (the input to verify).
	Triaged int `json:"triaged"`
	// Verified is the count surviving adversarial verification (Tier 2).
	Verified int `json:"verified"`
	// Killed is candidates that entered verification but were majority-refuted.
	Killed int `json:"killed"`
	// Suspected is the count of budget-orphaned candidates persisted as Tier 3
	// suspected: they passed triage but the run hit its hard budget before their
	// verification completed, so they are kept (not dropped) for human review.
	Suspected int `json:"suspected,omitempty"`
	// DroppedLowConfidence / DroppedDuplicate / DroppedSuppressed /
	// DroppedOutOfScope break down the triage losses.
	DroppedLowConfidence int `json:"dropped_low_confidence"`
	DroppedDuplicate     int `json:"dropped_duplicate"`
	DroppedSuppressed    int `json:"dropped_suppressed"`
	DroppedOutOfScope    int `json:"dropped_out_of_scope"`
	// MergedWithinLens / MergedCrossLens break down the location-based cross-lens
	// dedup losses in triage: after exact-fingerprint dedup, surviving candidates
	// are clustered by location (same file, nearby line) and only the cluster's
	// primary proceeds to verification. Each collapsed (non-primary) member is
	// counted here — MergedWithinLens when its lens equals the primary's lens
	// (the same lens reported the same defect twice with different wording),
	// MergedCrossLens when it came from a different lens. These are distinct from
	// DroppedDuplicate (exact fingerprint match): a merged member is a DIFFERENT
	// fingerprint that nonetheless points at the same underlying bug.
	MergedWithinLens int `json:"merged_within_lens"`
	MergedCrossLens  int `json:"merged_cross_lens"`
	// FinderRuns is the number of finder (lens, chunk) agents that actually
	// launched (i.e. were not skipped by budget degradation/stop). FinderFailures
	// is how many of those produced NO parseable output even after the repair
	// round-trip — their findings are lost, not absent. A scan with
	// FinderFailures > 0 must never report a bare "No findings": that result is
	// untrustworthy. See internal/cli/scan.go and reliabilityWarning.
	FinderRuns     int `json:"finder_runs"`
	FinderFailures int `json:"finder_failures"`
	// FinderBudgetStopped counts finders that ran but were truncated by a budget
	// limit (their own token budget or the shared budget pool) before producing
	// parseable output. These are deliberate budget stops, NOT reliability
	// failures: they are excluded from FinderFailures so a budget-limited scan is
	// never misreported as having broken finders. Their partial coverage is noted
	// under Result.Skipped instead.
	FinderBudgetStopped int `json:"finder_budget_stopped,omitempty"`
	// FinderRateLimited counts finders that exhausted the retry budget against
	// a rate-limiting provider (llm.ErrRateLimited). Distinct from
	// FinderFailures: the provider throttled us, the findings are NOT lost in
	// the model-output sense — they were never produced because the run
	// never completed. Coverage is incomplete but recoverable by lowering
	// --concurrency or re-running, so this is excluded from FinderReliable()
	// and MostFindersFailed(). A rate-limited-only run is "reliable but
	// coverage-incomplete", which is the intended distinction from a genuine
	// parse failure.
	FinderRateLimited int `json:"finder_rate_limited,omitempty"`
	// VerifierRuns / VerifierFailures mirror the above for refuter agents. A
	// refuter that fails to parse is conservatively treated as "could not refute"
	// (it cannot silently kill a candidate), but the failure is still counted so
	// the verification result's reliability is visible.
	VerifierRuns     int `json:"verifier_runs"`
	VerifierFailures int `json:"verifier_failures"`
	// ArbiterRuns is the number of arbiter agents launched to decide split
	// (mixed refuted/not-refuted) panel verdicts.
	ArbiterRuns int `json:"arbiter_runs,omitempty"`
	// ArbiterKills is the number of candidates the arbiter decided to kill
	// (arbiter returned refuted=true).
	ArbiterKills int `json:"arbiter_kills,omitempty"`
	// ArbiterFailures is the number of arbiter agents that produced no parseable
	// verdict; on failure the run falls back to majorityRefuted.
	ArbiterFailures int `json:"arbiter_failures,omitempty"`
	// InputTokens / OutputTokens is the run's total token spend. InputTokens
	// includes cached tokens (the llm.Usage convention).
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	// CacheReadTokens / CacheCreationTokens are the subsets of InputTokens
	// served from / written to the provider's prompt cache, for reporting
	// cache savings.
	CacheReadTokens     int64 `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens int64 `json:"cache_creation_tokens,omitempty"`
	// CartographerEnabled records whether the package-summary pass
	// (scan.cartographer) was active for this run. Persisted so the
	// valid-findings-per-token series — Verified / (InputTokens+OutputTokens),
	// one point per scan run over started_at — can be sliced by cartographer
	// on/off. That ratio, not raw token count, is how the feature earns its
	// keep: a new agent adds tokens by construction, so the question is whether
	// the injected context buys more verified findings per token spent.
	CartographerEnabled bool `json:"cartographer_enabled"`
	// SandboxExecs is the total number of sandbox_exec tool calls made by
	// refuter agents during the verification stage. Zero when the feature is
	// disabled or unused.
	SandboxExecs int `json:"sandbox_execs,omitempty"`
	// SandboxExecMillis is the total wall-clock time spent in sandbox
	// executions, in milliseconds.
	SandboxExecMillis int64 `json:"sandbox_exec_millis,omitempty"`
	// LeadsPosted is the number of cross-lens leads successfully posted to the
	// blackboard by finder agents during this run.
	LeadsPosted int `json:"leads_posted,omitempty"`
	// LeadsConsumed is the number of pending cross-lens leads that were claimed
	// and injected into finder tasks at the start of this run's hypothesize stage.
	LeadsConsumed int `json:"leads_consumed,omitempty"`
	// HeatOrdered reports whether the Sweep targets were reordered by
	// churn-heat before chunking (i.e. heat ordering ran AND produced a
	// non-trivial reordering compared to alphabetical). False when heat
	// ordering was disabled, git history was unavailable, or the heat map
	// was empty.
	HeatOrdered bool `json:"heat_ordered,omitempty"`
	// HeatFiles is the number of files in the heat map that scored above
	// zero for this Sweep. Zero when heat ordering was disabled or git
	// history was unavailable.
	HeatFiles int `json:"heat_files,omitempty"`
	// SweepNeverScanned is the number of files in the sweep's group 1 (never
	// scanned or at epoch sentinel). Zero when heat ordering is disabled.
	SweepNeverScanned int `json:"sweep_never_scanned,omitempty"`
	// SweepChangedSinceScan is the number of files admitted to the sweep's
	// group 1 because their current fingerprint differs from the content hash
	// recorded at their last scan. Zero when heat ordering is disabled.
	SweepChangedSinceScan int `json:"sweep_changed_since_scan,omitempty"`
	// CoveredFiles is the count of files that were actually covered (i.e. at
	// least one finderOK unit ran against them) in this run.
	CoveredFiles int `json:"covered_files,omitempty"`
	// Interrupted is set when the scan run was cancelled (context deadline
	// exceeded or context cancellation, e.g. SIGINT). The stats reflect whatever
	// stages completed before the interruption. The scan_runs row is sealed with
	// finished_at set so no row is left dangling.
	Interrupted bool `json:"interrupted,omitempty"`
	// Aborted is set when the scan run exited due to an unexpected internal
	// error (not a context cancellation). Partial stats are recorded and the
	// scan_runs row is sealed so no row is left dangling.
	Aborted bool `json:"aborted,omitempty"`
	// FinderAborted is set when the finder-stage circuit breaker tripped
	// (bugbot-2uz): a transport-error threshold was reached with zero
	// finderOK successes, so the funnel stopped launching further finder
	// units and cancelled in-flight ones. The already-recorded
	// FinderFailures are kept — MostFindersFailed() still reports the run as
	// unreliable — but this flag surfaces the abort reason distinctly from a
	// normal "all units ran and failed" run. A downstream consumer can tell
	// "we ran every unit and they all failed" from "we aborted after the
	// first wave of transport failures and never launched the rest".
	FinderAborted bool `json:"finder_aborted,omitempty"`
	// SeamsFound is the number of cross-language contract surfaces
	// (shared data files + shared env vars) discovered by
	// ingest.EnumerateSeams on this run's snapshot. The boundary lens
	// emits one custom finder unit per seam, so SeamsFound is also the
	// upper bound on SeamsCovered.
	SeamsFound int `json:"seams_found,omitempty"`
	// SeamsCovered is the count of seams that produced a finished (ok or
	// budget-truncated) finder unit. Equal to SeamsFound minus the seams
	// whose units were budget-skipped or never launched because the run
	// stopped early. SeamsCovered <= SeamsFound always.
	SeamsCovered int `json:"seams_covered,omitempty"`
}

// FinderReliable reports whether the finder stage produced trustworthy coverage:
// at least one finder ran and none of the finders that ran failed to parse. When
// false, an empty or sparse finding set is suspect — some lens's output was lost,
// not genuinely clean.
func (s Stats) FinderReliable() bool {
	return s.FinderRuns > 0 && s.FinderFailures == 0
}

// MostFindersFailed reports whether a strict majority of the finders that ran
// failed to produce parseable output. A scan in this state has effectively no
// signal and should exit nonzero so automation does not treat it as "clean".
func (s Stats) MostFindersFailed() bool {
	return s.FinderRuns > 0 && s.FinderFailures*2 > s.FinderRuns
}

// Result summarizes a completed funnel run for the caller.
type Result struct {
	// ScanRunID is the store scan-run this funnel recorded under.
	ScanRunID string
	// Commit is the snapshot commit the scan ran against.
	Commit string
	// Findings are the persisted Tier 2 survivors, sorted critical-first.
	Findings []store.Finding
	// Stats is the per-stage accounting.
	Stats Stats
	// Degraded reports whether the run crossed the soft budget and reduced its
	// lens set / refuter count.
	Degraded bool
	// Stopped reports whether the run hit the hard budget: it stopped launching
	// new agents and truncated in-flight ones at their next turn boundary.
	Stopped bool
	// Skipped lists human-readable notes about work the run deliberately did not
	// do (degradation, hard-budget stops). Never silent truncation.
	Skipped []string
	// CoveredFiles is the deduplicated, sorted list of files that were actually
	// covered by at least one finderOK unit in this run. Files from parse-failed,
	// budget-stopped, or budget-skipped units are NOT included. The diff-intent
	// custom unit (files == nil) contributes nothing here.
	CoveredFiles []string
}

// spendRecorder implements llm.Recorder, writing each completion's usage to the
// store's spend ledger under the active scan run and tracking a running total
// for budget decisions. It is safe for concurrent use by parallel agents.
type spendRecorder struct {
	ctx       context.Context
	store     *store.Store
	scanRunID string

	mu           sync.Mutex
	totalTokens  int64 // raw input+output, for honest ledger/status reporting
	chargeable   int64 // cache-discounted, for budget gating (overSoft/overHard)
	inTokens     int64
	outTokens    int64
	cacheRead    int64
	cacheCreated int64

	// onRecord, when non-nil, is called after each ledger update with the new
	// cumulative input/output/cache-read totals. The funnel uses it to emit
	// progress spend ticks. It must be cheap and non-blocking (it runs on the
	// agent request path).
	onRecord func(in, out, cached int64)

	// pool, when non-nil, is the shared budget pool. Every completion's
	// input+output tokens are added to it as they are ledgered, so concurrent
	// in-flight runs see the run-spanning spend total via their pre-turn
	// Limits.BudgetCheck hook. Budget accounting uses total InputTokens (tokens
	// processed), which is INCLUSIVE of cached tokens per the llm.Usage
	// convention — cache reads are not subtracted (see funnel doc comment).
	pool *agent.BudgetPool

	// finderPool / verifyPool are the role-scoped sub-pools that back the
	// downstream-budget reservation (see budgetState.reserveForDownstream). When
	// non-nil, each completion's chargeable tokens are ALSO added to the pool
	// matching its role (finder -> finderPool, anything else -> verifyPool), in
	// addition to the total pool above. They are nil unless a reservation is in
	// effect, so the default single-pool accounting is byte-for-byte unchanged.
	finderPool *agent.BudgetPool
	verifyPool *agent.BudgetPool

	// cacheReadWeight discounts cache reads when charging the budget pool, so
	// run-spanning accounting matches the per-run overBudget check. 1.0 = no
	// discount.
	cacheReadWeight float64
}

func (r *spendRecorder) Record(ev llm.UsageEvent) {
	w := r.cacheReadWeight
	if w <= 0 {
		w = 1.0
	}
	r.mu.Lock()
	r.totalTokens += ev.Usage.InputTokens + ev.Usage.OutputTokens
	r.chargeable += ev.Usage.ChargeableTokens(w)
	r.inTokens += ev.Usage.InputTokens
	r.outTokens += ev.Usage.OutputTokens
	r.cacheRead += ev.Usage.CacheReadInputTokens
	r.cacheCreated += ev.Usage.CacheCreationInputTokens
	in, out, cached := r.inTokens, r.outTokens, r.cacheRead
	cb := r.onRecord
	r.mu.Unlock()
	// Charge the shared budget pool with this completion's CHARGEABLE tokens
	// (cache reads discounted), so concurrent in-flight runs observe the new
	// run-spanning total at their next pre-turn check. Done outside the lock:
	// the pool is independently concurrency-safe.
	charge := ev.Usage.ChargeableTokens(w)
	r.pool.Add(charge)
	// Under a downstream-budget reservation, also charge the role-scoped sub-pool
	// so each stage gates on its own allowance. Both Add calls are nil-safe, so
	// this is a no-op when no reservation is active.
	if ev.Role == roleFinder || ev.Role == roleCartographer {
		r.finderPool.Add(charge)
	} else {
		r.verifyPool.Add(charge)
	}
	if cb != nil {
		cb(in, out, cached)
	}
	// Persist the ledger entry. Spend recording must not abort a scan, so a
	// failure here is swallowed; the in-memory totals (used for budget) remain
	// authoritative for this run regardless.
	_, _ = r.store.RecordSpend(r.ctx, store.Spend{
		ScanRunID:           r.scanRunID,
		Role:                ev.Role,
		Provider:            ev.Provider,
		Model:               ev.Model,
		InputTokens:         ev.Usage.InputTokens,
		OutputTokens:        ev.Usage.OutputTokens,
		CacheReadTokens:     ev.Usage.CacheReadInputTokens,
		CacheCreationTokens: ev.Usage.CacheCreationInputTokens,
	})
}

// chargeableTotal returns the cumulative cache-discounted tokens spent so far,
// the basis for every budget decision (soft degrade, hard stop, pool). Keeping
// this separate from total() means the store ledger and status pane stay
// honest (raw) while gating reflects real cost.
func (r *spendRecorder) chargeableTotal() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.chargeable
}

func (r *spendRecorder) totals() (in, out, cacheRead, cacheCreated int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inTokens, r.outTokens, r.cacheRead, r.cacheCreated
}

// budgetState tracks degradation/stop decisions for one run. Methods are safe
// for concurrent use.
type budgetState struct {
	budget          int64 // 0 = unlimited
	rec             *spendRecorder
	cacheReadWeight float64
	// pool is the shared, run-spanning token pool that every in-flight runner
	// consults pre-turn (via agent.Limits.BudgetCheck). It is the same pool the
	// recorder charges. Nil when TokenBudget is unlimited.
	pool *agent.BudgetPool

	// Downstream-budget reservation (see reserveForDownstream). When active,
	// the total budget is partitioned so the finder stage may consume at most
	// finderBudget and the verifier is guaranteed verifyBudget. finderPool /
	// verifyPool are the role-scoped pools the recorder charges and each stage
	// gates on independently. All four are zero/nil unless a reservation is in
	// effect, in which case the funnel runs in the default single-pool mode and
	// both stages share `pool` exactly as before.
	finderBudget int64
	verifyBudget int64
	finderPool   *agent.BudgetPool
	verifyPool   *agent.BudgetPool

	// finderClaim / verifyClaim are the per-task token claims (Options.
	// FinderTokenClaim / VerifierTokenClaim, default DefaultTokenClaim). They cap
	// the per-run TokenBudget of a single finder / verifier agent run so one
	// breadth-heavy run cannot be granted a whole stage's reserve at launch. The
	// shared pool is charged only for tokens ACTUALLY spent (the recorder), so a
	// run that finishes under its claim leaves the remainder in the pool for its
	// siblings — the claim is "returned to the pool" by never being removed.
	// Zero means no per-task cap (the run may use the pool's full remainder).
	finderClaim int64
	verifyClaim int64

	degraded atomic.Bool
	stopped  atomic.Bool
}

// reserveForDownstream partitions the run's total token budget so the finder
// stage may consume at most finderShare of it, RESERVING the remainder for
// downstream verification (and, transitively, reproduction — which can only run
// on verified Tier-2 survivors). Without this, the finder stage launches first
// and in bulk and drains the whole shared pool before any candidate reaches the
// verifier, so every finding is orphaned as Tier-3 and reproduction starves
// (bugbot-3lt). The two sub-pools are charged by role via the recorder and gate
// each stage on its own allowance, so the verifier keeps full refuter strength
// within its reserve instead of degrading the instant finders fill their share.
//
// It is a no-op (leaving the default single-pool mode intact) when the budget is
// unlimited or finderShare is outside (0,1): a share >= 1 is an explicit "no
// reservation" request, and a degenerate split that would leave the verifier
// zero reserve falls back to the shared pool rather than an unlimited verifier.
// Must be called before any spend is recorded.
func (b *budgetState) reserveForDownstream(finderShare float64) {
	if b.pool == nil || b.budget <= 0 {
		return // unlimited: nothing to partition
	}
	if finderShare <= 0 || finderShare >= 1 {
		return // no reservation: both stages share the total pool
	}
	finderBudget := int64(float64(b.budget) * finderShare)
	verifyBudget := b.budget - finderBudget
	if finderBudget <= 0 || verifyBudget <= 0 {
		return // degenerate rounding: keep the single shared pool
	}
	b.finderBudget = finderBudget
	b.verifyBudget = verifyBudget
	b.finderPool = agent.NewBudgetPool(finderBudget)
	b.verifyPool = agent.NewBudgetPool(verifyBudget)
	b.rec.finderPool = b.finderPool
	b.rec.verifyPool = b.verifyPool
}

// newBudgetState wires a budgetState and its shared pool to the recorder. A
// non-positive budget is unlimited: the pool is nil and every pool method is a
// no-op, so there is no per-turn check and no allowance clamping.
func newBudgetState(budget int64, rec *spendRecorder, cacheReadWeight float64) *budgetState {
	var pool *agent.BudgetPool
	if budget > 0 {
		pool = agent.NewBudgetPool(budget)
	}
	rec.pool = pool
	rec.cacheReadWeight = cacheReadWeight
	return &budgetState{budget: budget, rec: rec, pool: pool, cacheReadWeight: cacheReadWeight}
}

// runnerLimitsForPool derives the per-run agent.Limits for a runner launched
// now against the given pool, layering it onto the caller's base limits. The
// per-run TokenBudget is the tightest of three bounds: the role's per-task
// CLAIM (the claimant cap, default DefaultTokenClaim), this run's own base
// budget if it set one, and the pool's remaining headroom at launch. The
// BudgetCheck hook stops an in-flight run at the next turn once the pool is
// exhausted by *other* concurrent runs. Returns base unchanged when pool is
// nil (unlimited budget, or no reservation for this role).
//
// The claim is a CAP, not a held reservation: the shared pool is charged only
// for tokens actually spent (via the recorder), so a run that finishes under
// its claim leaves the unspent remainder in the pool for sibling runs. This is
// the claimant system's "return to the pool on closure" — realised by never
// removing the unspent budget in the first place, which avoids the utilisation
// loss and concurrency hazard of holding a reservation for the whole run.
func (b *budgetState) runnerLimitsForPool(base agent.Limits, pool *agent.BudgetPool, claim int64) agent.Limits {
	if pool == nil {
		return base
	}
	out := base
	out.BudgetCheck = pool.Check
	out.CacheReadWeight = b.cacheReadWeight
	// The per-run allowance starts at the pool's remaining headroom and is
	// tightened by the per-task claim and an explicit base budget. base.TokenBudget
	// 0 means "agent default" and negative means "unlimited per run"; both are
	// superseded here by the claim/remaining bounds. A zero result is forced to a
	// 1-token sentinel so Limits.resolve() does not reinterpret it as "use the
	// full default", giving a near-exhausted pool a runner that stops pre-turn.
	allow := pool.Remaining()
	if claim > 0 && claim < allow {
		allow = claim
	}
	if base.TokenBudget > 0 && base.TokenBudget < allow {
		allow = base.TokenBudget
	}
	out.TokenBudget = allow
	if out.TokenBudget == 0 {
		out.TokenBudget = 1
	}
	return out
}

// finderRunnerLimits / verifyRunnerLimits select the role-scoped pool under a
// downstream reservation, falling back to the shared total pool when no
// reservation is in effect for that role. Either way the role's per-task claim
// (finderClaim / verifyClaim) caps the per-run allowance so a single run cannot
// be granted the whole sub-pool at launch.
func (b *budgetState) finderRunnerLimits(base agent.Limits) agent.Limits {
	if b.finderPool != nil {
		return b.runnerLimitsForPool(base, b.finderPool, b.finderClaim)
	}
	return b.runnerLimitsForPool(base, b.pool, b.finderClaim)
}

func (b *budgetState) verifyRunnerLimits(base agent.Limits) agent.Limits {
	if b.verifyPool != nil {
		return b.runnerLimitsForPool(base, b.verifyPool, b.verifyClaim)
	}
	return b.runnerLimitsForPool(base, b.pool, b.verifyClaim)
}

// overSoft reports whether cumulative spend has crossed the soft (degradation)
// threshold. Always false when the budget is unlimited.
func (b *budgetState) overSoft() bool {
	if b.budget <= 0 || b.rec == nil {
		return false
	}
	return b.rec.chargeableTotal()*softBudgetDenom > b.budget*softBudgetNumer
}

// overHard reports whether cumulative spend has reached or exceeded the budget.
// Always false when the budget is unlimited.
func (b *budgetState) overHard() bool {
	if b.budget <= 0 || b.rec == nil {
		return false
	}
	return b.rec.chargeableTotal() >= b.budget
}

// finderOverSoft / finderOverHard gate the finder stage. Under a downstream
// reservation they measure finder-role spend against the finder sub-budget;
// otherwise they fall back to the total-budget gates (default single-pool mode).
func (b *budgetState) finderOverSoft() bool {
	if b.finderPool != nil {
		return b.finderPool.Spent()*softBudgetDenom > b.finderBudget*softBudgetNumer
	}
	return b.overSoft()
}

func (b *budgetState) finderOverHard() bool {
	if b.finderPool != nil {
		return b.finderPool.Spent() >= b.finderBudget
	}
	return b.overHard()
}

// verifyOverSoft / verifyOverHard gate the verify stage. Under a downstream
// reservation they measure verify-role spend against the RESERVED verify
// sub-budget — so the verifier degrades/stops only when it has consumed its own
// reserve, never merely because finders filled theirs. Without a reservation
// they fall back to the total-budget gates.
func (b *budgetState) verifyOverSoft() bool {
	if b.verifyPool != nil {
		return b.verifyPool.Spent()*softBudgetDenom > b.verifyBudget*softBudgetNumer
	}
	return b.overSoft()
}

func (b *budgetState) verifyOverHard() bool {
	if b.verifyPool != nil {
		return b.verifyPool.Spent() >= b.verifyBudget
	}
	return b.overHard()
}

// sortFindings orders findings critical-first, then by file/line for stable
// output.
func sortFindings(fs []store.Finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		si, sj := severityRank(fs[i].Severity), severityRank(fs[j].Severity)
		if si != sj {
			return si < sj
		}
		if fs[i].File != fs[j].File {
			return fs[i].File < fs[j].File
		}
		return fs[i].Line < fs[j].Line
	})
}

// severityRank maps a severity string to a sort key (lower = more severe).
func severityRank(s string) int {
	switch s {
	case "critical":
		return 0
	case "high":
		return 1
	case "medium":
		return 2
	case "low":
		return 3
	default:
		return 4
	}
}
