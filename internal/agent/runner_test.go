package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/llm"
)

// echoTool returns whatever it's given, or errors when asked to.
type echoTool struct {
	name    string
	failMsg string // non-empty => Run returns this as an error
}

func (e echoTool) Def() llm.ToolDef {
	return llm.ToolDef{
		Name:        e.name,
		Description: "echo",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"v":{"type":"string"}}}`),
	}
}

func (e echoTool) Run(ctx context.Context, args json.RawMessage) (string, error) {
	if e.failMsg != "" {
		return "", errors.New(e.failMsg)
	}
	return "echo:" + string(args), nil
}

func TestRun_CleanFinish(t *testing.T) {
	fc := newFakeClient(textResp("done", 10, 5))
	r := NewRunner(fc, nil, "sys")

	out, err := r.Run(context.Background(), "task")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Truncated {
		t.Errorf("expected not truncated, got reason %q", out.TruncationReason)
	}
	if out.FinalText != "done" {
		t.Errorf("FinalText = %q, want done", out.FinalText)
	}
	if out.Iterations != 1 {
		t.Errorf("Iterations = %d, want 1", out.Iterations)
	}
	if out.Usage.InputTokens != 10 || out.Usage.OutputTokens != 5 {
		t.Errorf("Usage = %+v, want {10 5}", out.Usage)
	}
	// Transcript should have a request + assistant event.
	if got := len(out.Transcript.Events); got != 2 {
		t.Errorf("transcript events = %d, want 2", got)
	}
}

func TestRun_ToolCallThenFinish(t *testing.T) {
	fc := newFakeClient(
		toolResp("c1", "echo", `{"v":"hi"}`, 10, 4),
		textResp("answer", 8, 3),
	)
	r := NewRunner(fc, []Tool{echoTool{name: "echo"}}, "sys")

	out, err := r.Run(context.Background(), "task")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.FinalText != "answer" {
		t.Errorf("FinalText = %q", out.FinalText)
	}
	if out.Iterations != 2 {
		t.Errorf("Iterations = %d, want 2", out.Iterations)
	}
	// Cumulative usage across both turns.
	if out.Usage.InputTokens != 18 || out.Usage.OutputTokens != 7 {
		t.Errorf("Usage = %+v, want {18 7}", out.Usage)
	}
	// Verify the second request carried the tool result back to the model.
	if len(fc.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(fc.requests))
	}
	last := fc.requests[1].Messages
	foundResult := false
	for _, m := range last {
		if m.Role == llm.RoleToolResult && m.ToolCallID == "c1" {
			foundResult = true
			if !strings.Contains(m.Content, "echo:") {
				t.Errorf("tool result content = %q", m.Content)
			}
		}
	}
	if !foundResult {
		t.Error("second request did not carry the tool result")
	}
}

func TestRun_MaxIterations(t *testing.T) {
	// Always request a tool, never finish -> must hit the iteration cap.
	steps := make([]scriptStep, 0, 10)
	for i := 0; i < 10; i++ {
		steps = append(steps, toolResp("c", "echo", `{"v":"x"}`, 1, 1))
	}
	fc := newFakeClient(steps...)
	r := NewRunner(fc, []Tool{echoTool{name: "echo"}}, "sys", WithLimits(Limits{MaxIterations: 3}))

	out, err := r.Run(context.Background(), "task")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.Truncated || out.TruncationReason != TruncMaxIterations {
		t.Errorf("expected max_iterations truncation, got truncated=%v reason=%q", out.Truncated, out.TruncationReason)
	}
	if out.Iterations != 3 {
		t.Errorf("Iterations = %d, want 3", out.Iterations)
	}
}

func TestRun_TokenBudget(t *testing.T) {
	// Tiny budget: first turn already exceeds it, loop should stop cleanly.
	fc := newFakeClient(
		toolResp("c1", "echo", `{"v":"x"}`, 50, 50), // 100 tokens > budget 10
		textResp("never reached", 1, 1),
	)
	r := NewRunner(fc, []Tool{echoTool{name: "echo"}}, "sys", WithLimits(Limits{TokenBudget: 10}))

	out, err := r.Run(context.Background(), "task")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.Truncated || out.TruncationReason != TruncTokenBudget {
		t.Errorf("expected token_budget truncation, got truncated=%v reason=%q", out.Truncated, out.TruncationReason)
	}
	// Only the first completion should have run.
	if fc.callCount() != 1 {
		t.Errorf("calls = %d, want 1 (budget should stop before second call)", fc.callCount())
	}
}

// TestRun_MaxTokens pins the documented behavior that plain Run (not just
// RunJSON) makes one continuation completion when a turn stops at the output
// token cap and stitches the two halves into the final answer. This is a public
// API contract: changing it alters Run's iteration/token cost, so it must not
// drift silently.
func TestRun_MaxTokens(t *testing.T) {
	fc := newFakeClient(
		maxTokensResp("The answer is fort-", 5, 5), // cut off mid-word at the cap
		textResp("two.", 5, 5),                     // continuation finishes it
	)
	r := NewRunner(fc, nil, "sys")

	out, err := r.Run(context.Background(), "task")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.FinalText != "The answer is fort-two." {
		t.Errorf("FinalText = %q, want the two halves stitched", out.FinalText)
	}
	// Both the initial truncated completion and its continuation ran this turn.
	if fc.callCount() != 2 {
		t.Errorf("calls = %d, want 2 (initial + continuation)", fc.callCount())
	}
	if out.Iterations != 2 {
		t.Errorf("Iterations = %d, want 2 (continuation counts)", out.Iterations)
	}
	// A completed continuation is a clean finish, not a truncation.
	if out.Truncated {
		t.Errorf("Truncated = true, want false after a successful continuation")
	}
}

func TestRun_ToolErrorFedBackToModel(t *testing.T) {
	fc := newFakeClient(
		toolResp("c1", "boom", `{}`, 1, 1),
		textResp("recovered", 1, 1),
	)
	r := NewRunner(fc, []Tool{echoTool{name: "boom", failMsg: "disk on fire"}}, "sys")

	out, err := r.Run(context.Background(), "task")
	if err != nil {
		t.Fatalf("Run should not fail on tool error: %v", err)
	}
	if out.FinalText != "recovered" {
		t.Errorf("FinalText = %q, want recovered", out.FinalText)
	}
	// The second request must carry the ERROR-prefixed tool result with IsError.
	last := fc.requests[1].Messages
	var tr *llm.Message
	for i := range last {
		if last[i].Role == llm.RoleToolResult {
			tr = &last[i]
		}
	}
	if tr == nil {
		t.Fatal("no tool result in second request")
	}
	if !tr.IsError {
		t.Error("tool result IsError = false, want true")
	}
	if !strings.HasPrefix(tr.Content, "ERROR:") || !strings.Contains(tr.Content, "disk on fire") {
		t.Errorf("tool result content = %q, want ERROR: ... disk on fire", tr.Content)
	}
}

func TestRun_UnknownToolFedBackToModel(t *testing.T) {
	fc := newFakeClient(
		toolResp("c1", "ghost", `{}`, 1, 1),
		textResp("ok", 1, 1),
	)
	r := NewRunner(fc, []Tool{echoTool{name: "echo"}}, "sys")

	out, err := r.Run(context.Background(), "task")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	last := fc.requests[1].Messages
	found := false
	for _, m := range last {
		if m.Role == llm.RoleToolResult && m.IsError && strings.Contains(m.Content, "unknown tool") {
			found = true
		}
	}
	if !found {
		t.Error("unknown tool not reported back to model as an error result")
	}
	_ = out
}

func TestRun_CompletionErrorAborts(t *testing.T) {
	fc := newFakeClient(scriptStep{err: errors.New("network down")})
	r := NewRunner(fc, nil, "sys")

	out, err := r.Run(context.Background(), "task")
	if err == nil {
		t.Fatal("expected error from failed completion")
	}
	if out.Transcript == nil {
		t.Error("Outcome.Transcript should be non-nil even on error")
	}
}

func TestRun_ContextCancellation(t *testing.T) {
	fc := newFakeClient(textResp("unused", 1, 1))
	r := NewRunner(fc, nil, "sys")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	out, err := r.Run(ctx, "task")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if out == nil || out.Transcript == nil {
		t.Error("expected non-nil outcome with transcript")
	}
}

func TestLimits_Resolve(t *testing.T) {
	got := Limits{}.resolve()
	if got.MaxIterations != DefaultMaxIterations {
		t.Errorf("MaxIterations = %d, want %d", got.MaxIterations, DefaultMaxIterations)
	}
	if got.TokenBudget != DefaultTokenBudget {
		t.Errorf("TokenBudget = %d, want %d", got.TokenBudget, DefaultTokenBudget)
	}
	// Negative means unlimited and must survive resolve unchanged.
	neg := Limits{MaxIterations: -1, TokenBudget: -1}.resolve()
	if neg.MaxIterations != -1 || neg.TokenBudget != -1 {
		t.Errorf("negative limits altered: %+v", neg)
	}
}

// TestRun_PrefixStability verifies the property prompt caching depends on: the
// request the loop sends at iteration N+1 must extend iteration N's request
// without rewriting any of it. Concretely, across consecutive completions the
// System string and the tool definitions must be byte-identical, and iteration
// N's Messages must be a strict prefix (element-wise byte-equal under JSON
// serialization) of iteration N+1's. Any drift here silently turns every
// provider-side cache lookup into a miss.
func TestRun_PrefixStability(t *testing.T) {
	fc := newFakeClient(
		toolResp("c1", "echo", `{"v":"one"}`, 10, 4),
		toolResp("c2", "grep", `{"v":"two"}`, 12, 4),
		textResp("answer", 8, 3),
	)
	tools := []Tool{echoTool{name: "echo"}, echoTool{name: "grep"}, echoTool{name: "read"}}
	r := NewRunner(fc, tools, "stable system prompt")

	if _, err := r.Run(context.Background(), "task"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fc.requests) != 3 {
		t.Fatalf("requests = %d, want 3", len(fc.requests))
	}

	marshal := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(b)
	}

	for i := 1; i < len(fc.requests); i++ {
		prev, cur := fc.requests[i-1], fc.requests[i]
		if cur.System != prev.System {
			t.Errorf("iteration %d: System changed:\n  prev %q\n  cur  %q", i+1, prev.System, cur.System)
		}
		if got, want := marshal(cur.Tools), marshal(prev.Tools); got != want {
			t.Errorf("iteration %d: tool definitions changed (order or content):\n  prev %s\n  cur  %s", i+1, want, got)
		}
		if len(cur.Messages) <= len(prev.Messages) {
			t.Fatalf("iteration %d: messages did not grow (%d -> %d)", i+1, len(prev.Messages), len(cur.Messages))
		}
		// Element-wise: the previous conversation must be an untouched prefix.
		for j := range prev.Messages {
			if got, want := marshal(cur.Messages[j]), marshal(prev.Messages[j]); got != want {
				t.Errorf("iteration %d: message %d rewritten:\n  prev %s\n  cur  %s", i+1, j, want, got)
			}
		}
		// The serialized previous request's message list must be a byte prefix of
		// the current one (the JSON array shares everything but the closing
		// bracket), which is the strongest cheap statement of prefix stability.
		prevJSON, curJSON := marshal(prev.Messages), marshal(cur.Messages)
		if !strings.HasPrefix(curJSON, strings.TrimSuffix(prevJSON, "]")) {
			t.Errorf("iteration %d: serialized messages are not an append-only extension", i+1)
		}
	}
}

// TestRun_AccumulatesCacheUsage verifies cache-read/creation token counts are
// summed across iterations alongside input/output.
func TestRun_AccumulatesCacheUsage(t *testing.T) {
	step1 := toolResp("c1", "echo", `{}`, 100, 5)
	step1.resp.Usage.CacheCreationInputTokens = 80
	step2 := textResp("done", 120, 6)
	step2.resp.Usage.CacheReadInputTokens = 90
	step2.resp.Usage.CacheCreationInputTokens = 10

	fc := newFakeClient(step1, step2)
	r := NewRunner(fc, []Tool{echoTool{name: "echo"}}, "sys")

	out, err := r.Run(context.Background(), "task")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Usage.InputTokens != 220 || out.Usage.OutputTokens != 11 {
		t.Errorf("Usage in/out = %d/%d, want 220/11", out.Usage.InputTokens, out.Usage.OutputTokens)
	}
	if out.Usage.CacheReadInputTokens != 90 {
		t.Errorf("CacheReadInputTokens = %d, want 90", out.Usage.CacheReadInputTokens)
	}
	if out.Usage.CacheCreationInputTokens != 90 {
		t.Errorf("CacheCreationInputTokens = %d, want 90", out.Usage.CacheCreationInputTokens)
	}
}

// TestToolCallActivity_Mapping verifies the tool-name → human-text mapping for
// every branch documented in the spec.
func TestToolCallActivity_Mapping(t *testing.T) {
	tests := []struct {
		name     string
		calls    []llm.ToolCall
		wantSub  string // must appear as substring
		wantNone bool   // empty calls => ""
	}{
		{
			name:     "empty",
			calls:    nil,
			wantNone: true,
		},
		{
			name:    "read_file",
			calls:   []llm.ToolCall{{Name: "read_file", Arguments: []byte(`{"path":"cmd/main.go"}`)}},
			wantSub: "reading cmd/main.go",
		},
		{
			name:    "read_file no path",
			calls:   []llm.ToolCall{{Name: "read_file", Arguments: []byte(`{}`)}},
			wantSub: "reading file",
		},
		{
			name:    "list_dir",
			calls:   []llm.ToolCall{{Name: "list_dir", Arguments: []byte(`{"dir":"internal/agent"}`)}},
			wantSub: "listing internal/agent",
		},
		{
			name:    "list_dir via directory field",
			calls:   []llm.ToolCall{{Name: "list_dir", Arguments: []byte(`{"directory":"src"}`)}},
			wantSub: "listing src",
		},
		{
			name:    "grep",
			calls:   []llm.ToolCall{{Name: "grep", Arguments: []byte(`{"pattern":"TODO"}`)}},
			wantSub: `grepping "TODO"`,
		},
		{
			name:    "find_definition",
			calls:   []llm.ToolCall{{Name: "find_definition", Arguments: []byte(`{"symbol":"Runner"}`)}},
			wantSub: "navigating Runner",
		},
		{
			name:    "find_references",
			calls:   []llm.ToolCall{{Name: "find_references", Arguments: []byte(`{"symbol":"Emit"}`)}},
			wantSub: "navigating Emit",
		},
		{
			name:    "find_implementations",
			calls:   []llm.ToolCall{{Name: "find_implementations", Arguments: []byte(`{"symbol":"Tool"}`)}},
			wantSub: "navigating Tool",
		},
		{
			name:    "read_symbol",
			calls:   []llm.ToolCall{{Name: "read_symbol", Arguments: []byte(`{"symbol":"Sink"}`)}},
			wantSub: "navigating Sink",
		},
		{
			name:    "sandbox_exec",
			calls:   []llm.ToolCall{{Name: "sandbox_exec", Arguments: []byte(`{}`)}},
			wantSub: "running sandbox",
		},
		{
			name:    "post_lead",
			calls:   []llm.ToolCall{{Name: "post_lead", Arguments: []byte(`{}`)}},
			wantSub: "posting lead",
		},
		{
			name:    "unknown tool uses name",
			calls:   []llm.ToolCall{{Name: "some_custom_tool", Arguments: []byte(`{}`)}},
			wantSub: "some_custom_tool",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := toolCallActivity(tc.calls)
			if tc.wantNone {
				if got != "" {
					t.Errorf("toolCallActivity(nil) = %q, want empty", got)
				}
				return
			}
			if !strings.Contains(got, tc.wantSub) {
				t.Errorf("toolCallActivity(%v) = %q, want substring %q", tc.calls[0].Name, got, tc.wantSub)
			}
		})
	}
}

// TestToolCallActivity_Truncation verifies that a very long tool-call argument
// (e.g. a very long path) is truncated to 80 runes.
func TestToolCallActivity_Truncation(t *testing.T) {
	longPath := strings.Repeat("a", 200)
	calls := []llm.ToolCall{{Name: "read_file", Arguments: []byte(`{"path":"` + longPath + `"}`)}}
	got := toolCallActivity(calls)
	runes := []rune(got)
	if len(runes) > 80 {
		t.Errorf("toolCallActivity truncation: got %d runes, want ≤80", len(runes))
	}
}

// TestWithActivitySink_CalledPerTurn verifies that WithActivitySink installs a
// sink that is called once per tool-call turn (not on clean finish turns).
func TestWithActivitySink_CalledPerTurn(t *testing.T) {
	fc := newFakeClient(
		toolResp("c1", "echo", `{"v":"hi"}`, 10, 4),
		toolResp("c2", "echo", `{"v":"bye"}`, 8, 3),
		textResp("done", 5, 2),
	)

	var mu strings.Builder
	var calls []string
	sink := func(activity string) {
		_ = mu // appease the race detector; Builder is not concurrent but sink is called sequentially
		calls = append(calls, activity)
	}
	r := NewRunner(fc, []Tool{echoTool{name: "echo"}}, "sys", WithActivitySink(sink))
	_, err := r.Run(context.Background(), "task")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Two tool-call turns → two sink calls.
	if len(calls) != 2 {
		t.Errorf("sink called %d times, want 2 (one per tool-call turn)", len(calls))
	}
	// Each call should be non-empty (derived from "echo" → default name fallback).
	for i, c := range calls {
		if c == "" {
			t.Errorf("sink call %d was empty", i)
		}
	}
}

// TestWithActivitySink_NilIsNoop verifies that a nil sink or a Runner without
// WithActivitySink runs cleanly with no overhead (no panic, no extra state).
func TestWithActivitySink_NilIsNoop(t *testing.T) {
	fc := newFakeClient(
		toolResp("c1", "echo", `{}`, 5, 2),
		textResp("done", 3, 1),
	)
	r := NewRunner(fc, []Tool{echoTool{name: "echo"}}, "sys")
	_, err := r.Run(context.Background(), "task")
	if err != nil {
		t.Fatalf("Run without sink: %v", err)
	}
}

// healthEchoTool is an echo-style tool whose Run returns a *ToolHealthError
// when configured to. It is the test vehicle for the WithToolHealthSink
// dispatch seam: a health error must reach the sink, a plain error must not.
type healthEchoTool struct {
	name   string
	health *ToolHealthError // non-nil => Run returns this
	plain  string           // non-empty => Run returns errors.New(plain)
}

func (e healthEchoTool) Def() llm.ToolDef {
	return llm.ToolDef{
		Name:        e.name,
		Description: "health-echo",
		Parameters:  json.RawMessage(`{"type":"object"}`),
	}
}

func (e healthEchoTool) Run(ctx context.Context, args json.RawMessage) (string, error) {
	if e.health != nil {
		return "", e.health
	}
	if e.plain != "" {
		return "", errors.New(e.plain)
	}
	return "ok", nil
}

// TestWithToolHealthSink_CalledOnHealthError verifies that a tool returning a
// *ToolHealthError triggers the sink with the tool name and the
// *ToolHealthError pointer (preserving Severity, Reason, Err).
func TestWithToolHealthSink_CalledOnHealthError(t *testing.T) {
	healthErr := &ToolHealthError{
		Severity: domain.SeverityHigh,
		Reason:   "sandbox runtime unavailable",
		Err:      errors.New("podman not found"),
	}
	fc := newFakeClient(
		toolResp("c1", "broken", `{}`, 10, 4),
		textResp("done", 5, 2),
	)

	var sinkTool string
	var sinkErr *ToolHealthError
	sink := func(tool string, he *ToolHealthError) {
		sinkTool = tool
		sinkErr = he
	}
	r := NewRunner(fc, []Tool{healthEchoTool{name: "broken", health: healthErr}}, "sys", WithToolHealthSink(sink))
	_, err := r.Run(context.Background(), "task")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sinkTool != "broken" {
		t.Errorf("sink called with tool %q, want %q", sinkTool, "broken")
	}
	if sinkErr != healthErr {
		t.Errorf("sink called with error %p, want the original %p", sinkErr, healthErr)
	}
	if sinkErr.Severity != domain.SeverityHigh {
		t.Errorf("sink severity = %q, want high", sinkErr.Severity)
	}
	if sinkErr.Reason != "sandbox runtime unavailable" {
		t.Errorf("sink reason = %q", sinkErr.Reason)
	}
}

// TestWithToolHealthSink_NotCalledOnPlainError is the central infra-vs-
// recoverable assertion: an ordinary model-recoverable tool error (e.g. bad
// args, file-not-found) must NOT trigger the health sink. Only *ToolHealthError
// reaches it.
func TestWithToolHealthSink_NotCalledOnPlainError(t *testing.T) {
	fc := newFakeClient(
		toolResp("c1", "plain", `{}`, 10, 4),
		textResp("done", 5, 2),
	)

	called := false
	sink := func(tool string, he *ToolHealthError) { called = true }
	r := NewRunner(fc, []Tool{healthEchoTool{name: "plain", plain: "bad args"}}, "sys", WithToolHealthSink(sink))
	_, err := r.Run(context.Background(), "task")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if called {
		t.Error("sink was called for a plain errors.New — must only fire on *ToolHealthError")
	}
}

// TestWithToolHealthSink_NilIsNoop verifies that a nil sink (or no
// WithToolHealthSink option at all) runs cleanly with no overhead — no
// panic, no extra state. Mirrors TestWithActivitySink_NilIsNoop.
func TestWithToolHealthSink_NilIsNoop(t *testing.T) {
	fc := newFakeClient(
		toolResp("c1", "broken", `{}`, 5, 2),
		textResp("done", 3, 1),
	)
	// No WithToolHealthSink option at all.
	r := NewRunner(fc, []Tool{healthEchoTool{name: "broken", health: &ToolHealthError{
		Severity: domain.SeverityCritical,
		Reason:   "container runtime missing",
	}}}, "sys")
	if _, err := r.Run(context.Background(), "task"); err != nil {
		t.Fatalf("Run without sink: %v", err)
	}

	// Nil sink is also a no-op.
	r2 := NewRunner(fc, []Tool{healthEchoTool{name: "broken", health: &ToolHealthError{
		Severity: domain.SeverityCritical,
		Reason:   "container runtime missing",
	}}}, "sys", WithToolHealthSink(nil))
	if _, err := r2.Run(context.Background(), "task"); err != nil {
		t.Fatalf("Run with nil sink: %v", err)
	}
}

// TestRunTool_ToolHealthSink_SkippedOnCancelledCtx verifies the dispatch seam
// does NOT record a tool-health signal when ctx is already cancelled: a failure
// caused by run teardown/cancellation is not a harness-tooling problem, even
// when the tool returns a *ToolHealthError.
func TestRunTool_ToolHealthSink_SkippedOnCancelledCtx(t *testing.T) {
	called := false
	sink := func(tool string, he *ToolHealthError) { called = true }
	r := NewRunner(newFakeClient(), []Tool{healthEchoTool{name: "broken", health: &ToolHealthError{
		Severity: domain.SeverityHigh, Reason: "sandbox runtime unavailable",
	}}}, "sys", WithToolHealthSink(sink))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, isErr := r.runTool(ctx, llm.ToolCall{Name: "broken", Arguments: json.RawMessage("{}")})
	if !isErr {
		t.Fatal("a ToolHealthError must still be returned as an error result")
	}
	if called {
		t.Error("health sink must NOT fire when ctx is already cancelled")
	}
}

// stopErrorResp builds a response with StopReason == StopError (refusal,
// safety filter, recitation) and no tool calls.
func stopErrorResp(text string, in, out int64) scriptStep {
	return scriptStep{resp: llm.Response{
		Text:       text,
		StopReason: llm.StopError,
		Usage:      llm.Usage{InputTokens: in, OutputTokens: out},
	}}
}

// TestRun_StopErrorYieldsTypedError verifies bugbot-wm2m: a final turn that
// ends with StopError and no tool calls must surface *ErrStopReason, never a
// clean Outcome that records refusal prose as the answer.
func TestRun_StopErrorYieldsTypedError(t *testing.T) {
	fc := newFakeClient(stopErrorResp("I cannot help with that.", 10, 5))
	r := NewRunner(fc, nil, "sys")

	out, err := r.Run(context.Background(), "task")
	var stopErr *ErrStopReason
	if !errors.As(err, &stopErr) {
		t.Fatalf("Run error = %v, want *ErrStopReason", err)
	}
	if stopErr.StopReason != llm.StopError {
		t.Errorf("StopReason = %q, want %q", stopErr.StopReason, llm.StopError)
	}
	if stopErr.Text != "I cannot help with that." {
		t.Errorf("Text = %q, want refusal prose", stopErr.Text)
	}
	if stopErr.Outcome == nil || out == nil {
		t.Fatal("partial Outcome must be attached and returned")
	}
	if stopErr.Outcome.Usage.InputTokens != 10 {
		t.Errorf("partial Outcome usage lost: %+v", stopErr.Outcome.Usage)
	}
}

// TestRun_StopErrorAfterToolsYieldsTypedError covers the multi-turn case: tool
// turns succeed, then the model refuses. The stale text from earlier turns
// must not be presented as a clean answer.
func TestRun_StopErrorAfterToolsYieldsTypedError(t *testing.T) {
	fc := newFakeClient(
		toolResp("c1", "ghost", `{}`, 5, 2),
		stopErrorResp("", 5, 2),
	)
	r := NewRunner(fc, nil, "sys")

	_, err := r.Run(context.Background(), "task")
	var stopErr *ErrStopReason
	if !errors.As(err, &stopErr) {
		t.Fatalf("Run error = %v, want *ErrStopReason", err)
	}
	if stopErr.Outcome.Iterations != 2 {
		t.Errorf("Iterations = %d, want 2", stopErr.Outcome.Iterations)
	}
}

// TestRun_EmptyFinalTurnFinalTextSet verifies that a final turn with no text
// leaves FinalTextSet false even when an earlier turn produced text, so
// callers can tell stale FinalText from a genuine final answer.
func TestRun_EmptyFinalTurnFinalTextSet(t *testing.T) {
	withText := toolResp("c1", "ghost", `{}`, 5, 2)
	withText.resp.Text = "thinking out loud"
	fc := newFakeClient(
		withText,
		scriptStep{resp: llm.Response{StopReason: llm.StopEndTurn, Usage: llm.Usage{InputTokens: 5, OutputTokens: 1}}},
	)
	r := NewRunner(fc, nil, "sys")

	out, err := r.Run(context.Background(), "task")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.FinalTextSet {
		t.Error("FinalTextSet = true, want false: final turn emitted no text")
	}
	if out.FinalText != "thinking out loud" {
		t.Errorf("FinalText = %q, want stale text preserved for transparency", out.FinalText)
	}
}

// TestRun_NonEmptyFinalTurnFinalTextSet is the positive counterpart: a final
// turn that emits text sets FinalTextSet.
func TestRun_NonEmptyFinalTurnFinalTextSet(t *testing.T) {
	fc := newFakeClient(textResp("the answer", 10, 5))
	r := NewRunner(fc, nil, "sys")

	out, err := r.Run(context.Background(), "task")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.FinalTextSet {
		t.Error("FinalTextSet = false, want true")
	}
	if out.FinalText != "the answer" {
		t.Errorf("FinalText = %q, want 'the answer'", out.FinalText)
	}
}
