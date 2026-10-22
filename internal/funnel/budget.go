package funnel

// budget.go holds the spend-recorder and budget-state machine extracted from
// funnel.go for readability. Pure code motion: no logic changes.

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/dpoage/bugbot/internal/store"
	llmkit "github.com/dpoage/llmkit"
	"github.com/dpoage/llmkit/agent"
)

// spendRecorder writes each completion's usage to the store's spend ledger
// under the active scan run and tracks running totals for budget decisions. It
// is safe for concurrent use by parallel agents.
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

	// pool, when non-nil, is the shared budget pool, charged with every
	// completion's chargeable tokens (see record).
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

// record ledgers one completion's usage under role. runnerPool is the pool the
// completing Runner already charged itself (agent.WithBudgetPool); it is skipped
// so no pool is charged twice.
func (r *spendRecorder) record(role, provider, model string, u llmkit.Usage, runnerPool *agent.BudgetPool) {
	w := r.cacheReadWeight
	if w <= 0 {
		w = 1.0
	}
	charge := u.ChargeableTokens(w)
	r.mu.Lock()
	r.totalTokens += u.InputTokens + u.OutputTokens
	r.chargeable += charge
	r.inTokens += u.InputTokens
	r.outTokens += u.OutputTokens
	r.cacheRead += u.CacheReadInputTokens
	r.cacheCreated += u.CacheCreationInputTokens
	in, out, cached := r.inTokens, r.outTokens, r.cacheRead
	cb := r.onRecord
	r.mu.Unlock()
	// Charge outside the lock: the pools are independently concurrency-safe and
	// nil-safe, so the role sub-pool charge is a no-op without a reservation.
	charges := [2]*agent.BudgetPool{r.pool, r.verifyPool}
	if role == roleFinder || role == roleCartographer {
		charges[1] = r.finderPool
	}
	for _, p := range charges {
		if p != runnerPool {
			p.Add(charge)
		}
	}
	if cb != nil {
		cb(in, out, cached)
	}
	// Spend recording must not abort a scan; the in-memory totals stay
	// authoritative for budget decisions.
	_, _ = r.store.RecordSpend(r.ctx, store.Spend{
		ScanRunID:           r.scanRunID,
		Role:                role,
		Provider:            provider,
		Model:               model,
		InputTokens:         u.InputTokens,
		OutputTokens:        u.OutputTokens,
		CacheReadTokens:     u.CacheReadInputTokens,
		CacheCreationTokens: u.CacheCreationInputTokens,
	})
}

// observer ledgers every successful completion event under role; runnerPool
// is as in record.
func (r *spendRecorder) observer(role string, runnerPool *agent.BudgetPool) llmkit.Observer {
	return llmkit.ObserverFunc(func(_ context.Context, ev llmkit.Event) {
		if ev.Kind != llmkit.KindCompletion || ev.Completion == nil || ev.Completion.Err != "" {
			return
		}
		c := ev.Completion
		r.record(role, c.Provider, c.Model, c.Response.Usage, runnerPool)
	})
}

// ledgered binds c to the spend ledger under role. A Runner claims its
// completions, so runnerClient must unwrap c and install the ledger per Runner;
// a bare Complete on c ledgers through llmkit.Observe.
func (r *spendRecorder) ledgered(c llmkit.Client, role string) llmkit.Client {
	return &ledgeredClient{Client: llmkit.Observe(c, r.observer(role, nil)), inner: c, rec: r, role: role}
}

type ledgeredClient struct {
	llmkit.Client
	inner llmkit.Client
	rec   *spendRecorder
	role  string
}

func (c *ledgeredClient) Identity() llmkit.Identity { return llmkit.IdentityOf(c.inner) }

