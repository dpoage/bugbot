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

// taskText returns the first user-message content of request n (the task
// prompt the agent was given), or "" if absent.
func (c *scriptedClient) taskText(n int) string {
	reqs := c.allRequests()
	if n < 0 || n >= len(reqs) {
		return ""
	}
	for _, m := range reqs[n].Messages {
		if m.Role == llm.RoleUser {
			return m.Content
		}
	}
	return ""
}

var _ llm.Client = (*scriptedClient)(nil)
