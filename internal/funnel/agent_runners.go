package funnel

import (
	"sync"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/domain"
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

// activitySinkFor returns a WithActivitySink option that routes each tool call's
// structured activity to the funnel's progress sink as a KindToolCall event for
// (role, label), via the shared progress.AgentScope seam. A nil progress sink
// makes emission a no-op (progress.Emit handles nil sinks).
func (f *Funnel) activitySinkFor(role, label string) agent.Option {
	scope := progress.NewAgentScope(f.opts.Progress, role, label)
	return agent.WithActivitySink(func(act agent.ToolActivity) {
		scope.EmitToolCall(act.Phase, act.Tool, act.File, act.Line, act.EndLine, act.Symbol, act.Pattern, act.Count, act.Err)
	})
}

// maybeStatusNoteTool returns a status_note Tool when f.opts.Features.StatusNotes is
// true, or nil when the flag is off. Callers append the non-nil result to
// their tool slice before building the runner; when nil, the tool is absent
// and the tool set is byte-identical to the pre-feature state. The tool's notes
// flow through the same AgentScope EmitToolCall seam as automatic tool-call
// events, so manual and derived activity render identically.
func (f *Funnel) maybeStatusNoteTool(role, label string) agent.Tool {
	if !f.opts.Features.StatusNotes {
		return nil
	}
	scope := progress.NewAgentScope(f.opts.Progress, role, label)
	return agent.NewStatusNoteTool(func(act agent.ToolActivity) {
		scope.EmitToolCall(act.Phase, act.Tool, act.File, act.Line, act.EndLine, act.Symbol, act.Pattern, act.Count, act.Err)
	})
}

// toolIssueMu guards Result.Stats.ToolIssues, which the finder and verify
// stages append to concurrently via recordToolIssue.
var toolIssueMu sync.Mutex

// recordToolIssue folds one harness tool-health problem into result.Stats and
// emits a KindToolUnhealthy progress event. It is the single chokepoint for
// both the objective sink (source "infra") and the subjective report_tool_issue
// tool (source "agent"), so both render identically in status.json and the scan
// summary. Entries are deduplicated by (source, tool, severity) with a count.
// Safe for concurrent use by parallel stage goroutines.
func (f *Funnel) recordToolIssue(result *Result, source, tool, severity, reason, role, label string) {
	toolIssueMu.Lock()
	merged := false
	for i := range result.Stats.ToolIssues {
		if ti := &result.Stats.ToolIssues[i]; ti.Source == source && ti.Tool == tool && ti.Severity == severity {
			ti.Count++
			merged = true
			break
		}
	}
	if !merged {
		result.Stats.ToolIssues = append(result.Stats.ToolIssues, ToolIssue{
			Source: source, Tool: tool, Severity: severity, Count: 1,
		})
	}
	toolIssueMu.Unlock()
	progress.Emit(f.opts.Progress, progress.Event{
		Kind: progress.KindToolUnhealthy, Role: role, Label: label,
		Tool: tool, Severity: severity, Message: reason,
	})
}

// toolHealthSinkFor returns a WithToolHealthSink option that routes a tool's
// objective infra failure (a *agent.ToolHealthError surfaced at the runner
// dispatch seam) to recordToolIssue as source "infra". Wired at every funnel
// runner site beside activitySinkFor; today sandbox_exec (refuter-side) is the
// sole producer, with codenav the natural finder-side producer next.
func (f *Funnel) toolHealthSinkFor(result *Result, role, label string) agent.Option {
	return agent.WithToolHealthSink(func(tool string, he *agent.ToolHealthError) {
		f.recordToolIssue(result, "infra", tool, string(he.Severity), he.Reason, role, label)
	})
}

// maybeReportToolIssueTool returns a report_tool_issue Tool when
// f.opts.Features.ToolComplaints is true, or nil when the flag is off. The
// agent-filed complaint is recorded as source "agent" through the same
// recordToolIssue chokepoint as the objective sink, so manual and detected
// issues surface identically.
func (f *Funnel) maybeReportToolIssueTool(result *Result, role, label string) agent.Tool {
	if !f.opts.Features.ToolComplaints {
		return nil
	}
	return agent.NewReportToolIssueTool(func(tool string, sev domain.Severity, summary string) error {
		f.recordToolIssue(result, "agent", tool, string(sev), summary, role, label)
		return nil
	})
}
