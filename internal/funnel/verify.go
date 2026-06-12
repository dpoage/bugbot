package funnel

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/progress"
)

// verified pairs a surviving candidate with the refuters' reasoning that backs
// it, so persistence can record the verification trace on the finding.
type verified struct {
	cand      Candidate
	reasoning string
}

// verify runs the adversarial verification stage. Each surviving candidate is
// challenged by N refuter agents (Options.Refuters, reduced to one under budget
// degradation). When the panel is unanimous the result stands immediately;
// on a split (mixed refuted/not-refuted) a single arbiter agent reads both sides
// and issues the deciding verdict. Survivors carry the refuters' (and arbiter's)
// reasoning into their finding's trace.
//
// Distinct candidates are verified in parallel, bounded by Options.MaxParallel.
// The refuters for a single candidate run sequentially within that candidate's
// goroutine — they are independent votes, and serializing them keeps the
// MaxParallel bound meaningful (it bounds candidates in flight, not the product
// of candidates and refuters). The arbiter is also sequential inside the same
// goroutine.
func (f *Funnel) verify(ctx context.Context, verifier llm.Client, persona string, candidates []Candidate, budget *budgetState, result *Result) ([]verified, int, []Candidate, error) {
	if len(candidates) == 0 {
		return nil, 0, nil, nil
	}

	// post_lead is deliberately absent from the refuter tool set. Refuter
	// independence — no shared state or context cross-contaminating the
	// adversarial check — is the core mechanism that kills false positives. If
	// a refuter could post leads it would let a bias planted by one finder
	// agent propagate into the verification result, undermining the whole
	// adversarial design. See also: tools_post_lead.go for the tool definition
	// and hypothesize.go where it is wired for finders only.
	// Verifiers use the default (looser) read caps: a refuter panel runs few,
	// short turns over one candidate, so its history does not accrete the way a
	// finder's does and the finder-specific tightening would only risk truncating
	// the very evidence a refuter needs to confirm or kill a candidate.
	tools, err := f.readOnlyTools(agent.ReadCaps{})
	if err != nil {
		return nil, 0, nil, err
	}

	// Aggregate sandbox-exec counters across all candidates. Because distinct
	// candidates run in parallel goroutines, we use atomics here and fold them
	// into the result under the existing mu at the end.
	var sbExecs atomic.Int32
	var sbMillis atomic.Int64

	var (
		mu            sync.Mutex
		survivors     []verified
		orphaned      []Candidate
		killed        int
		refuterRuns   int
		refuterFailed int
		arbiterRuns   int
		arbiterKills  int
		arbiterFailed int
		firstErr      error
	)
	sem := make(chan struct{}, f.opts.MaxParallel)
	var wg sync.WaitGroup

	for candIdx, c := range candidates {
		mu.Lock()
		stop := firstErr != nil
		mu.Unlock()
		if stop {
			break
		}

		wg.Add(1)
		c := c
		candIdx := candIdx
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			// Gate against the live spend total once we hold a worker slot (see the
			// finder stage for the rationale). A candidate whose verification is
			// skipped here is NOT dropped: it is budget-orphaned and persisted as a
			// Tier 3 suspected finding so a human can still review it.
			if budget.overHard() {
				budget.stopped.Store(true)
				mu.Lock()
				orphaned = append(orphaned, c)
				mu.Unlock()
				msg := fmt.Sprintf("hard budget reached: verification skipped for %q (%s:%d) — kept as T3 suspected", c.Title, c.File, c.Line)
				f.note(result, msg)
				progress.Emit(f.opts.Progress, progress.Event{Kind: progress.KindBudgetStopped, Message: msg})
				// Record orphaned_budget row (zero tokens, empty started/finished). Best-effort.
				f.recordVerifierUnit(ctx, result.ScanRunID, c.Lens, c.File, candIdx,
					time.Time{}, time.Time{}, 0, "orphaned_budget", nil, nil, false, false, result)
				return
			}
			nRefuters := f.opts.Refuters
			if budget.overSoft() {
				budget.degraded.Store(true)
				if nRefuters > degradedRefuters {
					nRefuters = degradedRefuters
					msg := fmt.Sprintf("budget degraded: %q verified with %d refuter(s)", c.Title, degradedRefuters)
					f.note(result, msg)
					progress.Emit(f.opts.Progress, progress.Event{Kind: progress.KindBudgetDegraded, Message: msg})
				}
			}

			// Build the candidate-specific tool set: the shared read-only tools
			// plus, when gated in, a sandbox_exec tool scoped to this candidate.
			// Run the one-time online dependency prefetch (DepStrategyFetch) before
			// the first sandbox tool; if it fails, skip the sandbox tool for this
			// candidate (and all others) rather than handing it an unusable cache.
			candTools := tools
			if prefErr := f.ensureDepPrefetch(ctx); prefErr != nil {
				f.note(result, fmt.Sprintf("sandbox dependency prefetch failed: %v — sandbox_exec disabled", prefErr))
			} else if sbTool := f.buildSandboxTool(c, &sbExecs, &sbMillis); sbTool != nil {
				candTools = append(candTools, sbTool)
			}

			sink := f.opts.Progress
			startedAt := time.Now()
			progress.Emit(sink, progress.Event{
				Kind: progress.KindAgentStarted, Role: progress.RoleVerifier, Label: c.Title,
			})
			verdicts, seatNames, tokens, nFailed, stopped, err := f.runRefuters(ctx, verifier, candTools, persona, c, nRefuters, budget)

			// Arbiter path: if the panel is split (not all-same), run one arbiter.
			// Tokens from the arbiter are accumulated into `tokens` so the
			// AgentFinished event carries the full candidate spend.
			var localArbiterRuns, localArbiterKills, localArbiterFailed int
			var arbiterReasoning string
			var arbiterVerdict *refutation
			arbiterBudgetStopped := false
			if err == nil && !stopped && isSplitVerdict(verdicts) {
				localArbiterRuns = 1
				av, aTokens, aStopped, aErr := f.runArbiter(ctx, verifier, candTools, persona, c, verdicts, seatNames, budget)
				tokens += aTokens
				if aStopped {
					// The arbiter was cut by the shared budget pool before it could
					// complete: treat the candidate as budget-orphaned (T3 suspected),
					// exactly like the existing mid-panel budget stop above.
					arbiterBudgetStopped = true
				} else if aErr != nil && ctx.Err() == nil {
					// Arbiter produced no parseable verdict: fall back to majorityRefuted.
					// A failed arbiter must NEVER kill a candidate on its own — the fallback
					// majorityRefuted has a conservative tie-survives property.
					localArbiterFailed = 1
				} else if aErr == nil {
					arbiterVerdict = av
					if av != nil && av.Refuted {
						localArbiterKills = 1
					}
					arbiterReasoning = fmt.Sprintf("arbiter [%s, confidence=%s]: %s",
						verdictWord(av), av.Confidence, strings.TrimSpace(av.Reasoning))
				}
			}

			finishedAt := time.Now()
			progress.Emit(sink, progress.Event{
				Kind: progress.KindAgentFinished, Role: progress.RoleVerifier, Label: c.Title,
				Tokens: tokens, Duration: finishedAt.Sub(startedAt), Err: errString(err),
			})
			// Fold stats and decide the verdict under the lock; the agent_units
			// row write happens AFTER unlock so a sqlite insert never serializes
			// sibling candidates (the pre-launch orphan path already records
			// outside the lock — same discipline). The runner-error path records
			// no row: the whole scan aborts on firstErr.
			recordStatus := ""
			mu.Lock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}
			// The pool ran dry while this candidate was being challenged: its
			// verdicts are incomplete, so we cannot trust a "survived" or "killed"
			// conclusion. Treat it as budget-orphaned (T3 suspected) rather than
			// silently passing a half-verified candidate as a T2 survivor. A budget
			// stop is not a reliability failure, so the vote is NOT folded into the
			// refuter run/failure stats — for a mid-panel stop because it is
			// partial, and for a mid-ARBITER stop (where the panel did complete)
			// because an orphaned candidate contributes no verdict and folding its
			// panel would skew per-verdict reliability ratios.
			if stopped || arbiterBudgetStopped {
				budget.stopped.Store(true)
				orphaned = append(orphaned, c)
				msg := fmt.Sprintf("budget stopped mid-verification of %q (%s:%d) — kept as T3 suspected", c.Title, c.File, c.Line)
				f.note(result, msg)
				progress.Emit(sink, progress.Event{Kind: progress.KindBudgetStopped, Message: msg})
				recordStatus = "orphaned_budget"
			} else {
				refuterRuns += len(verdicts)
				refuterFailed += nFailed
				arbiterRuns += localArbiterRuns
				arbiterKills += localArbiterKills
				arbiterFailed += localArbiterFailed
				if nFailed > 0 {
					progress.Emit(sink, progress.Event{
						Kind: progress.KindLensFailed, Role: progress.RoleVerifier, Label: c.Title,
						Message: fmt.Sprintf("%d/%d refuter(s) produced no parseable verdict for %q — treated as 'could not refute'", nFailed, len(verdicts), c.Title),
					})
				}

				// Decide verdict:
				//   - Unanimous: use existing majorityRefuted logic (all-refuted kills,
				//     all-not-refuted survives). This path is byte-identical to today for
				//     unanimous panels and for n==1.
				//   - Split + arbiter succeeded: arbiter's verdict decides.
				//   - Split + arbiter failed: fall back to majorityRefuted (conservative
				//     tie-survives preserved).
				var candKilled bool
				if isSplitVerdict(verdicts) {
					if localArbiterFailed > 0 || arbiterVerdict == nil {
						// Arbiter failed: fall back to majority rule.
						candKilled = majorityRefuted(verdicts)
					} else {
						candKilled = arbiterVerdict.Refuted
					}
				} else {
					candKilled = majorityRefuted(verdicts)
				}

				if candKilled {
					killed++
					recordStatus = "killed"
					progress.Emit(sink, progress.Event{
						Kind: progress.KindFindingKilled, Title: c.Title, File: c.File, Line: c.Line,
					})
				} else {
					progress.Emit(sink, progress.Event{
						Kind: progress.KindFindingVerified, Title: c.Title, File: c.File, Line: c.Line,
					})
					recordStatus = "survived"
					survivors = append(survivors, verified{
						cand:      c,
						reasoning: buildReasoning(verdicts, seatNames, arbiterReasoning, localArbiterRuns > 0 && localArbiterFailed == 0),
					})
				}
			}
			mu.Unlock()

			// arbiterRan is false for the orphaned mid-arbiter stop: an arbiter
			// cut by the pool produced no verdict worth reporting in detail.
			arbiterRan := localArbiterRuns > 0 && localArbiterFailed == 0 && !arbiterBudgetStopped
			f.recordVerifierUnit(ctx, result.ScanRunID, c.Lens, c.File, candIdx,
				startedAt, finishedAt, tokens, recordStatus, seatNames, seatRefutedSlice(verdicts),
				arbiterRan, arbiterRefuted(arbiterVerdict), result)
		}()
	}

	wg.Wait()
	if firstErr != nil {
		return nil, 0, nil, firstErr
	}
	mu.Lock()
	result.Stats.VerifierRuns = refuterRuns
	result.Stats.VerifierFailures = refuterFailed
	result.Stats.ArbiterRuns = arbiterRuns
	result.Stats.ArbiterKills = arbiterKills
	result.Stats.ArbiterFailures = arbiterFailed
	result.Stats.SandboxExecs = int(sbExecs.Load())
	result.Stats.SandboxExecMillis = sbMillis.Load()
	mu.Unlock()
	return survivors, killed, orphaned, nil
}

