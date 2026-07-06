package funnel

// budget.go holds the spend-recorder and budget-state machine extracted from
// funnel.go for readability. Pure code motion: no logic changes.

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/store"
)

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

// arbiterRunnerLimits derives the per-run limits for the split-verdict arbiter.
// The arbiter is a verify-stage agent, so it draws from the same verifyPool as
// the refuters, but it is capped at arbiterClaim (DefaultArbiterTokenClaim, ~5x
// the refuter claim) rather than verifyClaim: its task is strictly harder (it
// drives the split to ground with its own tool calls), so it gets a materially
// larger per-run allowance. Splits are rare, so the higher per-run cap barely
// moves total scan spend (bugbot-mi5.17).
func (b *budgetState) arbiterRunnerLimits(base agent.Limits) agent.Limits {
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
