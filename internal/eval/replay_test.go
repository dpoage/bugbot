package eval

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/llm"
)

// recordOneShot builds a synthetic single-completion transcript: one request
// (the task user-message) and one assistant reply carrying finalText with no
// tool calls. This is exactly the shape a RunJSON-driven finder/verifier
// produces, so it round-trips through the replay role client.
func recordOneShot(task, finalText string) *agent.Transcript {
	// Build via the public JSONL surface so we exercise Save/Load too: this is
	// the same path the documented recording workflow uses (transcripts are
	// persisted as JSONL and reloaded for replay).
	var buf bytes.Buffer
	src := agent.NewTranscript()
	src.Events = []agent.Event{
		{Kind: agent.EventRequest, Step: 1, Messages: []llm.Message{{Role: llm.RoleUser, Content: task}}},
		{Kind: agent.EventAssistant, Step: 1, Text: finalText, StopReason: llm.StopEndTurn,
			Usage: &llm.Usage{InputTokens: 100, OutputTokens: 50}},
	}
	if err := src.SaveJSONL(&buf); err != nil {
		panic(err)
	}
	loaded, err := agent.LoadJSONL(&buf)
	if err != nil {
		panic(err)
	}
	return loaded
}

func TestReplayRoleClient_OrderedSessions(t *testing.T) {
	ctx := context.Background()
	store := NewRoleTranscriptStore("finder", llm.Capabilities{},
		recordOneShot("audit a.go", `{"candidates":[]}`),
		recordOneShot("audit b.go", `{"candidates":[]}`),
	)
	c := newReplayRoleClient(store)

	// First agent run: a fresh conversation (run-start) advances to session 1.
	r1, err := c.Complete(ctx, llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: "audit a.go"}}})
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
	r2, err := c.Complete(ctx, llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: "audit b.go"}}})
	if err != nil {
		t.Fatalf("session 2 completion: %v", err)
	}
	if r2.Text == "" {
		t.Errorf("session 2 returned empty text")
	}
}

func TestReplayRoleClient_ExhaustionErrors(t *testing.T) {
	ctx := context.Background()
	store := NewRoleTranscriptStore("verifier", llm.Capabilities{},
		recordOneShot("only one", `{"refuted":false,"reasoning":"x","confidence":"high"}`),
	)
	c := newReplayRoleClient(store)

	if _, err := c.Complete(ctx, llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: "first"}}}); err != nil {
		t.Fatalf("first completion: %v", err)
	}
	// A second fresh run with no recording left must error loudly, not return
	// empty output.
	_, err := c.Complete(ctx, llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: "second"}}})
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
	tr := agent.NewTranscript()
	tr.Events = []agent.Event{
		{Kind: agent.EventRequest, Step: 1, Messages: []llm.Message{{Role: llm.RoleUser, Content: "task"}}},
		{Kind: agent.EventAssistant, Step: 1, StopReason: llm.StopToolUse,
			ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "read_file"}}},
		{Kind: agent.EventToolResult, Step: 1, ToolCallID: "call-1", ToolName: "read_file", Result: "src"},
		{Kind: agent.EventAssistant, Step: 2, Text: `{"candidates":[]}`, StopReason: llm.StopEndTurn},
	}
	store := NewRoleTranscriptStore("finder", llm.Capabilities{}, tr)
	c := newReplayRoleClient(store)

	// First completion (run start, no tool results yet): serves the tool-use turn.
	r1, err := c.Complete(ctx, llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: "task"}}})
	if err != nil {
		t.Fatalf("first turn: %v", err)
	}
	if len(r1.ToolCalls) != 1 {
		t.Fatalf("expected a tool call on turn 1, got %+v", r1)
	}

	// Second completion WITHOUT the recorded tool-result in the conversation:
	// structure diverges, so the replayer errors.
	_, err = c.Complete(ctx, llm.Request{Messages: []llm.Message{
		{Role: llm.RoleUser, Content: "task"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "read_file"}}},
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
	if !isRunStart(llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: "x"}}}) {
		t.Errorf("a user-only conversation is a run start")
	}
	if isRunStart(llm.Request{Messages: []llm.Message{
		{Role: llm.RoleUser, Content: "x"},
		{Role: llm.RoleAssistant, Content: "y"},
	}}) {
		t.Errorf("a conversation with an assistant turn is not a run start")
	}
	if isRunStart(llm.Request{Messages: []llm.Message{
		{Role: llm.RoleUser, Content: "x"},
		{Role: llm.RoleToolResult, ToolCallID: "c1", Content: "r"},
	}}) {
		t.Errorf("a conversation with a tool result is not a run start")
	}
}
