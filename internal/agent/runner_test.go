package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

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
