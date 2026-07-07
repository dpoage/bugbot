package progress

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

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
//
// id disambiguates concurrent agents that share (role, label) — e.g. two
// reproducer runs on duplicate open finding titles. It is minted once in
// NewAgentScope and stamped onto every event the scope emits (see
// events.go's AgentID doc); consumers key by progress.AgentEventKey, which
// falls back to (role, label) when id is empty (it never is for a scope built
// by NewAgentScope, but hand-built Events from older code paths may omit it).
type AgentScope struct {
	sink  EventSink
	role  string
	label string
	id    string
}

// NewAgentScope binds a scope to (role, label) on sink WITHOUT emitting
// anything. Call Start to emit the agent-started bracket; call EmitToolCall on
// its own when the started/finished bracket is emitted elsewhere (e.g. a runner
// option built before the agent's own start/finish lifecycle is known).
//
// NewAgentScope mints a fresh AgentID for the scope: 8 random bytes,
// hex-encoded. progress deliberately does not import internal/store (which
// has its own, unexported ID generator for agent_units primary keys) to stay
// dependency-light, so this is a small independent generator rather than a
// shared one.
func NewAgentScope(sink EventSink, role, label string) AgentScope {
	return AgentScope{sink: sink, role: role, label: label, id: newAgentID()}
}

// newAgentID returns a 16-character random hex string for use as an
// AgentScope's id, or "" on a crypto/rand failure (a practically-impossible
// but non-zero-probability I/O error). "" is deliberate, not a degenerate
// fallback value: AgentEventKey and every consumer keyed by it treat an
// empty AgentID as "no identity, fall back to (role, label)" — the same path
// pre-identity emitters already take — so a rand failure degrades to the
// historical role+label keying instead of silently colliding every agent in
// the process under one shared all-zeros id. Collisions among successfully
// minted ids are not guarded against beyond the birthday bound of 8 random
// bytes, which is ample for the number of agents any single scan or daemon
// cycle runs concurrently.
func newAgentID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}

// Start emits KindAgentStarted and returns the scope for chaining, so callers
// can write `scope := progress.NewAgentScope(sink, role, label).Start()`. Pair
// every Start with a Finish (typically deferred).
func (s AgentScope) Start() AgentScope {
	Emit(s.sink, Event{Kind: KindAgentStarted, Role: s.role, Label: s.label, AgentID: s.id})
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
		AgentID: s.id,
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
	ev := Event{Kind: KindAgentFinished, Role: s.role, Label: s.label, AgentID: s.id, Tokens: tokens, Duration: dur}
	if err != nil {
		ev.Err = err.Error()
	}
	Emit(s.sink, ev)
}

// EmitEvent emits ev under this agent's identity: Role, Label, and AgentID
// are filled in when the caller left them empty, then the event is forwarded
// to Emit unchanged otherwise. This is for emitters that build their own
// Event kinds (e.g. repro.go's KindReproAttempt rounds) but still want the
// event correctly attributed to the agent run the scope represents — without
// AgentScope needing a dedicated Emit* method for every such Kind.
func (s AgentScope) EmitEvent(ev Event) {
	if ev.Role == "" {
		ev.Role = s.role
	}
	if ev.Label == "" {
		ev.Label = s.label
	}
	if ev.AgentID == "" {
		ev.AgentID = s.id
	}
	Emit(s.sink, ev)
}
