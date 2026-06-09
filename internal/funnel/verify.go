package funnel

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/llm"
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
func (f *Funnel) verify(ctx context.Context, verifier llm.Client, candidates []Candidate, budget *budgetState, result *Result) ([]verified, int, error) {
	if len(candidates) == 0 {
		return nil, 0, nil
	}

	tools, err := f.readOnlyTools()
	if err != nil {
		return nil, 0, err
	}

	var (
		mu        sync.Mutex
		survivors []verified
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
			// finder stage for the rationale).
			if budget.overHard() {
				budget.stopped.Store(true)
				f.note(result, fmt.Sprintf("hard budget reached: skipped verification of %q (%s:%d)", c.Title, c.File, c.Line))
				return
			}
			nRefuters := f.opts.Refuters
			if budget.overSoft() {
				budget.degraded.Store(true)
				if nRefuters > degradedRefuters {
					nRefuters = degradedRefuters
					f.note(result, fmt.Sprintf("budget degraded: %q verified with %d refuter(s)", c.Title, degradedRefuters))
				}
			}

			verdicts, err := f.runRefuters(ctx, verifier, tools, c, nRefuters)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			if majorityRefuted(verdicts) {
				killed++
				return
			}
			survivors = append(survivors, verified{
				cand:      c,
				reasoning: buildReasoning(verdicts),
			})
		}()
	}

	wg.Wait()
	if firstErr != nil {
		return nil, 0, firstErr
	}
	return survivors, killed, nil
}

// runRefuters runs n independent refuter agents on a candidate and returns
// their verdicts. A refuter that fails to produce a parseable verdict is
// treated as "not refuted" (it could not prove the bug wrong), which is the
// precision-conservative default: a broken refuter must not be able to silently
// kill a candidate. Context cancellation propagates.
func (f *Funnel) runRefuters(ctx context.Context, verifier llm.Client, tools []agent.Tool, c Candidate, n int) ([]refutation, error) {
	runner := agent.NewRunner(verifier, tools, verifierSystemBase,
		agent.WithLimits(f.opts.VerifierLimits),
		f.transcriptOption(),
	)

	verdicts := make([]refutation, 0, n)
	for i := 0; i < n; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var v refutation
		_, err := runner.RunJSON(ctx, verifierTask(c), refutationSchema, &v)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			// Unparseable verdict => could not refute.
			v = refutation{Refuted: false, Reasoning: "refuter produced no parseable verdict", Confidence: "low"}
		}
		verdicts = append(verdicts, v)
	}
	return verdicts, nil
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