// runRefuters runs n independent refuter agents on a candidate and returns
// their verdicts together with the seat names used for each position. A refuter
// that fails to produce a parseable verdict is treated as "not refuted" (it
// could not prove the bug wrong), which is the precision-conservative default:
// a broken refuter must not be able to silently kill a candidate. Context
// cancellation propagates.
//
// The returned int counts refuters that produced no parseable verdict (a
// reliability signal). The returned stopped flag is true when a refuter run was
// cut short by the shared budget pool (TruncBudgetPool): once the pool is dry,
// the remaining votes for this candidate would be untrustworthy, so the caller
// treats the candidate as budget-orphaned rather than reaching a verdict on a
// partial vote. Limits are derived from the pool at launch so a refuter launched
// late gets the remaining headroom and one in flight stops at its next turn.
//
// Each seat gets a distinct specialty clause appended to its system prompt (see
// seats.go) so the panel attacks the report from different angles. When n==1
// (budget-degraded path, degradedRefuters) no seat clause is added — the single
// generalist produces today's prompt byte-identical.
func (f *Funnel) runRefuters(ctx context.Context, verifier llm.Client, tools []agent.Tool, persona string, c Candidate, n int, budget *budgetState) ([]refutation, []string, int64, int, bool, error) {
	// Detect whether the sandbox_exec tool is present so we can tailor the
	// system prompt to mention it.
	hasSandbox := false
	for _, t := range tools {
		if t.Def().Name == "sandbox_exec" {
			hasSandbox = true
			break
		}
	}

	var tokens int64
	var failed int
	verdicts := make([]refutation, 0, n)
	seatNames := make([]string, 0, n)
	for i := 0; i < n; i++ {
		if err := ctx.Err(); err != nil {
			return nil, nil, tokens, failed, false, err
		}
		seat := seatForIndex(i, n)
		// Build the per-seat system prompt: base + optional sandbox paragraph +
		// optional seat clause. When n==1 the seat is empty (no clause appended)
		// so the single-refuter path is byte-identical to the pre-diversity code.
		sysPrompt := verifierSystemPrompt(persona, hasSandbox)
		if seat.clause != "" {
			sysPrompt += "\n\n" + seat.clause
		}
		runner := agent.NewRunner(verifier, tools, sysPrompt,
			agent.WithLimits(budget.runnerLimits(f.opts.VerifierLimits)),
			agent.WithMaxTokens(DefaultMaxOutputTokens),
			f.transcriptOption(),
		)
		var v refutation
		outcome, err := runner.RunJSON(ctx, verifierTask(c), refutationSchema, &v)
		if outcome != nil {
			tokens += outcome.Usage.InputTokens + outcome.Usage.OutputTokens
			// A budget-pool stop means this refuter never got to complete its
			// challenge; stop voting and signal the caller to orphan the candidate.
			if outcome.TruncationReason == agent.TruncBudgetPool {
				return verdicts, seatNames, tokens, failed, true, nil
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil, nil, tokens, failed, false, ctx.Err()
			}
			// Unparseable verdict => could not refute. Counted so the verification's
			// reliability is visible, but it never silently kills a candidate.
			failed++
			v = refutation{Refuted: false, Reasoning: "refuter produced no parseable verdict", Confidence: "low"}
		}
		verdicts = append(verdicts, v)
		seatNames = append(seatNames, seat.name)
	}
	return verdicts, seatNames, tokens, failed, false, nil
}

