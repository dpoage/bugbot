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
	"github.com/dpoage/bugbot/internal/store"
)

// Defaults for Options. Exported so callers (CLI flags) can present them.
const (
	// DefaultRefuters is the number of adversarial refuter agents run per
	// candidate. Three gives a meaningful majority vote without tripling cost
	// versus one.
	DefaultRefuters = 3
	// DefaultMaxParallel bounds concurrently-running agents across the run.
	DefaultMaxParallel = 4
	// DefaultChunkSize is the number of target files handed to a single finder
	// invocation. Chunking keeps each finder's context focused and lets large
	// repos parallelize within a lens.
	DefaultChunkSize = 30

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
	// FinderLimits / VerifierLimits bound each individual agent run (iterations
	// and per-run token budget). Zero-value fields resolve to agent defaults.
	FinderLimits   agent.Limits
	VerifierLimits agent.Limits
	// TranscriptDir, when non-empty, makes every agent auto-save its transcript
	// there.
	TranscriptDir string
	// Progress, when non-nil, receives activity events as the run proceeds
	// (stage boundaries, agent start/finish, spend ticks, budget degradation).
	// Emission is best-effort and must never block or fail the run; a nil sink
	// disables emission. See internal/progress for the contract.
	Progress progress.Sink
}

// resolve fills in defaults without mutating the caller's Options.
func (o Options) resolve() Options {
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
	return o
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
	return &Funnel{
		clients: clients,
		store:   st,
		repo:    repo,
		opts:    resolved,
		lenses:  selectLenses(resolved.Lenses),
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
	// InputTokens / OutputTokens is the run's total token spend. InputTokens
	// includes cached tokens (the llm.Usage convention).
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	// CacheReadTokens / CacheCreationTokens are the subsets of InputTokens
	// served from / written to the provider's prompt cache, for reporting
	// cache savings.
	CacheReadTokens     int64 `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens int64 `json:"cache_creation_tokens,omitempty"`
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
	totalTokens  int64
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
}

func (r *spendRecorder) Record(ev llm.UsageEvent) {
	r.mu.Lock()
	r.totalTokens += ev.Usage.InputTokens + ev.Usage.OutputTokens
	r.inTokens += ev.Usage.InputTokens
	r.outTokens += ev.Usage.OutputTokens
	r.cacheRead += ev.Usage.CacheReadInputTokens
	r.cacheCreated += ev.Usage.CacheCreationInputTokens
	in, out, cached := r.inTokens, r.outTokens, r.cacheRead
	cb := r.onRecord
	r.mu.Unlock()
	// Charge the shared budget pool with this completion's tokens, so concurrent
	// in-flight runs observe the new run-spanning total at their next pre-turn
	// check. Done outside the lock: the pool is independently concurrency-safe.
	r.pool.Add(ev.Usage.InputTokens + ev.Usage.OutputTokens)
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

// total returns the cumulative tokens spent so far this run.
func (r *spendRecorder) total() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.totalTokens
}

func (r *spendRecorder) totals() (in, out, cacheRead, cacheCreated int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inTokens, r.outTokens, r.cacheRead, r.cacheCreated
}

// budgetState tracks degradation/stop decisions for one run. Methods are safe
// for concurrent use.
type budgetState struct {
	budget int64 // 0 = unlimited
	rec    *spendRecorder
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
func newBudgetState(budget int64, rec *spendRecorder) *budgetState {
	var pool *agent.BudgetPool
	if budget > 0 {
		pool = agent.NewBudgetPool(budget)
	}
	rec.pool = pool
	return &budgetState{budget: budget, rec: rec, pool: pool}
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
	return b.rec.total()*softBudgetDenom > b.budget*softBudgetNumer
}

// overHard reports whether cumulative spend has reached or exceeded the budget.
// Always false when the budget is unlimited.
func (b *budgetState) overHard() bool {
	if b.budget <= 0 || b.rec == nil {
		return false
	}
	return b.rec.total() >= b.budget
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
