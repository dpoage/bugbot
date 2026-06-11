package agent

import (
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/llm"
)

// bigToolResult builds a tool-result message large enough to be worth pruning.
func bigToolResult(id, content string) llm.Message {
	return llm.Message{Role: llm.RoleToolResult, ToolCallID: id, Content: content}
}

// sampleHistory builds a realistic finder history: a task, then alternating
// assistant turns (with tool calls) and tool results. n is the number of
// tool-result messages; each is a long blob so it is a pruning candidate.
func sampleHistory(n int) ([]llm.Message, map[string]string) {
	msgs := []llm.Message{{Role: llm.RoleUser, Content: "investigate these files for bugs"}}
	names := map[string]string{}
	for i := 0; i < n; i++ {
		id := "call-" + string(rune('a'+i))
		name := "read_file"
		names[id] = name
		msgs = append(msgs,
			llm.Message{Role: llm.RoleAssistant, Content: "let me read file " + id,
				ToolCalls: []llm.ToolCall{{ID: id, Name: name, Arguments: []byte(`{"path":"x.go"}`)}}},
			bigToolResult(id, strings.Repeat("line of source code\n", 200)),
		)
	}
	return msgs, names
}

func TestCompactHistory_PrunesOldKeepsRecentK(t *testing.T) {
	msgs, names := sampleHistory(6) // 6 tool results
	recentK := 4

	out, pruned := compactHistory(msgs, recentK, names)
	if !pruned {
		t.Fatal("expected pruning to occur")
	}

	// Walk tool-result messages; the first (6-4)=2 must be stubs, the last 4 verbatim.
	var trIdx int
	for _, m := range out {
		if m.Role != llm.RoleToolResult {
			continue
		}
		isStub := strings.HasPrefix(m.Content, "[tool result pruned")
		if trIdx < 2 && !isStub {
			t.Errorf("tool result %d should be a stub, got %q", trIdx, m.Content)
		}
		if trIdx >= 2 && isStub {
			t.Errorf("tool result %d (within recent-%d) should be verbatim, got stub", trIdx, recentK)
		}
		trIdx++
	}
}

func TestCompactHistory_PreservesPairingAndStructure(t *testing.T) {
	msgs, names := sampleHistory(5)
	orig := append([]llm.Message(nil), msgs...)

	out, pruned := compactHistory(msgs, 1, names)
	if !pruned {
		t.Fatal("expected pruning")
	}
	if len(out) != len(orig) {
		t.Fatalf("message count changed: got %d want %d", len(out), len(orig))
	}
	for i := range orig {
		o, n := orig[i], out[i]
		// Role, ToolCallID, IsError, and assistant ToolCalls must be untouched: a
		// provider rejects a tool result whose ID no longer matches its call.
		if o.Role != n.Role {
			t.Errorf("msg %d role changed: %v -> %v", i, o.Role, n.Role)
		}
		if o.ToolCallID != n.ToolCallID {
			t.Errorf("msg %d ToolCallID changed: %q -> %q", i, o.ToolCallID, n.ToolCallID)
		}
		if o.IsError != n.IsError {
			t.Errorf("msg %d IsError changed", i)
		}
		if len(o.ToolCalls) != len(n.ToolCalls) {
			t.Errorf("msg %d ToolCalls count changed", i)
		}
	}
	// The task message (index 0) and every assistant turn must keep their content.
	if out[0].Content != orig[0].Content {
		t.Errorf("task message content mutated: %q", out[0].Content)
	}
	for i := range out {
		if out[i].Role == llm.RoleAssistant && out[i].Content != orig[i].Content {
			t.Errorf("assistant turn %d content mutated (reasoning chain lost)", i)
		}
	}
}

func TestCompactHistory_StubFormat(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: "task"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "c1", Name: "read_file"}}},
		bigToolResult("c1", strings.Repeat("x", 8412)),
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "c2", Name: "grep"}}},
		{Role: llm.RoleToolResult, ToolCallID: "c2", Content: strings.Repeat("y", 500), IsError: true},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "c3", Name: "list_dir"}}},
		bigToolResult("c3", strings.Repeat("z", 100)),
	}
	names := map[string]string{"c1": "read_file", "c2": "grep", "c3": "list_dir"}

	out, pruned := compactHistory(msgs, 1, names) // keep only the last result (c3)
	if !pruned {
		t.Fatal("expected pruning")
	}
	if got := out[2].Content; got != "[tool result pruned to save context: read_file, 8412 bytes]" {
		t.Errorf("c1 stub = %q", got)
	}
	if got := out[4].Content; got != "[errored tool result pruned to save context: grep, 500 bytes]" {
		t.Errorf("c2 errored stub = %q", got)
	}
	// c3 is within recent-K=1, stays verbatim.
	if out[6].Content != strings.Repeat("z", 100) {
		t.Error("c3 (recent) should be untouched")
	}
}

func TestCompactHistory_NothingToPruneWhenAllRecent(t *testing.T) {
	msgs, names := sampleHistory(3)
	// recentK >= number of results: nothing falls outside the window.
	out, pruned := compactHistory(msgs, 3, names)
	if pruned {
		t.Error("expected no pruning when all results are within recent-K")
	}
	// Same backing slice returned unchanged (append-only prefix preserved).
	if &out[0] != &msgs[0] {
		t.Error("expected the original slice to be returned untouched")
	}
}

func TestCompactHistory_SkipsAlreadyStubbed(t *testing.T) {
	// A result already at/under stub size must not be re-mutated (that would
	// needlessly invalidate the prompt-cache prefix on a later compaction).
	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: "task"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "c1", Name: "read_file"}}},
		{Role: llm.RoleToolResult, ToolCallID: "c1", Content: "tiny"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "c2", Name: "read_file"}}},
		bigToolResult("c2", strings.Repeat("x", 4000)),
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "c3", Name: "read_file"}}},
		bigToolResult("c3", strings.Repeat("y", 4000)),
	}
	names := map[string]string{"c1": "read_file", "c2": "read_file", "c3": "read_file"}

	out, pruned := compactHistory(msgs, 1, names)
	if !pruned {
		t.Fatal("expected the large c2 result to be pruned")
	}
	// c1 ("tiny") is shorter than any stub, so it stays as-is.
	if out[2].Content != "tiny" {
		t.Errorf("tiny result should be left alone, got %q", out[2].Content)
	}
}

func TestEstimateTokens_CountsContentAndToolCalls(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: strings.Repeat("a", 400)}, // 400 bytes
		{Role: llm.RoleAssistant, Content: strings.Repeat("b", 400),
			ToolCalls: []llm.ToolCall{{Name: "read_file", Arguments: []byte(strings.Repeat("c", 192))}}},
	}
	// (400 + 400 + len("read_file")=9 + 192) / 4 = 1001/4 = 250
	if got := estimateTokens(msgs); got != 250 {
		t.Errorf("estimateTokens = %d, want 250", got)
	}
}
