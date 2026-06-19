package funnel

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/store"
)

// --- helpers ----------------------------------------------------------------

// leadCaptureClient records every task message its finder agents receive. It
// returns a scripted response (empty candidates by default) so the pipeline can
// complete without error.
type leadCaptureClient struct {
	mu    sync.Mutex
	tasks []string
	inner *scriptedClient
}

func newLeadCaptureClient() *leadCaptureClient {
	return &leadCaptureClient{inner: newScriptedClient()}
}

func (c *leadCaptureClient) Capabilities() llm.Capabilities { return llm.Capabilities{} }

func (c *leadCaptureClient) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	// Capture the first user message (the task) on each completion.
	for _, m := range req.Messages {
		if m.Role == llm.RoleUser {
			c.mu.Lock()
			c.tasks = append(c.tasks, m.Content)
			c.mu.Unlock()
			break
		}
	}
	return c.inner.Complete(ctx, req)
}

func (c *leadCaptureClient) tasksSeen() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.tasks))
	copy(out, c.tasks)
	return out
}

// --- Tests -------------------------------------------------------------------

// TestSweep_LeadsInjected_IntoFinderTask proves that a lead seeded in the store
// before a Sweep is injected into the matching finder's task message. The
// leadCaptureClient records every task it receives; we assert the CROSS-LENS
// LEADS section appears in at least one task for the target lens.
func TestSweep_LeadsInjected_IntoFinderTask(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	// Seed a lead targeting the nil-safety lens.
	lead := store.Lead{
		PosterLens: "concurrency",
		TargetLens: "nil-safety/error-handling",
		File:       "bug.go",
		Line:       10,
		Note:       "locking around cache map looks inconsistent",
	}
	if err := st.AddLead(ctx, lead); err != nil {
		t.Fatalf("AddLead: %v", err)
	}

	// leadCaptureClient records all task messages; returns empty candidates.
	finder := newLeadCaptureClient()
	verifier := newScriptedClient()

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{})
	if err != nil {
		t.Fatal(err)
	}

	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	// At least one task must contain the CROSS-LENS LEADS section with our lead.
	foundLeadsSection := false
	for _, task := range finder.tasksSeen() {
		if strings.Contains(task, "CROSS-LENS LEADS") &&
			strings.Contains(task, "bug.go:10") &&
			strings.Contains(task, "locking around cache map looks inconsistent") {
			foundLeadsSection = true
			break
		}
	}
	if !foundLeadsSection {
		t.Errorf("no finder task contained the CROSS-LENS LEADS section with the seeded lead; tasks: %v", finder.tasksSeen())
	}

	// The lead must be consumed after the run.
	pending, err := st.PendingLeads(ctx, "nil-safety/error-handling")
	if err != nil {
		t.Fatalf("PendingLeads: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("lead should be consumed after Sweep, got %d pending", len(pending))
	}

	// LeadsConsumed counter must be 1.
	if res.Stats.LeadsConsumed != 1 {
		t.Errorf("LeadsConsumed = %d, want 1", res.Stats.LeadsConsumed)
	}
}

// TestSweep_LeadsPosted_ThroughToolCall proves that a scripted finder emitting
// a post_lead tool call results in: (a) a lead row in the store, (b)
// LeadsPosted==1, and (c) a second Sweep injects the lead into the target
// lens's task.
func TestSweep_LeadsPosted_ThroughToolCall(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	// Build a finder client that, for the nil-safety lens, emits a post_lead
	// tool call followed by a final empty-candidates answer.
	const postLeadBody = `{"target_lens":"concurrency","file":"bug.go","line":10,"note":"locking looks inconsistent"}`
	nilSafetyPostClient := newPostLeadToolCallClient(postLeadBody)

	verifier := newScriptedClient()
	verifier.fallback = notRefutedJSON

	f, err := New(
		RoleClients{Finder: nilSafetyPostClient, Verifier: verifier},
		st, repo,
		Options{Discovery: DiscoveryConfig{Lenses: []string{"nil-safety/error-handling"}}},
	)
	if err != nil {
		t.Fatal(err)
	}

	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatalf("first Sweep: %v", err)
	}

	// LeadsPosted must be 1.
	if res.Stats.LeadsPosted != 1 {
		t.Errorf("LeadsPosted = %d, want 1", res.Stats.LeadsPosted)
	}

	// The lead must be stored in the database.
	pending, err := st.PendingLeads(ctx, "concurrency")
	if err != nil {
		t.Fatalf("PendingLeads after first sweep: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("want 1 pending lead for concurrency, got %d", len(pending))
	}
	ld := pending[0]
	if ld.File != "bug.go" || ld.Line != 10 {
		t.Errorf("lead location = %s:%d, want bug.go:10", ld.File, ld.Line)
	}
	if !strings.Contains(ld.Note, "locking") {
		t.Errorf("lead note = %q, expected it to mention locking", ld.Note)
	}

	// Second sweep with the concurrency lens: the lead should be injected.
	finder2 := newLeadCaptureClient()
	f2, err := New(
		RoleClients{Finder: finder2, Verifier: newScriptedClient()},
		st, repo,
		Options{Discovery: DiscoveryConfig{Lenses: []string{"concurrency"}}},
	)
	if err != nil {
		t.Fatal(err)
	}

	res2, err := f2.Sweep(ctx)
	if err != nil {
		t.Fatalf("second Sweep: %v", err)
	}

	// LeadsConsumed must be 1 in the second sweep.
	if res2.Stats.LeadsConsumed != 1 {
		t.Errorf("second sweep LeadsConsumed = %d, want 1", res2.Stats.LeadsConsumed)
	}

	// The concurrency finder's task must contain the lead.
	foundLead := false
	for _, task := range finder2.tasksSeen() {
		if strings.Contains(task, "CROSS-LENS LEADS") && strings.Contains(task, "bug.go:10") {
			foundLead = true
			break
		}
	}
	if !foundLead {
		t.Errorf("second sweep did not inject lead into concurrency finder task; tasks: %v", finder2.tasksSeen())
	}

	// Lead must be consumed after the second sweep.
	pending2, err := st.PendingLeads(ctx, "concurrency")
	if err != nil {
		t.Fatalf("PendingLeads after second sweep: %v", err)
	}
	if len(pending2) != 0 {
		t.Errorf("lead should be consumed after second Sweep, got %d pending", len(pending2))
	}
}

