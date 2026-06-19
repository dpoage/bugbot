package funnel

import (
	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/progress"
)

// budgetStopped reports whether outcome was truncated by a budget limit (the
// run's own token budget or the shared cross-runner budget pool), as opposed to
// the iteration cap or no truncation at all. An unparseable result from such a
// run is an expected budget stop, not a reliability failure.
//
// This is the SHARED predicate for both the finder stage (runFinder) and the
// verify stage (runRefuters / runArbiter): a verifier that exhausts its OWN
// per-run token budget must be classified budget-stopped too, not routed to
// the unparseable/failed path. The agent layer distinguishes the two stop
// reasons — TruncTokenBudget for the run's own per-run ceiling and
// TruncBudgetPool for the shared pool — but the funnel treats both the same
// way for reliability accounting (neither is a model or provider fault).
func budgetStopped(o *agent.Outcome) bool {
	if o == nil || !o.Truncated {
		return false
	}
	return o.TruncationReason == agent.TruncTokenBudget ||
		o.TruncationReason == agent.TruncBudgetPool
}

// hasSandboxExec reports whether the supplied tool set contains a tool named
// "sandbox_exec" — the single tool that signals "this verifier has actual
// sandbox execution capability" so the system prompt can mention it. The
// scan loop (no capability) and the per-candidate sandbox tool produced by
// buildSandboxTool are the two producers; the verify path needs to detect
// either without caring about the tool's other fields.
func hasSandboxExec(tools []agent.Tool) bool {
	for _, t := range tools {
		if t.Def().Name == "sandbox_exec" {
			return true
		}
	}
	return false
}

// newAgentRunner is the funnel-side single builder for agent.Runner instances.
// It applies the consistent option set every funnel agent (finder, refuter,
// arbiter) needs:
//
//   - WithLimits: per-run token + iteration budget derived from the shared
//     pool by the caller (budget.finderRunnerLimits / budget.verifyRunnerLimits).
//   - WithMaxTokens: the per-completion output cap, shared by every funnel
//     stage (DefaultMaxOutputTokens).
//   - f.transcriptOption: JSONL transcript auto-save when TranscriptDir is
//     configured, otherwise a no-op (so the call site is uniform).
//
// Centralizing the builder eliminates the four pre-existing near-duplicate
// agent.NewRunner(...) call sites (hypothesize.go:793, verify.go:68, verify.go:116)
// so a future tweak — say a new per-stage hook, or a per-stage option split
// for finding vs verifying — happens in one place.
func (f *Funnel) newAgentRunner(client llm.Client, tools []agent.Tool, systemPrompt string, limits agent.Limits, extra ...agent.Option) *agent.Runner {
	opts := []agent.Option{
		agent.WithLimits(limits),
		agent.WithMaxTokens(DefaultMaxOutputTokens),
		f.transcriptOption(),
	}
	opts = append(opts, extra...)
	return agent.NewRunner(client, tools, systemPrompt, opts...)
}

// activitySinkFor returns a WithActivitySink option that routes each turn's
// tool-call note to the funnel's progress sink as a KindAgentActivity event for
// (role, label), via the shared progress.AgentScope seam. A nil progress sink
// makes emission a no-op (progress.Emit handles nil sinks).
func (f *Funnel) activitySinkFor(role, label string) agent.Option {
	return agent.WithActivitySink(progress.NewAgentScope(f.opts.Progress, role, label).ActivitySink())
}

// maybeStatusNoteTool returns a status_note Tool when f.opts.Features.StatusNotes is
// true, or nil when the flag is off. Callers append the non-nil result to
// their tool slice before building the runner; when nil, the tool is absent
// and the tool set is byte-identical to the pre-feature state. The tool's notes
// flow through the same AgentScope activity sink as automatic tool-call notes,
// so manual and derived activity render identically.
func (f *Funnel) maybeStatusNoteTool(role, label string) agent.Tool {
	if !f.opts.Features.StatusNotes {
		return nil
	}
	return agent.NewStatusNoteTool(progress.NewAgentScope(f.opts.Progress, role, label).ActivitySink())
}
