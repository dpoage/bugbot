package repro

import (
	"context"
	"sync"

	"github.com/dpoage/bugbot/internal/llm"
)

// scriptedClient is a sequential fake llm.Client for repro tests. Each call to
// Complete returns the next queued plan body (as the assistant's final text,
// with no tool calls), which is exactly what RunJSON consumes: one completion,
// no tool calls, parse FinalText. Every request is recorded so tests can assert
// that revision feedback reached the model.
//
// Tests that need to verify the agent-layer gate for native schema-constrained
// output set caps.StructuredOutput=true before passing the client to the
// reproducer/patch-prover; otherwise the zero value is the historical default
// (no caps, all behavior unchanged).
type scriptedClient struct {
	mu     sync.Mutex
	bodies []string
	idx    int
	// caps is the static capability profile this client reports. The zero
	// value (StructuredOutput=false) is the historical default; tests that
	// need to verify the agent-layer gate for native schema-constrained
	// output set this field directly before passing the client to the
	// reproducer/patch-prover. The recording of requests is unchanged.
	caps llm.Capabilities
	// requests records every llm.Request Complete sees, in arrival order.
	// Tests inspect it via allRequests to assert that revision feedback or
	// wire-level fields (ResponseSchema, Tools) reached the model.
	requests []llm.Request
}

func newScriptedClient(bodies ...string) *scriptedClient {
	return &scriptedClient{bodies: bodies}
}

func (c *scriptedClient) Capabilities() llm.Capabilities { return c.caps }

func (c *scriptedClient) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	if err := ctx.Err(); err != nil {
		return llm.Response{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = append(c.requests, req)

	body := "{}"
	if c.idx < len(c.bodies) {
		body = c.bodies[c.idx]
	}
	c.idx++
	return llm.Response{
		Text:       body,
		StopReason: llm.StopEndTurn,
		Usage:      llm.Usage{InputTokens: 10, OutputTokens: 5},
	}, nil
}

// allRequests returns a copy of the recorded requests.
func (c *scriptedClient) allRequests() []llm.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]llm.Request, len(c.requests))
	copy(out, c.requests)
	return out
}

// taskText returns the LAST user-message content of request n (the task
// prompt this round's own turn carried), or "" if absent. Round 1 has
// exactly one user message, so this matches the historical "first" behavior
// there; a continuation round (bugbot-z6ay, see planFor/RunJSONContinue)
// carries the ENTIRE prior conversation as a prefix, so the round's own new
// task is the last user turn, not the first.
func (c *scriptedClient) taskText(n int) string {
	reqs := c.allRequests()
	if n < 0 || n >= len(reqs) {
		return ""
	}
	text := ""
	for _, m := range reqs[n].Messages {
		if m.Role == llm.RoleUser {
			text = m.Content
		}
	}
	return text
}

var _ llm.Client = (*scriptedClient)(nil)

// toolScriptStep is one programmed turn of a toolScriptedClient: either a
// tool-use response (built via toolCallStep) or a plain end-turn text
// response (built via textStep).
type toolScriptStep struct {
	resp llm.Response
}

// toolCallStep builds a tool-use step requesting a single call to name with
// the given raw JSON args string.
func toolCallStep(id, name, args string) toolScriptStep {
	return toolScriptStep{resp: llm.Response{
		StopReason: llm.StopToolUse,
		ToolCalls:  []llm.ToolCall{{ID: id, Name: name, Arguments: []byte(args)}},
		Usage:      llm.Usage{InputTokens: 10, OutputTokens: 5},
	}}
}

// textStep builds an end-turn text response, e.g. the final JSON plan a
// RunJSON caller parses once the model stops requesting tools.
func textStep(text string) toolScriptStep {
	return toolScriptStep{resp: llm.Response{
		Text:       text,
		StopReason: llm.StopEndTurn,
		Usage:      llm.Usage{InputTokens: 10, OutputTokens: 5},
	}}
}

// toolScriptedClient is a sequential fake llm.Client that can request tool
// calls, unlike scriptedClient (which only ever returns final text). It
// exists to exercise the reproducer's tool loop (write_repro_file/workspace)
// end-to-end: each
// call to Complete returns the next scripted step in order; once exhausted it
// returns a benign end-turn so an over-running test fails on assertions
// rather than panicking on an out-of-range index.
type toolScriptedClient struct {
	mu       sync.Mutex
	steps    []toolScriptStep
	idx      int
	requests []llm.Request
}

func newToolScriptedClient(steps ...toolScriptStep) *toolScriptedClient {
	return &toolScriptedClient{steps: steps}
}

func (c *toolScriptedClient) Capabilities() llm.Capabilities { return llm.Capabilities{} }

func (c *toolScriptedClient) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	if err := ctx.Err(); err != nil {
		return llm.Response{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = append(c.requests, req)
	if c.idx >= len(c.steps) {
		return llm.Response{Text: "(unscripted)", StopReason: llm.StopEndTurn}, nil
	}
	step := c.steps[c.idx]
	c.idx++
	return step.resp, nil
}

var _ llm.Client = (*toolScriptedClient)(nil)
