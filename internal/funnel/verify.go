package funnel

import (
	"context"
	"fmt"
	"strings"

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
// cut short by a budget stop — either the shared cross-runner pool
// (TruncBudgetPool) or this refuter's own per-run token budget
// (TruncTokenBudget): once either is gone, the remaining votes for this
// candidate would be untrustworthy, so the caller treats the candidate as
// budget-orphaned rather than reaching a verdict on a partial vote. Limits are
// derived from the pool at launch so a refuter launched late gets the remaining
// headroom and one in flight stops at its next turn.
//
// Each seat gets a distinct specialty clause appended to its system prompt (see
// seats.go) so the panel attacks the report from different angles. When n==1
// (budget-degraded path, degradedRefuters) no seat clause is added — the single
// generalist produces today's prompt byte-identical.
func (f *Funnel) runRefuters(ctx context.Context, verifier llm.Client, tools []agent.Tool, persona string, c Candidate, n int, budget *budgetState) ([]refutation, []string, int64, int, bool, error) {
	hasSandbox := hasSandboxExec(tools)

	var tokens int64
	var failed int
	verdicts := make([]refutation, 0, n)
	seatNames := make([]string, 0, n)
	for i := 0; i < n; i++ {
		if err := ctx.Err(); err != nil {
			return nil, nil, tokens, failed, false, err
		}
		seat := seatForCandidate(i, n, c, hasSandbox)
		// Build the per-seat system prompt: base + optional sandbox paragraph +
		// optional seat clause. When n==1 the seat is empty (no clause appended)
		// so the single-refuter path is byte-identical to the pre-diversity code.
		sysPrompt := verifierSystemPrompt(persona, hasSandbox)
		if seat.clause != "" {
			sysPrompt += "\n\n" + seat.clause
		}
		runner := f.newAgentRunner(verifier, tools, sysPrompt, budget.verifyRunnerLimits(f.opts.Limits.VerifierLimits),
			f.activitySinkFor(progress.RoleVerifier, c.Title))
		var v refutation
		outcome, err := runner.RunJSON(ctx, verifierTask(c), refutationSchema, &v)
		if outcome != nil {
			tokens += outcome.Usage.InputTokens + outcome.Usage.OutputTokens
			// Any budget stop — shared pool OR this refuter's own per-run
			// allowance — means this refuter never got to complete its
			// challenge; stop voting and signal the caller to orphan the
			// candidate. The shared predicate (agent_runners.go) keeps the
			// finder/verify paths in sync.
			if budgetStopped(outcome) {
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
// and any error. The stopped flag is true when the arbiter was cut by a budget
// stop — the shared cross-runner pool (TruncBudgetPool) or the arbiter's own
// per-run token budget (TruncTokenBudget) — signalling the caller to orphan
// the candidate. An unparseable verdict is returned as a non-nil error with
// stopped=false so the caller can fall back to majorityRefuted.
//
// post_lead is absent from candTools (same rationale as refuters: refuter
// independence is the core false-positive killer; the arbiter must be equally
// independent).
func (f *Funnel) runArbiter(ctx context.Context, verifier llm.Client, candTools []agent.Tool, persona string, c Candidate, verdicts []refutation, seatNames []string, budget *budgetState) (*refutation, int64, bool, error) {
	hasSandbox := hasSandboxExec(candTools)
	runner := f.newAgentRunner(verifier, candTools, arbiterSystemPrompt(persona, hasSandbox), budget.verifyRunnerLimits(f.opts.Limits.VerifierLimits),
		f.activitySinkFor(progress.RoleVerifier, c.Title))
	var av refutation
	outcome, err := runner.RunJSON(ctx, arbiterTask(c, verdicts, seatNames), arbiterSchema, &av)
	var tokens int64
	if outcome != nil {
		tokens = outcome.Usage.InputTokens + outcome.Usage.OutputTokens
		// Any budget stop: signal the caller to orphan the candidate
		// (T3 suspected). The shared predicate matches the finder stage so
		// a verifier that exhausts its own per-run allowance is classified
		// consistently with one stopped by the shared pool.
		if budgetStopped(outcome) {
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

// examinedVerdicts returns the subset of verdicts from seats that actually
// examined the code (CouldNotReadCode==false). An unparseable verdict
// (Reasoning=="refuter produced no parseable verdict") counts as examined
// because the runner tried; abstaining seats (CouldNotReadCode==true) are
// excluded because they never saw the evidence.
func examinedVerdicts(verdicts []refutation) []refutation {
	out := make([]refutation, 0, len(verdicts))
	for _, v := range verdicts {
		if !v.CouldNotReadCode {
			out = append(out, v)
		}
	}
	return out
}

// belowQuorum reports whether too few seats examined the code to reach a
// trustworthy verdict. The quorum floor requires that a strict majority of the
// panel size N actually examined the code (i.e. examinedCount*2 > N). This
// mirrors the majorityRefuted threshold: if we require 2/3 to kill, we also
// require 2/3 to have examined before we trust a survive outcome. When N==0 or
// N==1 the floor is met as long as at least one seat examined (the degraded
// single-refuter path must not be penalized).
func belowQuorum(examined, panelSize int) bool {
	if panelSize <= 1 {
		return examined == 0
	}
	return examined*2 <= panelSize
}

// isSplitVerdict reports whether the EXAMINED verdicts contain at least one
// refuted and at least one not-refuted entry. Abstaining seats
// (CouldNotReadCode==true) are excluded from the count because they contribute
// no evidence. An empty examined slice is not split. A single-examined-seat
// panel can never be split.
func isSplitVerdict(verdicts []refutation) bool {
	examined := examinedVerdicts(verdicts)
	if len(examined) < 2 {
		return false
	}
	hasRefuted := false
	hasNotRefuted := false
	for _, v := range examined {
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

// majorityRefuted reports whether a strict majority of EXAMINED verdicts are
// "refuted". Abstaining seats (CouldNotReadCode==true) are excluded from the
// denominator — inability to access the code is not a vote. A tie or empty
// examined set never refutes (tie-survives is intentional: an unparseable
// arbiter must NEVER silently kill a candidate).
func majorityRefuted(verdicts []refutation) bool {
	examined := examinedVerdicts(verdicts)
	if len(examined) == 0 {
		return false
	}
	refuted := 0
	for _, v := range examined {
		if v.Refuted {
			refuted++
		}
	}
	return refuted*2 > len(examined)
}

// buildReasoning concatenates the refuters' reasoning into a verification trace
// recorded on the finding. It labels each refuter's verdict with its seat name
// so the trace is legible to a human triaging the finding later. Abstaining
// seats are labeled distinctly. When arbiterRan is true, a final line for the
// arbiter's verdict is appended and the header changes to reflect arbitration.
func buildReasoning(verdicts []refutation, seatNames []string, arbiterLine string, arbiterRan bool) string {
	var b strings.Builder
	examined := examinedVerdicts(verdicts)
	refuted := 0
	for _, v := range examined {
		if v.Refuted {
			refuted++
		}
	}
	n := len(examined)
	if arbiterRan {
		fmt.Fprintf(&b, "Survived adversarial verification (split panel decided by arbitration, %d/%d examined refuters could not disprove it):\n", n-refuted, n)
	} else {
		fmt.Fprintf(&b, "Survived adversarial verification (%d/%d examined refuters could not disprove it):\n", n-refuted, n)
	}
	total := len(verdicts)
	for i, v := range verdicts {
		var verdict string
		if v.CouldNotReadCode {
			verdict = "abstained (could not read cited code)"
		} else if v.Refuted {
			verdict = "refuted"
		} else {
			verdict = "could not refute"
		}
		// Label with seat name when the panel has >= 2 seats (total>=2 means
		// seats were assigned). total==1 keeps the old unlabeled format.
		if total >= 2 && i < len(seatNames) && seatNames[i] != "" {
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

// confidenceRank maps a confidence string to a numeric rank (higher = more
// confident) for use in bestRefuterCorrection.
func confidenceRank(c string) int {
	switch c {
	case "high":
		return 2
	case "medium":
		return 1
	default:
		return 0
	}
}

// bestRefuterCorrection returns the CorrectedDescription from the examined
// not-refuted seat with the highest confidence, or fallback when no examined
// not-refuted seat provided a non-empty correction. This folds mechanism
// corrections on unanimous-survive panels without an extra arbiter round-trip.
//
// Only not-refuted seats are considered: a refuter that claimed to have refuted
// the bug (Refuted==true) cannot also supply a correction (the bug was refuted,
// so no correction is warranted).
func bestRefuterCorrection(examined []refutation, fallback string) string {
	best := ""
	bestRank := -1
	for _, v := range examined {
		if v.Refuted || v.CorrectedDescription == "" {
			continue
		}
		r := confidenceRank(v.Confidence)
		if r > bestRank {
			bestRank = r
			best = v.CorrectedDescription
		}
	}
	if best == "" {
		return fallback
	}
	return best
}
