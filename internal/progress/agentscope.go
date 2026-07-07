package progress

import "time"

// AgentScope is the shared, opt-in observability seam for a single agent run.
// It brackets the run with KindAgentStarted / KindAgentFinished and routes
// per-call structured activity — derived from the runner's tool calls (via
// agent.WithActivitySink) and from the optional status_note tool — to the sink
// as KindToolCall events. Every pipeline stage that drives an agent.Runner
// (finder, verifier, cartographer, reproducer, patch-prover, severity sweep)
// wires the SAME plumbing through this type, so each surfaces identically in
// `bugbot status` and the live pane.
//
// AgentScope is the single bridge between the agent layer (which speaks only in
// plain func(ToolActivity) callbacks and knows nothing of progress) and the
// progress event stream (which knows nothing of agents). Keeping the bridge
// here means the role/label → event mapping is defined once, not re-derived
// per stage.
//
// Construction is side-effect-free; Start emits the started event. A nil sink
// is safe — Emit no-ops on nil — so an unobserved run pays nothing. AgentScope
// is immutable after construction and the sink fanout is concurrency-safe, so a
// scope may be shared across the goroutines of one agent run and parallel
// agents may hold independent scopes.
type AgentScope struct {
	sink  EventSink
	role  string
	label string
}

// NewAgentScope binds a scope to (role, label) on sink WITHOUT emitting
// anything. Call Start to emit the agent-started bracket; call EmitToolCall on
// its own when the started/finished bracket is emitted elsewhere (e.g. a runner
// option built before the agent's own start/finish lifecycle is known).
func NewAgentScope(sink EventSink, role, label string) AgentScope {
	return AgentScope{sink: sink, role: role, label: label}
}

// Start emits KindAgentStarted and returns the scope for chaining, so callers
// can write `scope := progress.NewAgentScope(sink, role, label).Start()`. Pair
// every Start with a Finish (typically deferred).
func (s AgentScope) Start() AgentScope {
	Emit(s.sink, Event{Kind: KindAgentStarted, Role: s.role, Label: s.label})
	return s
}

// EmitToolCall emits a KindToolCall event for this agent. The flat fields map
// directly from agent.ToolActivity (the funnel builds the func(ToolActivity)
// bridge so progress need not import the agent package). An empty tool name is
// dropped: a KindToolCall event requires Tool to be non-empty.
func (s AgentScope) EmitToolCall(phase, tool, file string, line, endLine int, symbol, pattern string, count int, errStr string) {
	if tool == "" {
		return
	}
	Emit(s.sink, Event{
		Kind:    KindToolCall,
		Role:    s.role,
		Label:   s.label,
		Phase:   phase,
		Tool:    tool,
		File:    file,
		Line:    line,
		EndLine: endLine,
		Symbol:  symbol,
		Pattern: pattern,
		Count:   count,
		Err:     errStr,
	})
}

// Finish emits KindAgentFinished with the run's cumulative token usage,
// wall-clock duration, and error (left empty on success). tokens is the sum of
// input and output tokens; callers derive it from agent.Outcome.Usage.
func (s AgentScope) Finish(tokens int64, dur time.Duration, err error) {
	ev := Event{Kind: KindAgentFinished, Role: s.role, Label: s.label, Tokens: tokens, Duration: dur}
	if err != nil {
		ev.Err = err.Error()
	}
	Emit(s.sink, ev)
}