// runArbiter runs a single arbiter agent on a split-verdict candidate.
// It returns the arbiter's refutation verdict, the tokens used, a stopped flag,
// and any error. The stopped flag is true when the arbiter was cut by the shared
// budget pool (TruncBudgetPool), signalling the caller to orphan the candidate.
// An unparseable verdict is returned as a non-nil error with stopped=false so
// the caller can fall back to majorityRefuted.
//
// post_lead is absent from candTools (same rationale as refuters: refuter
// independence is the core false-positive killer; the arbiter must be equally
// independent).
func (f *Funnel) runArbiter(ctx context.Context, verifier llm.Client, candTools []agent.Tool, persona string, c Candidate, verdicts []refutation, seatNames []string, budget *budgetState) (*refutation, int64, bool, error) {
	hasSandbox := false
	for _, t := range candTools {
		if t.Def().Name == "sandbox_exec" {
			hasSandbox = true
			break
		}
	}
	runner := agent.NewRunner(verifier, candTools, arbiterSystemPrompt(persona, hasSandbox),
		agent.WithLimits(budget.runnerLimits(f.opts.VerifierLimits)),
		agent.WithMaxTokens(DefaultMaxOutputTokens),
		f.transcriptOption(),
	)
	var av refutation
	outcome, err := runner.RunJSON(ctx, arbiterTask(c, verdicts, seatNames), refutationSchema, &av)
	var tokens int64
	if outcome != nil {
		tokens = outcome.Usage.InputTokens + outcome.Usage.OutputTokens
		if outcome.TruncationReason == agent.TruncBudgetPool {
			// Budget-pool stop: the arbiter never completed its challenge.
			// Signal the caller to orphan the candidate (T3 suspected).
			return nil, tokens, true, nil
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, tokens, false, ctx.Err()
		}
		// Unparseable: the caller must fall back to majorityRefuted.
		return nil, tokens, false, err
	}
	return &av, tokens, false, nil
}

