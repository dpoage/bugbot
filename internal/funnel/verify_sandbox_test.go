package funnel

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/sandbox"
)

// --- Fake sandbox for funnel tests ----------------------------------------

// funnelFakeSandbox is a concurrency-safe fake sandbox for funnel-level tests.
// It returns a scripted result and records calls. The counter is atomic so the
// fake stays safe under parallel candidates / multiple refuters.
type funnelFakeSandbox struct {
	result sandbox.Result
	calls  atomic.Int32
}

func (f *funnelFakeSandbox) Exec(_ context.Context, _ sandbox.Spec) (sandbox.Result, error) {
	f.calls.Add(1)
	return f.result, nil
}

var _ sandbox.Sandbox = (*funnelFakeSandbox)(nil)

// --- scriptedClientWithToolCall extends scriptedClient to emit a tool call
//
// on the first completion (simulating a refuter that issues sandbox_exec),
// then on the second completion (tool result feed-back) returns the verdict.
type toolCallClient struct {
	callCount    atomic.Int32
	toolCallBody string
	verdictBody  string
	inUsage      int64
	outUsage     int64
}

func newToolCallClient(toolCallBody, verdictBody string) *toolCallClient {
	return &toolCallClient{
		toolCallBody: toolCallBody,
		verdictBody:  verdictBody,
		inUsage:      100,
		outUsage:     50,
	}
}

func (c *toolCallClient) Capabilities() llm.Capabilities { return llm.Capabilities{} }

func (c *toolCallClient) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	c.callCount.Add(1)
	usage := llm.Usage{InputTokens: c.inUsage, OutputTokens: c.outUsage}

	// On first call (no tool-result messages yet), emit a sandbox_exec tool call.
	// On subsequent calls (the agent is feeding back the tool result), return the
	// verdict JSON directly.
	for _, m := range req.Messages {
		if m.Role == llm.RoleToolResult {
			// The agent is feeding back the sandbox result; return the verdict.
			return llm.Response{
				Text:       c.verdictBody,
				StopReason: llm.StopEndTurn,
				Usage:      usage,
			}, nil
		}
	}

	// First call: emit a tool call with the configured body as Arguments.
	return llm.Response{
		ToolCalls: []llm.ToolCall{{
			ID:        "call-1",
			Name:      "sandbox_exec",
			Arguments: json.RawMessage(c.toolCallBody),
		}},
		StopReason: llm.StopToolUse,
		Usage:      usage,
	}, nil
}

// --- Tests ----------------------------------------------------------------

// TestSandboxGating_FeatureOff proves no refuter gets the sandbox_exec tool
// when SandboxOpts.Enabled is false (the default).
func TestSandboxGating_FeatureOff(t *testing.T) {
	_, repo := openFixture(t)
	f := &Funnel{
		repo:   repo,
		opts:   Options{SandboxOpts: SandboxOpts{Enabled: false}},
		lenses: selectLenses(nil),
	}

	c := Candidate{Severity: "critical", Title: "test"}
	var execs atomic.Int32
	var millis atomic.Int64
	tool := f.buildSandboxTool(c, &execs, &millis)
	if tool != nil {
		t.Error("buildSandboxTool should return nil when feature is disabled")
	}
}

// TestSandboxGating_NilSandbox proves that even with Enabled=true, nil Sandbox
// means no tool.
func TestSandboxGating_NilSandbox(t *testing.T) {
	_, repo := openFixture(t)
	f := &Funnel{
		repo: repo,
		opts: Options{SandboxOpts: SandboxOpts{
			Enabled: true,
			Sandbox: nil,
		}},
	}

	c := Candidate{Severity: "critical"}
	var execs atomic.Int32
	var millis atomic.Int64
	tool := f.buildSandboxTool(c, &execs, &millis)
	if tool != nil {
		t.Error("buildSandboxTool with nil sandbox must return nil")
	}
}

// TestSandboxGating_BelowMinSeverity proves a candidate below the threshold
// does not receive the tool.
func TestSandboxGating_BelowMinSeverity(t *testing.T) {
	sb := &funnelFakeSandbox{}
	_, repo := openFixture(t)
	f := &Funnel{
		repo: repo,
		opts: Options{SandboxOpts: SandboxOpts{
			Enabled:     true,
			Sandbox:     sb,
			MinSeverity: "high",
		}},
	}

	// low and medium are below high.
	for _, sev := range []domain.Severity{"low", "medium"} {
		c := Candidate{Severity: sev}
		var execs atomic.Int32
		var millis atomic.Int64
		tool := f.buildSandboxTool(c, &execs, &millis)
		if tool != nil {
			t.Errorf("buildSandboxTool with severity=%q should return nil (below threshold high)", sev)
		}
	}
}

