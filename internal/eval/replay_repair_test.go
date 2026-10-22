package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	llmkit "github.com/dpoage/llmkit"
	"github.com/dpoage/llmkit/agent"
)

// TestReplayRoleClient_RepairStaysInSession: a RunJSON repair completion rides
// its run's RunID and opens a fresh single-turn history, so replay must serve
// it from the same recorded session instead of rolling over to the next one.
func TestReplayRoleClient_RepairStaysInSession(t *testing.T) {
	ctx := context.Background()
	schema := json.RawMessage(`{"type":"object","properties":{"candidates":{"type":"array"}},"required":["candidates"]}`)
	var out struct {
		Candidates []any `json:"candidates"`
	}

	rec := agent.NewRunner(agent.NewReplayClientFromResponses([]llmkit.Response{
		{Text: "I could not decide.", StopReason: llmkit.StopEndTurn},
		{Text: `{"candidates":[]}`, StopReason: llmkit.StopEndTurn},
	}, llmkit.Capabilities{}), nil, "sys")
	recorded, err := rec.RunJSON(ctx, "audit a.go", schema, &out)
	if err != nil {
		t.Fatalf("record RunJSON: %v", err)
	}
	var buf bytes.Buffer
	if err := recorded.Transcript.SaveJSONL(&buf); err != nil {
		t.Fatal(err)
	}
	session, err := agent.LoadJSONL(&buf)
	if err != nil {
		t.Fatal(err)
	}

	c := newReplayRoleClient(NewRoleTranscriptStore("finder", llmkit.Capabilities{}, session))
	replayed, err := agent.NewRunner(c, nil, "sys").RunJSON(ctx, "audit a.go", schema, &out)
	if err != nil {
		t.Fatalf("replay RunJSON: %v", err)
	}
	if replayed.Iterations != recorded.Iterations {
		t.Errorf("replayed %d completions, recorded %d", replayed.Iterations, recorded.Iterations)
	}
	if c.nextIdx != 1 {
		t.Errorf("replay consumed %d sessions, want 1", c.nextIdx)
	}
	if err := c.active.Err(); err != nil {
		t.Errorf("replay diverged: %v", err)
	}
}
