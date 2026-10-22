package eval

import (
	"context"
	"fmt"
	"sync"

	llmkit "github.com/dpoage/llmkit"
	"github.com/dpoage/llmkit/agent"
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
	caps     llmkit.Capabilities
}

// NewRoleTranscriptStore builds a store from ordered recorded sessions for a
// role. role is a label used only in error messages. caps is the capability
// profile reported by the replay client (pass the recorded model's profile, or
// the zero value if the code under test does not branch on capabilities).
func NewRoleTranscriptStore(role string, caps llmkit.Capabilities, sessions ...*agent.Transcript) *RoleTranscriptStore {
	return &RoleTranscriptStore{role: role, caps: caps, sessions: sessions}
}

// replayRoleClient is the llmkit.Client served to the funnel for a recorded role.
// It detects each NEW agent run and, on that boundary, advances to the next
// recorded session. Within a run it delegates to that session's
// agent.ReplayClient, which validates the tool round-trip structure and serves
// recorded completions in order.
type replayRoleClient struct {
	store *RoleTranscriptStore

	mu        sync.Mutex
	nextIdx   int                 // index of the next session to start
	active    *agent.ReplayClient // the in-flight session's replayer, or nil
	activeRun llmkit.RunID        // the agent run active serves ("" outside a Runner)
}

func newReplayRoleClient(store *RoleTranscriptStore) *replayRoleClient {
	return &replayRoleClient{store: store}
}

func (c *replayRoleClient) Capabilities() llmkit.Capabilities { return c.store.caps }

// Complete serves the next recorded completion. It rolls over to the next
// recorded session when the Runner's run id (llmkit.RunFromContext) changes —
// a RunJSON repair rides its run's id and stays in the session — or, outside a
// Runner, on a request with no prior assistant/tool-result turns.
func (c *replayRoleClient) Complete(ctx context.Context, req llmkit.Request) (llmkit.Response, error) {
	if err := ctx.Err(); err != nil {
		return llmkit.Response{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	run := llmkit.RunFromContext(ctx)
	newRun := run != c.activeRun || (run == "" && isRunStart(req))
	if c.active == nil || newRun {
		if c.active != nil {
			if err := c.active.Err(); err != nil {
				return llmkit.Response{}, fmt.Errorf("eval: %s session %d diverged: %w", c.store.role, c.nextIdx, err)
			}
		}
		if c.nextIdx >= len(c.store.sessions) {
			return llmkit.Response{}, fmt.Errorf(
				"eval: %s replay exhausted: the run started agent run #%d but only %d session(s) were recorded; re-record or reduce the run's work (lenses/candidates)",
				c.store.role, c.nextIdx+1, len(c.store.sessions))
		}
		tr := c.store.sessions[c.nextIdx]
		rc, err := agent.NewReplayClient(tr, tr.RunID, c.store.caps)
		if err != nil {
			return llmkit.Response{}, fmt.Errorf("eval: %s session %d: %w", c.store.role, c.nextIdx+1, err)
		}
		c.active = rc
		c.activeRun = run
		c.nextIdx++
	}

	resp, err := c.active.Complete(ctx, req)
	if err != nil {
		return llmkit.Response{}, fmt.Errorf("eval: %s session %d diverged: %w", c.store.role, c.nextIdx, err)
	}
	return resp, nil
}

// isRunStart reports whether req is the opening turn of a fresh agent run: the
// conversation has no assistant turns and no tool results yet, i.e. only the
// initial user (task) message(s).
func isRunStart(req llmkit.Request) bool {
	for _, m := range req.Messages {
		if m.Role == llmkit.RoleAssistant || m.Role == llmkit.RoleToolResult {
			return false
		}
	}
	return true
}

var _ llmkit.Client = (*replayRoleClient)(nil)
