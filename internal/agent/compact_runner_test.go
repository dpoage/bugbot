package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/llm"
)

// bigEchoTool returns a fixed large blob regardless of input, so each tool turn
// inflates the history by a known, prunable amount.
type bigEchoTool struct {
	name string
	blob string
}

func (b bigEchoTool) Def() llm.ToolDef {
	return llm.ToolDef{
		Name:        b.name,
		Description: "returns a large blob",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"v":{"type":"string"}}}`),
	}
}

func (b bigEchoTool) Run(ctx context.Context, args json.RawMessage) (string, error) {
	return b.blob, nil
}

// lastUserOrToolBytes sums tool-result content bytes in a request, the quantity
// compaction actually reclaims.
func toolResultBytes(req llm.Request) int {
	n := 0
	for _, m := range req.Messages {
		if m.Role == llm.RoleToolResult {
			n += len(m.Content)
		}
	}
	return n
}

func TestRun_CompactionShrinksRequest(t *testing.T) {
	blob := strings.Repeat("source line\n", 1000) // ~12 KB per tool result
	// Drive 5 tool turns then a final answer. Each tool turn appends a big result,
	// so by the later turns history exceeds the threshold and compaction fires.
	fc := newFakeClient(
		toolResp("c1", "big", `{"v":"1"}`, 100, 10),
		toolResp("c2", "big", `{"v":"2"}`, 100, 10),
		toolResp("c3", "big", `{"v":"3"}`, 100, 10),
		toolResp("c4", "big", `{"v":"4"}`, 100, 10),
		toolResp("c5", "big", `{"v":"5"}`, 100, 10),
		textResp("final answer", 100, 10),
	)
	// Threshold ~5 KB worth of tokens (bytes/4): crossed after a couple of 12 KB
	// results accrue. recentK defaults to compactRecentToolResults (4).
	r := NewRunner(fc, []Tool{bigEchoTool{name: "big", blob: blob}}, "sys",
		WithLimits(Limits{MaxIterations: 20, HistoryTokenBudget: 1500}))

	out, err := r.Run(context.Background(), "task")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.FinalText != "final answer" {
		t.Fatalf("FinalText = %q", out.FinalText)
	}

	// The final completion's request must carry FEWER tool-result bytes than a
	// naive append-only run would (which is all 5 blobs ~= 60 KB). With recent-K=4
	// and compaction, the oldest result(s) are stubbed.
	fc.mu.Lock()
	reqs := append([]llm.Request(nil), fc.requests...)
	fc.mu.Unlock()
	if len(reqs) == 0 {
		t.Fatal("no requests captured")
	}

	// Compaction fires at a turn boundary, so the request actually SENT to the
	// client that turn must carry fewer tool-result bytes than the same history
	// would append-only. Find the largest tool-result payload sent across the run
	// and confirm it is below the naive ceiling (all five 12 KB blobs), and that
	// at least one request carried a stub.
	naiveCeiling := len(blob) * 5
	maxSent := 0
	stubbedAny := false
	for _, req := range reqs {
		if b := toolResultBytes(req); b > maxSent {
			maxSent = b
		}
		for _, m := range req.Messages {
			if m.Role == llm.RoleToolResult && strings.HasPrefix(m.Content, "[tool result pruned") {
				stubbedAny = true
			}
		}
	}
	if maxSent >= naiveCeiling {
		t.Errorf("largest request tool-result bytes = %d, expected < naive %d (compaction never shrank a request)", maxSent, naiveCeiling)
	}
	if !stubbedAny {
		t.Error("expected at least one pruned-stub tool result in some request")
	}
}

func TestRun_CompactionDisabledWhenBudgetZero(t *testing.T) {
	blob := strings.Repeat("x", 12000)
	fc := newFakeClient(
		toolResp("c1", "big", `{"v":"1"}`, 100, 10),
		toolResp("c2", "big", `{"v":"2"}`, 100, 10),
		toolResp("c3", "big", `{"v":"3"}`, 100, 10),
		textResp("done", 100, 10),
	)
	// HistoryTokenBudget defaults to 0 here => compaction OFF; history is pure
	// append-only.
	r := NewRunner(fc, []Tool{bigEchoTool{name: "big", blob: blob}}, "sys",
		WithLimits(Limits{MaxIterations: 20}))

	if _, err := r.Run(context.Background(), "task"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	fc.mu.Lock()
	reqs := append([]llm.Request(nil), fc.requests...)
	fc.mu.Unlock()
	last := reqs[len(reqs)-1]
	// All three full blobs must be present (no stubbing).
	for _, m := range last.Messages {
		if m.Role == llm.RoleToolResult && strings.HasPrefix(m.Content, "[tool result pruned") {
			t.Fatal("compaction fired despite HistoryTokenBudget=0")
		}
	}
	if got, want := toolResultBytes(last), len(blob)*3; got != want {
		t.Errorf("tool-result bytes = %d, want %d (full append-only history)", got, want)
	}
}

func TestRun_CompactionSingleShotPerCrossingThenRearms(t *testing.T) {
	// Verify the prefix stays STABLE across consecutive turns once an old result
	// is stubbed: a turn after compaction must not re-mutate the already-stubbed
	// message (which would re-cost a cache miss). We assert the oldest stub's
	// content is identical in every request that contains it.
	blob := strings.Repeat("q", 12000)
	steps := []scriptStep{}
	for i := 0; i < 8; i++ {
		steps = append(steps, toolResp("c"+string(rune('1'+i)), "big", `{"v":"x"}`, 100, 10))
	}
	steps = append(steps, textResp("done", 100, 10))
	fc := newFakeClient(steps...)

	r := NewRunner(fc, []Tool{bigEchoTool{name: "big", blob: blob}}, "sys",
		WithLimits(Limits{MaxIterations: 20, HistoryTokenBudget: 1500}))
	if _, err := r.Run(context.Background(), "task"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	fc.mu.Lock()
	reqs := append([]llm.Request(nil), fc.requests...)
	fc.mu.Unlock()

	// Find the first request whose message[2] (the first tool result) is a stub,
	// then assert it stays byte-identical in all later requests.
	var firstStub string
	for _, req := range reqs {
		if len(req.Messages) < 3 {
			continue
		}
		m := req.Messages[2]
		if m.Role != llm.RoleToolResult {
			continue
		}
		if strings.HasPrefix(m.Content, "[tool result pruned") {
			if firstStub == "" {
				firstStub = m.Content
			} else if m.Content != firstStub {
				t.Errorf("oldest stub mutated across turns: %q vs %q", firstStub, m.Content)
			}
		}
	}
	if firstStub == "" {
		t.Fatal("expected the oldest tool result to be stubbed at some point")
	}
}
