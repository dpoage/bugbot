package agent

import (
	"context"
	"sync"

	"github.com/dpoage/bugbot/internal/llm"
)

// scriptStep is one programmed turn of a fakeClient: the response to return,
// or an error to surface (an infra failure).
type scriptStep struct {
	resp llm.Response
	err  error
}

// fakeClient is a scripted llm.Client for testing the loop. It returns each
// scripted step in order and records every request it received.
type fakeClient struct {
	mu       sync.Mutex
	steps    []scriptStep
	idx      int
	requests []llm.Request
	caps     llm.Capabilities
}

func newFakeClient(steps ...scriptStep) *fakeClient {
	return &fakeClient{steps: steps}
}

func (f *fakeClient) Capabilities() llm.Capabilities { return f.caps }

func (f *fakeClient) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	if err := ctx.Err(); err != nil {
		return llm.Response{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	if f.idx >= len(f.steps) {
		// Default to a benign end-turn so over-running tests fail on assertions,
		// not panics.
		return llm.Response{Text: "(unscripted)", StopReason: llm.StopEndTurn}, nil
	}
	step := f.steps[f.idx]
	f.idx++
	return step.resp, step.err
}

func (f *fakeClient) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.idx
}

// --- response builders ----------------------------------------------------

// textResp builds an end-turn text response with the given usage.
func textResp(text string, in, out int64) scriptStep {
	return scriptStep{resp: llm.Response{
		Text:       text,
		StopReason: llm.StopEndTurn,
		Usage:      llm.Usage{InputTokens: in, OutputTokens: out},
	}}
}

// maxTokensResp builds a text response that stopped at the output token cap
// (StopMaxTokens), e.g. a JSON answer cut off mid-object.
func maxTokensResp(text string, in, out int64) scriptStep {
	return scriptStep{resp: llm.Response{
		Text:       text,
		StopReason: llm.StopMaxTokens,
		Usage:      llm.Usage{InputTokens: in, OutputTokens: out},
	}}
}

// toolResp builds a tool-use response requesting a single tool call.
func toolResp(id, name, args string, in, out int64) scriptStep {
	return scriptStep{resp: llm.Response{
		StopReason: llm.StopToolUse,
		ToolCalls: []llm.ToolCall{{
			ID:        id,
			Name:      name,
			Arguments: []byte(args),
		}},
		Usage: llm.Usage{InputTokens: in, OutputTokens: out},
	}}
}
