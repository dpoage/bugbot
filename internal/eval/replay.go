package eval

import (
	"context"
	"fmt"
	"sync"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/llm"
)

// RoleTranscriptStore holds the ordered transcript recordings for ONE funnel
// role (finder or verifier) and serves them to the replay client one agent-run
// at a time.
//
// # Why a per-role store, not one big ReplayClient
//
// agent.ReplayClient replays the sequence of completions within a SINGLE agent
// run (one RunJSON call drives one runner that may make several completions).
// But the funnel creates MANY runners per role: one finder runner per
// (lens, chunk), and one verifier runner per candidate, each of which makes its
// own completions. A single client instance is shared across all of them.
//
// So replay must be two-level: a sequence OF agent runs, each itself a sequence
// of completions. RoleTranscriptStore is that outer sequence. Each entry is one
// recorded session (an agent.Transcript). The store hands out one session per
// agent run, in recorded order.
//
// # Determinism requirement
//
// Because sessions are popped in order, the funnel must drive the role's runs
// in the SAME order they were recorded. The funnel launches finder lenses in
// yield order and verifies candidates in arrival order; both are deterministic
// when MaxParallel is 1 (which RunCase defaults to). Recording and replay MUST
// both run serially. If the order diverges, the underlying ReplayClient's
// tool-structure validation will surface a divergence error, and if the run
// asks for more sessions than were recorded the store errors loudly rather than
// silently returning empty output.
type RoleTranscriptStore struct {
	role     string
	sessions []*agent.Transcript
	caps     llm.Capabilities
}

// NewRoleTranscriptStore builds a store from ordered recorded sessions for a
// role. role is a label used only in error messages. caps is the capability
// profile reported by the replay client (pass the recorded model's profile, or
// the zero value if the code under test does not branch on capabilities).
func NewRoleTranscriptStore(role string, caps llm.Capabilities, sessions ...*agent.Transcript) *RoleTranscriptStore {
	return &RoleTranscriptStore{role: role, caps: caps, sessions: sessions}
}

// replayRoleClient is the llm.Client served to the funnel for a recorded role.
// It detects each NEW agent run (a fresh runner always opens with a single
// user-message conversation: no prior tool results) and, on that boundary,
// advances to the next recorded session. Within a run it delegates to that
// session's agent.ReplayClient, which validates the tool round-trip structure
// and serves recorded completions in order.
type replayRoleClient struct {
	store *RoleTranscriptStore

	mu      sync.Mutex
	nextIdx int                 // index of the next session to start
	active  *agent.ReplayClient // the in-flight session's replayer, or nil
}

func newReplayRoleClient(store *RoleTranscriptStore) *replayRoleClient {
	return &replayRoleClient{store: store}
}

func (c *replayRoleClient) Capabilities() llm.Capabilities { return c.store.caps }

// Complete serves the next recorded completion. On the first turn of a new
// agent run (a request with no prior assistant/tool-result turns) it rolls over
// to the next recorded session.
func (c *replayRoleClient) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	if err := ctx.Err(); err != nil {
		return llm.Response{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if isRunStart(req) || c.active == nil {
		if c.nextIdx >= len(c.store.sessions) {
			return llm.Response{}, fmt.Errorf(
				"eval: %s replay exhausted: the run started agent run #%d but only %d session(s) were recorded; re-record or reduce the run's work (lenses/candidates)",
				c.store.role, c.nextIdx+1, len(c.store.sessions))
		}
		tr := c.store.sessions[c.nextIdx]
		rc, err := agent.NewReplayClient(tr, c.store.caps)
		if err != nil {
			return llm.Response{}, fmt.Errorf("eval: %s session %d: %w", c.store.role, c.nextIdx+1, err)
		}
		c.active = rc
		c.nextIdx++
	}

	resp, err := c.active.Complete(ctx, req)
	if err != nil {
		return llm.Response{}, fmt.Errorf("eval: %s session %d diverged: %w", c.store.role, c.nextIdx, err)
	}
	return resp, nil
}

// isRunStart reports whether req is the opening turn of a fresh agent run: the
// conversation has no assistant turns and no tool results yet, i.e. only the
// initial user (task) message(s). A new runner always starts here, so this is
// the boundary at which we advance to the next recorded session.
func isRunStart(req llm.Request) bool {
	for _, m := range req.Messages {
		if m.Role == llm.RoleAssistant || m.Role == llm.RoleToolResult {
			return false
		}
	}
	return true
}

var _ llm.Client = (*replayRoleClient)(nil)