// TestSweep_InactiveLens_LeadStaysPending proves that a lead targeting a lens
// that is NOT active in the current run stays pending (it will be consumed when
// that lens next runs). This validates the lens-restriction logic.
func TestSweep_InactiveLens_LeadStaysPending(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	// Seed a lead targeting boundary-conditions.
	lead := store.Lead{
		PosterLens: "nil-safety/error-handling",
		TargetLens: "boundary-conditions",
		File:       "bug.go",
		Line:       5,
		Note:       "off-by-one in loop",
	}
	if err := st.AddLead(ctx, lead); err != nil {
		t.Fatalf("AddLead: %v", err)
	}

	// Run with ONLY nil-safety — boundary-conditions is inactive.
	finder := newLeadCaptureClient()
	f, err := New(
		RoleClients{Finder: finder, Verifier: newScriptedClient()},
		st, repo,
		Options{Discovery: DiscoveryConfig{Lenses: []string{"nil-safety/error-handling"}}},
	)
	if err != nil {
		t.Fatal(err)
	}

	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	// The lead should NOT be consumed (boundary-conditions did not run).
	pending, err := st.PendingLeads(ctx, "boundary-conditions")
	if err != nil {
		t.Fatalf("PendingLeads: %v", err)
	}
	if len(pending) != 1 {
		t.Errorf("want lead to stay pending for inactive lens, got %d pending", len(pending))
	}

	// LeadsConsumed must be 0 (the nil-safety lens had no leads; boundary-conditions was inactive).
	if res.Stats.LeadsConsumed != 0 {
		t.Errorf("LeadsConsumed = %d, want 0 (inactive lens leads not consumed)", res.Stats.LeadsConsumed)
	}
}

// TestVerify_RefuterToolset_NoPostLead pins that post_lead is absent from the
// refuter tool set. This is the invariant that preserves refuter independence.
// It uses VerifyFinding which re-runs refuters on a single stored finding
// without going through the full pipeline.
func TestVerify_RefuterToolset_NoPostLead(t *testing.T) {
	ctx := context.Background()
	_, repo := openFixture(t)

	capture := &toolCaptureClient{}
	f := &Funnel{
		repo:    repo,
		clients: RoleClients{Verifier: capture},
		opts:    Options{Limits: StageLimits{Refuters: 1}},
		lenses:  selectLenses(nil),
	}

	fnd := store.Finding{
		Fingerprint: "fp-no-post-lead",
		Title:       "test finding",
		Severity:    "high",
		File:        "bug.go",
		Line:        10,
		Lens:        "nil-safety/error-handling",
	}
	_, _, err := f.VerifyFinding(ctx, fnd)
	if err != nil {
		t.Fatalf("VerifyFinding: %v", err)
	}

	if len(capture.toolNames) == 0 {
		t.Fatal("verifier client was never called")
	}
	for i, names := range capture.toolNames {
		for _, n := range names {
			if n == "post_lead" {
				t.Errorf("completion %d offered post_lead to a refuter agent: %v", i, names)
			}
		}
	}
}

// --- post_lead tool-call client ---------------------------------------------

// postLeadToolCallClient is a finder client that emits a post_lead tool call on
// its first completion (simulating a finder that discovers a cross-lens
// suspicion), then on the second completion (after the tool result is fed back)
// returns empty candidates so the pipeline completes cleanly.
type postLeadToolCallClient struct {
	mu   sync.Mutex
	body string
	used int
	base *scriptedClient
}

func newPostLeadToolCallClient(postLeadArgsJSON string) *postLeadToolCallClient {
	return &postLeadToolCallClient{
		body: postLeadArgsJSON,
		base: newScriptedClient(),
	}
}

func (c *postLeadToolCallClient) Capabilities() llm.Capabilities { return llm.Capabilities{} }

func (c *postLeadToolCallClient) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	c.mu.Lock()
	n := c.used
	c.used++
	c.mu.Unlock()

	usage := llm.Usage{
		InputTokens:          c.base.inUsage,
		OutputTokens:         c.base.outUsage,
		CacheReadInputTokens: c.base.cachedUsage,
	}

	// If the request contains a tool-result message, the tool call was already
	// dispatched; return empty candidates so the pipeline completes.
	for _, m := range req.Messages {
		if m.Role == llm.RoleToolResult {
			return llm.Response{
				Text:       emptyCandidates,
				StopReason: llm.StopEndTurn,
				Usage:      usage,
			}, nil
		}
	}

	// On the first call without a tool-result: emit the post_lead tool call.
	if n == 0 {
		return llm.Response{
			ToolCalls: []llm.ToolCall{{
				ID:        "post-lead-1",
				Name:      "post_lead",
				Arguments: json.RawMessage(c.body),
			}},
			StopReason: llm.StopToolUse,
			Usage:      usage,
		}, nil
	}

	// Fallback: empty candidates.
	return llm.Response{
		Text:       emptyCandidates,
		StopReason: llm.StopEndTurn,
		Usage:      usage,
	}, nil
}
