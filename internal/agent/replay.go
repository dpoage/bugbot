package agent

import (
	"context"
	"fmt"
	"sync"

	"github.com/dpoage/bugbot/internal/llm"
)

// ReplayClient is an [llm.Client] that serves a fixed sequence of recorded
// responses in order, instead of calling a real provider. It is the building
// block for offline evaluation: record a run's transcript once, then replay it
// deterministically against modified harness code.
//
// On each Complete call it returns the next recorded response. It validates
// that the request's tool-call structure matches what was recorded for that
// step (number of prior tool-result messages and the tool names involved),
// erroring if the sequence diverges. Matching is intentionally lenient on exact
// message text — only the tool-call structure must line up — so prompt wording
// can change between record and replay without breaking the replay.
//
// ReplayClient is safe for concurrent use, though a single Runner calls it
// sequentially.
type ReplayClient struct {
	mu        sync.Mutex
	responses []replayStep
	idx       int
	caps      llm.Capabilities
}

// replayStep pairs a recorded response with the tool-call structure expected of
// the request that should elicit it.
type replayStep struct {
	resp          llm.Response
	expectToolIDs []string // tool-result ToolCallIDs expected to precede this request, in order
}

// NewReplayClient builds a ReplayClient from a recorded [Transcript]. It pairs
// each EventAssistant with the tool-result IDs that preceded its request, so
// replay can verify the harness reproduces the same tool round-trips.
//
// caps is returned from Capabilities; pass a profile matching the model the
// transcript was recorded against (or the zero value if it doesn't matter for
// the code under test).
func NewReplayClient(tr *Transcript, caps llm.Capabilities) (*ReplayClient, error) {
	if tr == nil {
		return nil, fmt.Errorf("agent: nil transcript")
	}
	rc := &ReplayClient{caps: caps}

	// Walk events, accumulating the tool-result IDs seen since the last
	// assistant turn; attach them as the "expected preceding tool results" for
	// the next assistant response.
	var pending []string
	for _, ev := range tr.Events {
		switch ev.Kind {
		case EventToolResult:
			pending = append(pending, ev.ToolCallID)
		case EventAssistant:
			step := replayStep{
				resp: llm.Response{
					Text:       ev.Text,
					ToolCalls:  ev.ToolCalls,
					StopReason: ev.StopReason,
				},
				expectToolIDs: pending,
			}
			if ev.Usage != nil {
				step.resp.Usage = *ev.Usage
			}
			rc.responses = append(rc.responses, step)
			pending = nil
		}
	}
	if len(rc.responses) == 0 {
		return nil, fmt.Errorf("agent: transcript has no assistant responses to replay")
	}
	return rc, nil
}

// NewReplayClientFromResponses builds a ReplayClient that serves resps in order
// without any tool-call structure validation. Useful for hand-scripted tests.
func NewReplayClientFromResponses(resps []llm.Response, caps llm.Capabilities) *ReplayClient {
	rc := &ReplayClient{caps: caps}
	for _, r := range resps {
		rc.responses = append(rc.responses, replayStep{resp: r})
	}
	return rc
}

// Capabilities returns the configured capability profile.
func (rc *ReplayClient) Capabilities() llm.Capabilities { return rc.caps }

// Complete serves the next recorded response, validating tool-call structure.
func (rc *ReplayClient) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	if err := ctx.Err(); err != nil {
		return llm.Response{}, err
	}

	rc.mu.Lock()
	defer rc.mu.Unlock()

	if rc.idx >= len(rc.responses) {
		return llm.Response{}, fmt.Errorf("agent: replay exhausted after %d responses; request sequence diverged (extra completion call)", len(rc.responses))
	}
	step := rc.responses[rc.idx]

	// Validate the tool-result structure leading into this request, when the
	// step carries expectations (i.e. built from a transcript).
	if step.expectToolIDs != nil {
		got := trailingToolResultIDs(req.Messages, len(step.expectToolIDs))
		if err := matchToolIDs(rc.idx, step.expectToolIDs, got); err != nil {
			return llm.Response{}, err
		}
	}

	rc.idx++
	return step.resp, nil
}

// trailingToolResultIDs returns the ToolCallIDs of the last n tool-result
// messages in msgs, in order. If there are fewer than n, it returns all of
// them.
func trailingToolResultIDs(msgs []llm.Message, n int) []string {
	var ids []string
	for _, m := range msgs {
		if m.Role == llm.RoleToolResult {
			ids = append(ids, m.ToolCallID)
		}
	}
	if n >= 0 && len(ids) > n {
		ids = ids[len(ids)-n:]
	}
	return ids
}

// matchToolIDs compares expected and actual tool-result ID sequences, returning
// a descriptive divergence error if they differ.
func matchToolIDs(step int, want, got []string) error {
	if len(want) != len(got) {
		return fmt.Errorf("agent: replay diverged at step %d: expected %d preceding tool result(s) %v, got %d %v",
			step+1, len(want), want, len(got), got)
	}
	for i := range want {
		if want[i] != got[i] {
			return fmt.Errorf("agent: replay diverged at step %d: tool result %d was %q, recorded %q",
				step+1, i, got[i], want[i])
		}
	}
	return nil
}
