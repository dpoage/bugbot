package agent

import (
	"bytes"
	"context"
	"testing"

	"github.com/dpoage/bugbot/internal/llm"
)

func TestTranscript_RoundTrip(t *testing.T) {
	fc := newFakeClient(
		toolResp("c1", "echo", `{"v":"hi"}`, 10, 4),
		textResp("answer", 8, 3),
	)
	r := NewRunner(fc, []Tool{echoTool{name: "echo"}}, "sys")
	out, err := r.Run(context.Background(), "task")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var buf bytes.Buffer
	if err := out.Transcript.SaveJSONL(&buf); err != nil {
		t.Fatalf("SaveJSONL: %v", err)
	}

	loaded, err := LoadJSONL(&buf)
	if err != nil {
		t.Fatalf("LoadJSONL: %v", err)
	}
	if len(loaded.Events) != len(out.Transcript.Events) {
		t.Fatalf("event count: loaded %d, original %d", len(loaded.Events), len(out.Transcript.Events))
	}
	// Spot-check kinds and key fields survived.
	var sawToolResult, sawAssistant bool
	for _, ev := range loaded.Events {
		switch ev.Kind {
		case EventToolResult:
			sawToolResult = true
			if ev.ToolName != "echo" || ev.ToolCallID != "c1" {
				t.Errorf("tool result event lost fields: %+v", ev)
			}
		case EventAssistant:
			sawAssistant = true
		}
	}
	if !sawToolResult || !sawAssistant {
		t.Error("round-trip lost event kinds")
	}
}

func TestReplayClient_ReproducesRun(t *testing.T) {
	// Record a run.
	fc := newFakeClient(
		toolResp("c1", "echo", `{"v":"hi"}`, 10, 4),
		textResp("recorded answer", 8, 3),
	)
	r := NewRunner(fc, []Tool{echoTool{name: "echo"}}, "sys")
	rec, err := r.Run(context.Background(), "task")
	if err != nil {
		t.Fatalf("record Run: %v", err)
	}

	// Replay it through a fresh runner with the same tools.
	replay, err := NewReplayClient(rec.Transcript, llm.Capabilities{})
	if err != nil {
		t.Fatalf("NewReplayClient: %v", err)
	}
	r2 := NewRunner(replay, []Tool{echoTool{name: "echo"}}, "sys")
	out, err := r2.Run(context.Background(), "task")
	if err != nil {
		t.Fatalf("replay Run: %v", err)
	}
	if out.FinalText != "recorded answer" {
		t.Errorf("replay FinalText = %q", out.FinalText)
	}
	if out.Iterations != 2 {
		t.Errorf("replay Iterations = %d, want 2", out.Iterations)
	}
}

func TestReplayClient_Divergence(t *testing.T) {
	// Record a run that calls a tool then finishes.
	fc := newFakeClient(
		toolResp("c1", "echo", `{"v":"hi"}`, 1, 1),
		textResp("done", 1, 1),
	)
	r := NewRunner(fc, []Tool{echoTool{name: "echo"}}, "sys")
	rec, _ := r.Run(context.Background(), "task")

	replay, err := NewReplayClient(rec.Transcript, llm.Capabilities{})
	if err != nil {
		t.Fatalf("NewReplayClient: %v", err)
	}
	// Replay against a runner with NO tools: the recorded tool call will produce
	// an "unknown tool" error result, but the tool-result *ID* still matches, so
	// the structure check passes — this is the lenient design. To force a real
	// divergence we exhaust the responses by looping more than recorded.
	r2 := NewRunner(replay, []Tool{echoTool{name: "echo"}}, "sys")
	if _, err := r2.Run(context.Background(), "task"); err != nil {
		t.Fatalf("first replay should succeed: %v", err)
	}
	// A second run reuses the same exhausted client -> divergence/exhaustion.
	_, err = r2.Run(context.Background(), "task")
	if err == nil {
		t.Fatal("expected exhaustion error on replay reuse")
	}
}

func TestReplayClient_FromResponses(t *testing.T) {
	replay := NewReplayClientFromResponses([]llm.Response{
		{Text: "one", StopReason: llm.StopEndTurn},
	}, llm.Capabilities{ContextWindow: 1234})
	if replay.Capabilities().ContextWindow != 1234 {
		t.Error("capabilities not threaded through")
	}
	resp, err := replay.Complete(context.Background(), llm.Request{})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "one" {
		t.Errorf("Text = %q", resp.Text)
	}
	if _, err := replay.Complete(context.Background(), llm.Request{}); err == nil {
		t.Error("expected exhaustion on second call")
	}
}