// isSplitVerdict reports whether the verdicts contain at least one refuted and
// at least one not-refuted entry. An empty slice is not split (it is unanimous
// "could not refute"). A single-refuter panel can never be split.
func isSplitVerdict(verdicts []refutation) bool {
	if len(verdicts) < 2 {
		return false
	}
	hasRefuted := false
	hasNotRefuted := false
	for _, v := range verdicts {
		if v.Refuted {
			hasRefuted = true
		} else {
			hasNotRefuted = true
		}
		if hasRefuted && hasNotRefuted {
			return true
		}
	}
	return false
}

// verdictWord returns "refuted" or "not-refuted" for a refutation verdict,
// for use in trace lines. A nil verdict returns "unknown".
func verdictWord(v *refutation) string {
	if v == nil {
		return "unknown"
	}
	if v.Refuted {
		return "refuted"
	}
	return "not-refuted"
}

// errString returns err.Error() or "" for a nil error, for embedding in a
// JSON-serializable progress event.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// seatRefutedSlice returns a parallel bool slice indicating which verdicts were
// "refuted", for use in the per-unit observability row. Returns nil when the
// slice is empty.
func seatRefutedSlice(verdicts []refutation) []bool {
	if len(verdicts) == 0 {
		return nil
	}
	out := make([]bool, len(verdicts))
	for i, v := range verdicts {
		out[i] = v.Refuted
	}
	return out
}

