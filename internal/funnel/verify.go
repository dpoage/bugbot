package funnel

import (
	"context"
	"fmt"
	"strings"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/llm"
)

// verified pairs a surviving candidate with the refuters' reasoning that backs
// it, so persistence can record the verification trace on the finding.
type verified struct {
	cand      Candidate
	reasoning string
}

// The helpers below (runRefuters, runArbiter, etc.) are used by
// verify_stream.go's runVerifyAndPersist. The old batch verify() method
// was removed when the streaming topology (run.go → verify_stream.go) replaced it.
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
			agent.WithMaxTokens(f.opts.maxOutputTokens()),
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
		agent.WithMaxTokens(f.opts.maxOutputTokens()),
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