// TestSandboxGating_AtOrAboveMinSeverity proves a candidate at or above the
// threshold receives the tool.
func TestSandboxGating_AtOrAboveMinSeverity(t *testing.T) {
	sb := &funnelFakeSandbox{}
	_, repo := openFixture(t)
	f := &Funnel{
		repo: repo,
		opts: Options{SandboxOpts: SandboxOpts{
			Enabled:     true,
			Sandbox:     sb,
			MinSeverity: "high",
			MaxExecs:    3,
		}},
	}

	// high and critical meet or exceed the threshold.
	for _, sev := range []domain.Severity{"high", "critical"} {
		c := Candidate{Severity: sev}
		var execs atomic.Int32
		var millis atomic.Int64
		tool := f.buildSandboxTool(c, &execs, &millis)
		if tool == nil {
			t.Errorf("buildSandboxTool with severity=%q should return a tool (at/above threshold high)", sev)
		}
	}
}

// TestSandboxGating_DefaultMinSeverity confirms the default min severity is
// "high" when MinSeverity is empty.
func TestSandboxGating_DefaultMinSeverity(t *testing.T) {
	sb := &funnelFakeSandbox{}
	_, repo := openFixture(t)
	f := &Funnel{
		repo: repo,
		opts: Options{SandboxOpts: SandboxOpts{
			Enabled:     true,
			Sandbox:     sb,
			MinSeverity: "", // empty -> default "high"
			MaxExecs:    3,
		}},
	}

	var execs atomic.Int32
	var millis atomic.Int64

	// medium is below default "high" threshold.
	if tool := f.buildSandboxTool(Candidate{Severity: "medium"}, &execs, &millis); tool != nil {
		t.Error("medium should be below default high threshold")
	}
	// high meets the default threshold.
	if tool := f.buildSandboxTool(Candidate{Severity: "high"}, &execs, &millis); tool == nil {
		t.Error("high should meet the default high threshold")
	}
}

