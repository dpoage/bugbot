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
// Options.TokenBudget (0 = unlimited) bounds the whole run. The funnel tracks
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
)

// RoleClients holds the per-role LLM clients the funnel drives. Tests inject
// fakes; the CLI builds these via llm.ResolveRole. Reproducer is not used by
// this stage (Tier 1 reproduction is a later stage) and is intentionally
// absent.
type RoleClients struct {
	Finder   llm.Client
	Verifier llm.Client
}

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
	// MaxParallel bounds concurrently-running agents. Zero uses
	// DefaultMaxParallel; negative is treated as 1.
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
// Safe to call multiple times and on a funnel that never ran.
func (f *Funnel) Close() error {
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
	// VerifierRuns / VerifierFailures mirror the above for refuter agents. A
	// refuter that fails to parse is conservatively treated as "could not refute"
	// (it cannot silently kill a candidate), but the failure is still counted so
	// the verification result's reliability is visible.
	VerifierRuns     int `json:"verifier_runs"`
	VerifierFailures int `json:"verifier_failures"`
	// InputTokens / OutputTokens is the run's total token spend. InputTokens
	// includes cached tokens (the llm.Usage convention).
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	// CacheReadTokens / CacheCreationTokens are the subsets of InputTokens
	// served from / written to the provider's prompt cache, for reporting
	// cache savings.
	CacheReadTokens     int64 `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens int64 `json:"cache_creation_tokens,omitempty"`
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
	r.pool.Add(ev.Usage.ChargeableTokens(w))
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

	degraded atomic.Bool
	stopped  atomic.Bool
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

// runnerLimits derives the per-run agent.Limits for a runner launched now,
// layering the shared budget pool onto the caller's base limits. The per-run
// TokenBudget is clamped to what the pool actually has left at launch, so a
// late-launched agent gets only the remaining headroom rather than a fixed
// constant. The BudgetCheck hook stops an in-flight run at the next turn once
// the pool is exhausted by *other* concurrent runs. Both default to the base
// limits when the budget is unlimited.
func (b *budgetState) runnerLimits(base agent.Limits) agent.Limits {
	if b.pool == nil {
		return base
	}
	out := base
	out.BudgetCheck = b.pool.Check
	out.CacheReadWeight = b.cacheReadWeight
	// Clamp the per-run allowance to the remaining pool. A negative base budget
	// means "unlimited per run"; we still cap it at the pool's remainder. Note
	// this clamp only bounds a SOLO or late-launched runner: N runners launched
	// concurrently each read the same remainder, so concurrent overshoot is
	// bounded by the shared BudgetCheck hook above, not by this clamp.
	rem := b.pool.Remaining()
	// base.TokenBudget: 0 means "agent default", negative means "unlimited per
	// run". In both cases, and whenever the pool remainder is the tighter bound,
	// clamp to the remainder so the per-run allowance never exceeds what the
	// run-spanning pool can actually afford. A zero remainder yields a runner
	// that stops on its first pre-turn check, which is the desired late-launch
	// behavior. We guard against the zero-means-default resolution turning a
	// near-exhausted pool back into a full 1M allowance.
	if base.TokenBudget <= 0 || rem < base.TokenBudget {
		out.TokenBudget = rem
		if out.TokenBudget == 0 {
			// Limits.resolve() treats 0 as "use default"; force a hard stop by
			// using the smallest negative-free sentinel that still bites pre-turn.
			out.TokenBudget = 1
		}
	}
	return out
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
