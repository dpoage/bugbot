package funnel

import (
	"context"
	"fmt"
	"strings"
	"sync"
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
// degradation). A strict majority of "refuted" verdicts kills the candidate;
// survivors carry the refuters' reasoning into their finding's trace.
//
// Distinct candidates are verified in parallel, bounded by Options.MaxParallel.
// The refuters for a single candidate run sequentially within that candidate's
// goroutine — they are independent votes, and serializing them keeps the
// MaxParallel bound meaningful (it bounds candidates in flight, not the product
// of candidates and refuters).
func (f *Funnel) verify(ctx context.Context, verifier llm.Client, candidates []Candidate, budget *budgetState, result *Result) ([]verified, int, []Candidate, error) {
	if len(candidates) == 0 {
		return nil, 0, nil, nil
	}

	tools, err := f.readOnlyTools()
	if err != nil {
		return nil, 0, nil, err
	}

	var (
		mu        sync.Mutex
		survivors []verified
		orphaned  []Candidate
		killed    int
		firstErr  error
	)
	sem := make(chan struct{}, f.opts.MaxParallel)
	var wg sync.WaitGroup

	for _, c := range candidates {
		mu.Lock()
		stop := firstErr != nil
		mu.Unlock()
		if stop {
			break
		}

		wg.Add(1)
		c := c
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

			sink := f.opts.Progress
			start := time.Now()
			progress.Emit(sink, progress.Event{
				Kind: progress.KindAgentStarted, Role: progress.RoleVerifier, Label: c.Title,
			})
			verdicts, tokens, stopped, err := f.runRefuters(ctx, verifier, tools, c, nRefuters, budget)
			progress.Emit(sink, progress.Event{
				Kind: progress.KindAgentFinished, Role: progress.RoleVerifier, Label: c.Title,
				Tokens: tokens, Duration: time.Since(start), Err: errString(err),
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			// The pool ran dry while this candidate was being challenged: its
			// verdicts are incomplete, so we cannot trust a "survived" or "killed"
			// conclusion. Treat it as budget-orphaned (T3 suspected) rather than
			// silently passing a half-verified candidate as a T2 survivor.
			if stopped {
				budget.stopped.Store(true)
				orphaned = append(orphaned, c)
				msg := fmt.Sprintf("budget stopped mid-verification of %q (%s:%d) — kept as T3 suspected", c.Title, c.File, c.Line)
				f.note(result, msg)
				progress.Emit(sink, progress.Event{Kind: progress.KindBudgetStopped, Message: msg})
				return
			}
			if majorityRefuted(verdicts) {
				killed++
				return
			}
			progress.Emit(sink, progress.Event{
				Kind: progress.KindFindingVerified, Title: c.Title, File: c.File, Line: c.Line,
			})
			survivors = append(survivors, verified{
				cand:      c,
				reasoning: buildReasoning(verdicts),
			})
		}()
	}

	wg.Wait()
	if firstErr != nil {
		return nil, 0, nil, firstErr
	}
	return survivors, killed, orphaned, nil
}

// runRefuters runs n independent refuter agents on a candidate and returns
// their verdicts. A refuter that fails to produce a parseable verdict is
// treated as "not refuted" (it could not prove the bug wrong), which is the
// precision-conservative default: a broken refuter must not be able to silently
// kill a candidate. Context cancellation propagates.
//
// The returned stopped flag is true when a refuter run was cut short by the
// shared budget pool (TruncBudgetPool): once the pool is dry, the remaining
// votes for this candidate would be untrustworthy, so the caller treats the
// candidate as budget-orphaned rather than reaching a verdict on a partial vote.
// Limits are derived from the pool at launch so a refuter launched late gets the
// remaining headroom and one in flight stops at its next turn.
func (f *Funnel) runRefuters(ctx context.Context, verifier llm.Client, tools []agent.Tool, c Candidate, n int, budget *budgetState) ([]refutation, int64, bool, error) {
	runner := agent.NewRunner(verifier, tools, verifierSystemBase,
		agent.WithLimits(budget.runnerLimits(f.opts.VerifierLimits)),
		f.transcriptOption(),
	)

	var tokens int64
	verdicts := make([]refutation, 0, n)
	for i := 0; i < n; i++ {
		if err := ctx.Err(); err != nil {
			return nil, tokens, false, err
		}
		var v refutation
		outcome, err := runner.RunJSON(ctx, verifierTask(c), refutationSchema, &v)
		if outcome != nil {
			tokens += outcome.Usage.InputTokens + outcome.Usage.OutputTokens
			// A budget-pool stop means this refuter never got to complete its
			// challenge; stop voting and signal the caller to orphan the candidate.
			if outcome.TruncationReason == agent.TruncBudgetPool {
				return verdicts, tokens, true, nil
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil, tokens, false, ctx.Err()
			}
			// Unparseable verdict => could not refute.
			v = refutation{Refuted: false, Reasoning: "refuter produced no parseable verdict", Confidence: "low"}
		}
		verdicts = append(verdicts, v)
	}
	return verdicts, tokens, false, nil
}

// errString returns err.Error() or "" for a nil error, for embedding in a
// JSON-serializable progress event.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// majorityRefuted reports whether a strict majority of verdicts are "refuted".
// A tie (e.g. 1-1 with two refuters) is NOT a majority, so the candidate
// survives: killing a candidate requires the refuters to reach a clear
// consensus that it is wrong. With the default of 3 refuters, 2+ must refute to
// kill. An empty verdict set never refutes.
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
// recorded on the finding. It labels each refuter's verdict so the trace is
// legible to a human triaging the finding later.
func buildReasoning(verdicts []refutation) string {
	var b strings.Builder
	b.WriteString("Survived adversarial verification (")
	refuted := 0
	for _, v := range verdicts {
		if v.Refuted {
			refuted++
		}
	}
	fmt.Fprintf(&b, "%d/%d refuters could not disprove it):\n", len(verdicts)-refuted, len(verdicts))
	for i, v := range verdicts {
		verdict := "could not refute"
		if v.Refuted {
			verdict = "refuted"
		}
		fmt.Fprintf(&b, "  refuter %d [%s, confidence=%s]: %s\n", i+1, verdict, v.Confidence, strings.TrimSpace(v.Reasoning))
	}
	return b.String()
}