// TestSweep_SandboxExec_StatsAggregated runs a full sweep where a refuter uses
// sandbox_exec (via a scripted tool-call client) and asserts that Stats.SandboxExecs
// is incremented and SandboxExecMillis is non-zero.
func TestSweep_SandboxExec_StatsAggregated(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	// The finder reports one high-severity candidate.
	const candTitle = "nil deref in Greeting"
	finder := newScriptedClient().onSystemContains(
		"nil-safety/error-handling",
		candJSON(`{"file":"bug.go","line":10,"title":"`+candTitle+`","description":"cfg nil","severity":"high","evidence":"no nil check","confidence":"high"}`),
	)

	// The verifier first emits a sandbox_exec tool call, then (on the tool-result
	// feed-back) returns a not-refuted verdict.
	sbCallJSON := `{"cmd":["go","test","-run","TestGreeting","./..."]}`
	verdictJSON := notRefutedJSON

	sb := &funnelFakeSandbox{
		result: sandbox.Result{
			ExitCode: 0,
			Stdout:   "ok\n",
			Duration: 42 * time.Millisecond,
		},
	}

	verifier := newToolCallClient(sbCallJSON, verdictJSON)

	sandboxOpts := SandboxOpts{
		Sandbox:     sb,
		Enabled:     true,
		MinSeverity: "high",
		MaxExecs:    3,
	}

	f, err := New(
		RoleClients{Finder: finder, Verifier: verifier},
		st, repo,
		Options{
			Discovery:   DiscoveryConfig{Lenses: []string{"nil-safety/error-handling"}},
			Limits:      StageLimits{Refuters: 1}, // one refuter so we can reason about exact counts
			SandboxOpts: sandboxOpts,
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// The candidate should survive (refuter issued the tool call but then said not-refuted).
	if len(res.Findings) != 1 {
		t.Errorf("want 1 finding, got %d: %+v", len(res.Findings), res.Findings)
	}

	// The sandbox must have been called.
	if sb.calls.Load() == 0 {
		t.Error("sandbox was never called; expected at least one sandbox_exec")
	}

	// Stats must reflect the execution.
	if res.Stats.SandboxExecs < 1 {
		t.Errorf("Stats.SandboxExecs = %d, want >= 1", res.Stats.SandboxExecs)
	}
	if res.Stats.SandboxExecMillis <= 0 {
		t.Errorf("Stats.SandboxExecMillis = %d, want > 0", res.Stats.SandboxExecMillis)
	}
}

// TestSweep_SandboxExec_AbsentForBelowThreshold proves that a low-severity
// candidate does NOT receive the sandbox_exec tool even when the feature is on.
func TestSweep_SandboxExec_AbsentForBelowThreshold(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	// Finder reports a LOW-severity candidate.
	finder := newScriptedClient().onSystemContains(
		"nil-safety/error-handling",
		candJSON(`{"file":"bug.go","line":10,"title":"low sev bug","description":"x","severity":"low","evidence":"y","confidence":"high"}`),
	)
	verifier := verifierRouting(newScriptedClient())

	sb := &funnelFakeSandbox{}
	sandboxOpts := SandboxOpts{
		Sandbox:     sb,
		Enabled:     true,
		MinSeverity: "high",
		MaxExecs:    3,
	}

	f, err := New(
		RoleClients{Finder: finder, Verifier: verifier},
		st, repo,
		Options{
			Discovery:   DiscoveryConfig{Lenses: []string{"nil-safety/error-handling"}},
			SandboxOpts: sandboxOpts,
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// The sandbox must NEVER be called for below-threshold candidates.
	if sb.calls.Load() > 0 {
		t.Errorf("sandbox was called %d times for a low-severity candidate; want 0", sb.calls.Load())
	}
	if res.Stats.SandboxExecs != 0 {
		t.Errorf("Stats.SandboxExecs = %d, want 0", res.Stats.SandboxExecs)
	}
}

// TestHelpers_SandboxMinSeverity confirms the default and passthrough logic.
func TestHelpers_SandboxMinSeverity(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "high"},
		{"critical", "critical"},
		{"high", "high"},
		{"medium", "medium"},
		{"low", "low"},
		{"garbage", "high"},
	}
	for _, tc := range cases {
		if got := sandboxMinSeverity(tc.in); string(got) != tc.want {
			t.Errorf("sandboxMinSeverity(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestHelpers_SandboxMaxExecs confirms the default and minimum enforcement.
func TestHelpers_SandboxMaxExecs(t *testing.T) {
	if sandboxMaxExecs(0) != 3 {
		t.Errorf("sandboxMaxExecs(0) = %d, want 3", sandboxMaxExecs(0))
	}
	if sandboxMaxExecs(-1) != 3 {
		t.Errorf("sandboxMaxExecs(-1) = %d, want 3", sandboxMaxExecs(-1))
	}
	if sandboxMaxExecs(5) != 5 {
		t.Errorf("sandboxMaxExecs(5) = %d, want 5", sandboxMaxExecs(5))
	}
}

// TestVerifierSystemPrompt_WithSandbox confirms the sandbox paragraph appears.
func TestVerifierSystemPrompt_WithSandbox(t *testing.T) {
	p := verifierSystemPrompt("senior Go engineer", true, true)
	if !strings.Contains(p, "sandbox_exec") {
		t.Error("system prompt with sandbox should mention sandbox_exec")
	}
	if !strings.Contains(p, "PREFER EMPIRICAL DEMONSTRATION") {
		t.Error("system prompt with sandbox should mention empirical demonstration")
	}
}

// TestVerifierSystemPrompt_WithoutSandbox confirms no sandbox paragraph when false.
func TestVerifierSystemPrompt_WithoutSandbox(t *testing.T) {
	const persona = "senior Go engineer"
	p := verifierSystemPrompt(persona, false, true)
	if strings.Contains(p, "sandbox_exec") {
		t.Error("system prompt without sandbox must not mention sandbox_exec")
	}
	if p != verifierSystemBase(persona, true)+verifierNoSandboxParagraph {
		t.Error("system prompt without sandbox must equal verifierSystemBase + the no-exec prohibition (bugbot-mi5.20)")
	}
}

// toolCaptureClient records the tool names offered on each completion and
// returns a fixed not-refuted verdict. It lets tests pin which tools a stage
// exposes without exercising any of them.
type toolCaptureClient struct {
	mu        sync.Mutex
	toolNames [][]string
}

func (c *toolCaptureClient) Capabilities() llm.Capabilities { return llm.Capabilities{} }

func (c *toolCaptureClient) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	names := make([]string, 0, len(req.Tools))
	for _, td := range req.Tools {
		names = append(names, td.Name)
	}
	c.mu.Lock()
	c.toolNames = append(c.toolNames, names)
	c.mu.Unlock()
	return llm.Response{Text: notRefutedJSON, StopReason: llm.StopEndTurn, Usage: llm.Usage{InputTokens: 10, OutputTokens: 5}}, nil
}

// TestVerifyFinding_NoSandboxTool pins the deliberate omission documented in
// VerifyFinding: daemon re-verification never offers sandbox_exec, even with
// the feature enabled and a high-severity finding. Re-verify runs on every
// open finding every code change; sandbox spend belongs to initial verify.
func TestVerifyFinding_NoSandboxTool(t *testing.T) {
	ctx := context.Background()
	_, repo := openFixture(t)

	capture := &toolCaptureClient{}
	f := &Funnel{
		repo:    repo,
		clients: RoleClients{Verifier: capture},
		opts: Options{
			Limits: StageLimits{Refuters: 1},
			SandboxOpts: SandboxOpts{
				Enabled:     true,
				Sandbox:     &funnelFakeSandbox{},
				MinSeverity: "high",
				MaxExecs:    3,
			},
		},
		lenses: selectLenses(nil),
	}

	fnd := domain.Finding{
		Fingerprint: "fp-test",
		Title:       "nil deref",
		Severity:    "critical",
		File:        "bug.go",
		Line:        10,
		Lens:        "nil-safety/error-handling",
	}
	refuted, _, err := f.VerifyFinding(ctx, fnd)
	if err != nil {
		t.Fatalf("VerifyFinding: %v", err)
	}
	if refuted {
		t.Fatalf("scripted not-refuted verdict should not refute")
	}
	if len(capture.toolNames) == 0 {
		t.Fatal("verifier client was never called")
	}
	for i, names := range capture.toolNames {
		for _, n := range names {
			if n == "sandbox_exec" {
				t.Errorf("completion %d offered sandbox_exec to a re-verification refuter: %v", i, names)
			}
		}
	}
}
