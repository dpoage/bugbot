package eval

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/llm"
)

func TestScriptedClient_RoutingPrecedence(t *testing.T) {
	ctx := context.Background()
	c := NewScriptedClient()
	c.OnSystemContains("lens-a", `{"a":1}`)
	c.OnTaskContains("title-x", `{"x":1}`)

	// System-prompt route.
	got, _ := c.Complete(ctx, llm.Request{System: "focus: lens-a"})
	if got.Text != `{"a":1}` {
		t.Errorf("system route: got %q", got.Text)
	}
	// Task route.
	got, _ = c.Complete(ctx, llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: "refute title-x"}}})
	if got.Text != `{"x":1}` {
		t.Errorf("task route: got %q", got.Text)
	}
	// No match => fallback => empty candidates.
	got, _ = c.Complete(ctx, llm.Request{System: "nothing"})
	if got.Text != EmptyCandidates {
		t.Errorf("fallback: got %q", got.Text)
	}
	// First registered match wins when two could apply.
	c2 := NewScriptedClient()
	c2.OnSystemContains("x", "FIRST")
	c2.OnSystemContains("x", "SECOND")
	got, _ = c2.Complete(ctx, llm.Request{System: "xx"})
	if got.Text != "FIRST" {
		t.Errorf("first-match-wins violated: %q", got.Text)
	}
}

func TestScriptedClient_UsageAndCallCount(t *testing.T) {
	ctx := context.Background()
	c := NewScriptedClient()
	got, _ := c.Complete(ctx, llm.Request{})
	if got.Usage.InputTokens == 0 || got.Usage.OutputTokens == 0 {
		t.Errorf("usage must be nonzero for spend accounting: %+v", got.Usage)
	}
	if got.StopReason != llm.StopEndTurn {
		t.Errorf("stop reason = %q, want end_turn", got.StopReason)
	}
	_, _ = c.Complete(ctx, llm.Request{})
	if c.CallCount() != 2 {
		t.Errorf("call count = %d, want 2", c.CallCount())
	}
}

func TestScriptedClient_ContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := NewScriptedClient()
	if _, err := c.Complete(ctx, llm.Request{}); err == nil {
		t.Errorf("expected context error")
	}
}

func TestCandidates_ValidJSONAndDefaults(t *testing.T) {
	body := Candidates(
		CandidateJSON{File: "a.go", Line: 3, Title: "t1"}, // defaults sev/conf
		CandidateJSON{File: "b.go", Line: 4, Title: `quote "inside"`, Severity: "low", Confidence: "medium"},
	)
	var parsed struct {
		Candidates []map[string]any `json:"candidates"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("Candidates produced invalid JSON: %v\n%s", err, body)
	}
	if len(parsed.Candidates) != 2 {
		t.Fatalf("want 2 candidates, got %d", len(parsed.Candidates))
	}
	// Defaults applied.
	if parsed.Candidates[0]["severity"] != "high" || parsed.Candidates[0]["confidence"] != "high" {
		t.Errorf("defaults not applied: %v", parsed.Candidates[0])
	}
	// Embedded quotes are escaped (not a literal-string builder bug).
	if !strings.Contains(parsed.Candidates[1]["title"].(string), `quote "inside"`) {
		t.Errorf("title not round-tripped: %v", parsed.Candidates[1]["title"])
	}
}

// TestCleanCase_FPCountedThroughFullPipeline drives the real funnel on clean
// code where a finder confabulates a bug AND the verifier (wrongly) confirms it,
// so the false positive slips all the way through to a persisted finding. The
// scorer must count it as an FP on the clean case. This is the negative control
// for the benchmark gate: it proves the gate would catch a real precision
// regression.
func TestCleanCase_FPCountedThroughFullPipeline(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	c := Case{
		Name:   "clean-but-leaky",
		Repo:   FixtureSpec{Files: map[string]string{"math.go": cleanSrc}},
		Seeded: nil,
		Scripted: &ScriptedCase{
			Finder: func(sc *ScriptedClient) {
				sc.OnSystemContains(lensBoundary, Candidates(CandidateJSON{
					File: "math.go", Line: 6, Title: "spurious overflow",
					Description: "x", Severity: "high", Evidence: "y", Confidence: "high",
				}))
			},
			Verifier: func(sc *ScriptedClient) {
				// A broken verifier that fails to refute the confabulation.
				sc.OnTaskContains("spurious overflow", NotRefutedJSON)
			},
		},
	}
	cr, err := RunCase(ctx, c)
	if err != nil {
		t.Fatalf("run case: %v", err)
	}
	if !cr.Clean {
		t.Fatalf("case must be clean")
	}
	if cr.FalsePositives != 1 {
		t.Errorf("a finding that slipped through on clean code must be an FP: fp=%d findings=%d",
			cr.FalsePositives, len(cr.Findings))
	}
	if cr.Precision() != 0 {
		t.Errorf("clean case with an FP has precision 0, got %v", cr.Precision())
	}
}
