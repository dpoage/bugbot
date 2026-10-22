package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	llmkit "github.com/dpoage/llmkit"
	"github.com/dpoage/llmkit/agent"
)

// recordRun drives task through a real agent.Runner scripted with resps and
// returns the run's transcript round-tripped through JSONL — the same write
// and load path the documented recording workflow uses.
func recordRun(task string, tools []agent.Tool, resps ...llmkit.Response) *agent.Transcript {
	r := agent.NewRunner(agent.NewReplayClientFromResponses(resps, llmkit.Capabilities{}), tools, "sys")
	out, err := r.Run(context.Background(), task)
	if err != nil {
		panic(err)
	}
	var buf bytes.Buffer
	if err := out.Transcript.SaveJSONL(&buf); err != nil {
		panic(err)
	}
	loaded, err := agent.LoadJSONL(&buf)
	if err != nil {
		panic(err)
	}
	return loaded
}

// recordOneShot records a single-completion run: the task, then one reply
// carrying finalText with no tool calls — the shape a RunJSON-driven
// finder/verifier produces.
func recordOneShot(task, finalText string) *agent.Transcript {
	return recordRun(task, nil, llmkit.Response{
		Text: finalText, StopReason: llmkit.StopEndTurn,
		Usage: llmkit.Usage{InputTokens: 100, OutputTokens: 50},
	})
}

type staticTool struct{ name, result string }

func (s staticTool) Def() llmkit.ToolDef {
	return llmkit.ToolDef{Name: s.name, Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (s staticTool) Run(context.Context, json.RawMessage) (string, error) { return s.result, nil }

func TestReplayRoleClient_OrderedSessions(t *testing.T) {
	ctx := context.Background()
	store := NewRoleTranscriptStore("finder", llmkit.Capabilities{},
		recordOneShot("audit a.go", `{"candidates":[]}`),
		recordOneShot("audit b.go", `{"candidates":[]}`),
	)
	c := newReplayRoleClient(store)

	// First agent run: a fresh conversation (run-start) advances to session 1.
	r1, err := c.Complete(ctx, llmkit.Request{Messages: []llmkit.Message{llmkit.TextMessage(llmkit.RoleUser, "audit a.go")}})
	if err != nil {
		t.Fatalf("session 1 completion: %v", err)
	}
	if !strings.Contains(r1.Text, "candidates") {
		t.Errorf("unexpected session 1 text: %q", r1.Text)
	}
	if r1.Usage.InputTokens != 100 {
		t.Errorf("usage not carried from recording: %+v", r1.Usage)
	}

	// Second agent run: another fresh conversation advances to session 2.
	r2, err := c.Complete(ctx, llmkit.Request{Messages: []llmkit.Message{llmkit.TextMessage(llmkit.RoleUser, "audit b.go")}})
	if err != nil {
		t.Fatalf("session 2 completion: %v", err)
	}
	if r2.Text == "" {
		t.Errorf("session 2 returned empty text")
	}
}

func TestReplayRoleClient_ExhaustionErrors(t *testing.T) {
	ctx := context.Background()
	store := NewRoleTranscriptStore("verifier", llmkit.Capabilities{},
		recordOneShot("only one", `{"refuted":false,"reasoning":"x","confidence":"high"}`),
	)
	c := newReplayRoleClient(store)

	if _, err := c.Complete(ctx, llmkit.Request{Messages: []llmkit.Message{llmkit.TextMessage(llmkit.RoleUser, "first")}}); err != nil {
		t.Fatalf("first completion: %v", err)
	}
	// A second fresh run with no recording left must error loudly, not return
	// empty output.
	_, err := c.Complete(ctx, llmkit.Request{Messages: []llmkit.Message{llmkit.TextMessage(llmkit.RoleUser, "second")}})
	if err == nil {
		t.Fatalf("expected exhaustion error on extra agent run")
	}
	if !strings.Contains(err.Error(), "exhausted") {
		t.Errorf("error should mention exhaustion: %v", err)
	}
}

// TestReplayRoleClient_DivergenceWithinSession builds a session whose recorded
// completion expects a preceding tool-result round-trip; replaying it without
// that round-trip must surface a structural divergence error.
func TestReplayRoleClient_DivergenceWithinSession(t *testing.T) {
	ctx := context.Background()

	// A two-turn session: the model first asks for a tool, then (after the tool
	// result) gives its final answer. The second assistant response is recorded
	// as expecting one preceding tool result with ID "call-1".
	tr := recordRun("task", []agent.Tool{staticTool{name: "read_file", result: "src"}},
		llmkit.Response{StopReason: llmkit.StopToolUse, ToolCalls: []llmkit.ToolCall{{ID: "call-1", Name: "read_file", Arguments: json.RawMessage(`{}`)}}},
		llmkit.Response{Text: `{"candidates":[]}`, StopReason: llmkit.StopEndTurn},
	)
	store := NewRoleTranscriptStore("finder", llmkit.Capabilities{}, tr)
	c := newReplayRoleClient(store)

	// First completion (run start, no tool results yet): serves the tool-use turn.
	r1, err := c.Complete(ctx, llmkit.Request{Messages: []llmkit.Message{llmkit.TextMessage(llmkit.RoleUser, "task")}})
	if err != nil {
		t.Fatalf("first turn: %v", err)
	}
	if len(r1.ToolCalls) != 1 {
		t.Fatalf("expected a tool call on turn 1, got %+v", r1)
	}

	// Second completion WITHOUT the recorded tool-result in the conversation:
	// structure diverges, so the replayer errors.
	_, err = c.Complete(ctx, llmkit.Request{Messages: []llmkit.Message{
		llmkit.TextMessage(llmkit.RoleUser, "task"),
		{Role: llmkit.RoleAssistant, ToolCalls: []llmkit.ToolCall{{ID: "call-1", Name: "read_file"}}},
		// NOTE: the expected tool-result message is omitted on purpose.
	}})
	if err == nil {
		t.Fatalf("expected divergence error when tool round-trip is missing")
	}
	if !strings.Contains(err.Error(), "diverged") {
		t.Errorf("error should describe divergence: %v", err)
	}
}

func TestIsRunStart(t *testing.T) {
	if !isRunStart(llmkit.Request{Messages: []llmkit.Message{llmkit.TextMessage(llmkit.RoleUser, "x")}}) {
		t.Errorf("a user-only conversation is a run start")
	}
	if isRunStart(llmkit.Request{Messages: []llmkit.Message{
		llmkit.TextMessage(llmkit.RoleUser, "x"),
		llmkit.TextMessage(llmkit.RoleAssistant, "y"),
	}}) {
		t.Errorf("a conversation with an assistant turn is not a run start")
	}
	if isRunStart(llmkit.Request{Messages: []llmkit.Message{
		llmkit.TextMessage(llmkit.RoleUser, "x"),
		{Role: llmkit.RoleToolResult, ToolCallID: "c1", Content: []llmkit.Block{{Kind: llmkit.BlockText, Text: "r"}}},
	}}) {
		t.Errorf("a conversation with a tool result is not a run start")
	}
}