// arbiterRefuted returns whether the arbiter's verdict was "refuted". Returns
// false for a nil arbiter (no arbiter ran or it failed to parse).
func arbiterRefuted(v *refutation) bool {
	return v != nil && v.Refuted
}

// majorityRefuted reports whether a strict majority of verdicts are "refuted".
// A tie (e.g. 1-1 with two refuters) is NOT a majority, so the candidate
// survives: killing a candidate requires the refuters to reach a clear
// consensus that it is wrong. With the default of 3 refuters, 2+ must refute to
// kill. An empty verdict set never refutes.
//
// majorityRefuted is the fallback for split panels when the arbiter fails to
// produce a parseable verdict. Its tie-survives property is intentional and
// must be preserved: an unparseable arbiter must NEVER be able to kill a
// candidate on its own.
func majorityRefuted(verdicts []refutation) bool {
	if len(verdicts) == 0 {
		return false
	}
	refuted := 0
	for _, v := range verdicts {
		if v.Refuted {
			refuted++
		}
	}
	return refuted*2 > len(verdicts)
}

// buildReasoning concatenates the refuters' reasoning into a verification trace
// recorded on the finding. It labels each refuter's verdict with its seat name
// so the trace is legible to a human triaging the finding later.
// When arbiterRan is true, a final line for the arbiter's verdict is appended
// and the header changes to reflect arbitration.
func buildReasoning(verdicts []refutation, seatNames []string, arbiterLine string, arbiterRan bool) string {
	var b strings.Builder
	refuted := 0
	for _, v := range verdicts {
		if v.Refuted {
			refuted++
		}
	}
	n := len(verdicts)
	if arbiterRan {
		fmt.Fprintf(&b, "Survived adversarial verification (split panel decided by arbitration, %d/%d refuters could not disprove it):\n", n-refuted, n)
	} else {
		fmt.Fprintf(&b, "Survived adversarial verification (%d/%d refuters could not disprove it):\n", n-refuted, n)
	}
	for i, v := range verdicts {
		verdict := "could not refute"
		if v.Refuted {
			verdict = "refuted"
		}
		// Label with seat name when the panel has >= 2 seats (n>=2 means seats
		// were assigned). n==1 keeps the old unlabeled format.
		if n >= 2 && i < len(seatNames) && seatNames[i] != "" {
			fmt.Fprintf(&b, "  refuter %d [%s, %s, confidence=%s]: %s\n", i+1, seatNames[i], verdict, v.Confidence, strings.TrimSpace(v.Reasoning))
		} else {
			fmt.Fprintf(&b, "  refuter %d [%s, confidence=%s]: %s\n", i+1, verdict, v.Confidence, strings.TrimSpace(v.Reasoning))
		}
	}
	if arbiterRan && arbiterLine != "" {
		fmt.Fprintf(&b, "  %s\n", arbiterLine)
	}
	return b.String()
}