// runnerClient returns the client a Runner over c should drive and the options
// that ledger its completions and gate it on pool (nil = no pool).
func runnerClient(c llmkit.Client, pool *agent.BudgetPool) (llmkit.Client, []agent.Option) {
	var opts []agent.Option
	if pool != nil {
		opts = append(opts, agent.WithBudgetPool(pool))
	}
	if lc, ok := c.(*ledgeredClient); ok {
		return lc.inner, append(opts, agent.WithObserver(lc.rec.observer(lc.role, pool)))
	}
	return c, opts
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
	// consults pre-turn (agent.WithBudgetPool). Nil when TokenBudget is unlimited.
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

	// arbiterClaim is the per-task token claim for the split-verdict arbiter
	// (Options.ArbiterTokenClaim, default DefaultArbiterTokenClaim ~5x the
	// refuter claim). The arbiter draws from verifyPool like a refuter but is
	// capped at this larger claim so it can drive the split to ground; see
	// arbiterRunnerLimits (bugbot-mi5.17).
	arbiterClaim int64

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

// runnerBudget is one Runner's budget wiring: its per-run limits and the pool
// it gates on pre-turn and charges (agent.WithBudgetPool; nil = none).
type runnerBudget struct {
	limits agent.Limits
	pool   *agent.BudgetPool
}

// runnerLimitsForPool derives the budget for a runner launched now against the
// given pool, layering it onto the caller's base limits. The per-run
// TokenBudget is the tightest of three bounds: the role's per-task CLAIM (the
// claimant cap, default DefaultTokenClaim), this run's own base budget if it
// set one, and the pool's remaining headroom at launch. The pool stops an
// in-flight run at the next turn once *other* concurrent runs exhaust it.
// Returns base and no pool when pool is nil (unlimited budget).
//
// The claim is a CAP, not a held reservation: pools are charged only for
// tokens actually spent, so a run that finishes under its claim leaves the
// unspent remainder in the pool for sibling runs.
func (b *budgetState) runnerLimitsForPool(base agent.Limits, pool *agent.BudgetPool, claim int64) runnerBudget {
	if pool == nil {
		return runnerBudget{limits: base}
	}
	out := base
	out.CacheReadWeight = b.cacheReadWeight
	// base.TokenBudget 0 means "agent default" and negative "unlimited per
	// run"; both are superseded by the claim/remaining bounds. A zero result is
	// forced to 1 so the agent does not read it as "use the default".
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
	return runnerBudget{limits: out, pool: pool}
}

// finderRunnerLimits / verifyRunnerLimits select the role-scoped pool under a
// downstream reservation, falling back to the shared total pool when no
// reservation is in effect for that role. Either way the role's per-task claim
// (finderClaim / verifyClaim) caps the per-run allowance so a single run cannot
// be granted the whole sub-pool at launch.
func (b *budgetState) finderRunnerLimits(base agent.Limits) runnerBudget {
	if b.finderPool != nil {
		return b.runnerLimitsForPool(base, b.finderPool, b.finderClaim)
	}
	return b.runnerLimitsForPool(base, b.pool, b.finderClaim)
}

func (b *budgetState) verifyRunnerLimits(base agent.Limits) runnerBudget {
	if b.verifyPool != nil {
		return b.runnerLimitsForPool(base, b.verifyPool, b.verifyClaim)
	}
	return b.runnerLimitsForPool(base, b.pool, b.verifyClaim)
}

// arbiterRunnerLimits derives the per-run limits for the split-verdict arbiter.
// The arbiter is a verify-stage agent, so it draws from the same verifyPool as
// the refuters, but it is capped at arbiterClaim (DefaultArbiterTokenClaim, ~5x
// the refuter claim) rather than verifyClaim: its task is strictly harder (it
// drives the split to ground with its own tool calls), so it gets a materially
// larger per-run allowance. Splits are rare, so the higher per-run cap barely
// moves total scan spend (bugbot-mi5.17).
func (b *budgetState) arbiterRunnerLimits(base agent.Limits) runnerBudget {
	if b.verifyPool != nil {
		return b.runnerLimitsForPool(base, b.verifyPool, b.arbiterClaim)
	}
	return b.runnerLimitsForPool(base, b.pool, b.arbiterClaim)
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
