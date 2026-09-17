package agenttools

import (
	"context"
	"strings"
	"testing"

	llmkit "github.com/dpoage/llmkit"
	"github.com/dpoage/llmkit/agent"
)

// TestEndToEnd_GrepReadAnswer scripts a fake model that drives the real
// read-only tools against a fixture tree: it greps for a symbol, reads the file
// the grep points at, then answers. It asserts the tools actually executed
// against the fixture and that the transcript captured every step.
func TestEndToEnd_GrepReadAnswer(t *testing.T) {
	root := fixtureTree(t)

	grep, err := NewGrep(root)
	if err != nil {
		t.Fatalf("NewGrep: %v", err)
	}
	readFile, err := NewReadFile(root)
	if err != nil {
		t.Fatalf("NewReadFile: %v", err)
	}
	listDir, err := NewListDir(root)
	if err != nil {
		t.Fatalf("NewListDir: %v", err)
	}

	// Scripted plan:
	//   step 1: grep for "func foo"
	//   step 2: read pkg/util.go
	//   step 3: final answer naming the return value
	fc := newFakeClient(
		toolResp("g1", "grep", `{"pattern":"func foo"}`, 20, 10),
		toolResp("r1", "read_file", `{"path":"pkg/util.go"}`, 30, 10),
		textResp("foo is defined in pkg/util.go and returns 42", 25, 12),
	)

	r := agent.NewRunner(fc, []agent.Tool{grep, readFile, listDir}, "You are a code comprehension agent.")
	out, err := r.Run(context.Background(), "Where is foo defined and what does it return?")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if out.Truncated {
		t.Errorf("unexpected truncation: %s", out.TruncationReason)
	}
	if !strings.Contains(out.FinalText, "42") {
		t.Errorf("FinalText = %q, expected to mention 42", out.FinalText)
	}
	if out.Iterations != 3 {
		t.Errorf("Iterations = %d, want 3", out.Iterations)
	}

	// Verify the tools actually ran against the fixture by inspecting the tool
	// results captured in the transcript.
	var grepResult, readResult string
	var nReq, nAsst, nTool int
	for _, ev := range out.Transcript.Events {
		switch ev.Kind {
		case agent.EventRequest:
			nReq++
		case agent.EventAssistant:
			nAsst++
		case agent.EventToolResult:
			nTool++
			switch ev.ToolName {
			case "grep":
				grepResult = ev.Result
			case "read_file":
				readResult = ev.Result
			}
		}
	}

	// grep must have located the definition in the real fixture file.
	if !strings.Contains(grepResult, "pkg/util.go:") {
		t.Errorf("grep result did not hit the fixture file:\n%s", grepResult)
	}
	// read_file must have returned the real numbered source with the return value.
	if !strings.Contains(readResult, "return 42") {
		t.Errorf("read_file result missing fixture content:\n%s", readResult)
	}

	// Transcript captured every step: 3 requests, 3 assistant turns, 2 tool
	// results.
	if nReq != 3 || nAsst != 3 || nTool != 2 {
		t.Errorf("transcript step counts: req=%d asst=%d tool=%d, want 3/3/2", nReq, nAsst, nTool)
	}

	// Usage accumulated across all three turns.
	wantIn := int64(20 + 30 + 25)
	wantOut := int64(10 + 10 + 12)
	if out.Usage.InputTokens != wantIn || out.Usage.OutputTokens != wantOut {
		t.Errorf("Usage = %+v, want {%d %d}", out.Usage, wantIn, wantOut)
	}

	// The transcript must round-trip and replay deterministically.
	replay, err := agent.NewReplayClient(out.Transcript, llmkit.Capabilities{})
	if err != nil {
		t.Fatalf("NewReplayClient: %v", err)
	}
	r2 := agent.NewRunner(replay, []agent.Tool{grep, readFile, listDir}, "ignored")
	out2, err := r2.Run(context.Background(), "Where is foo defined and what does it return?")
	if err != nil {
		t.Fatalf("replay Run: %v", err)
	}
	if out2.FinalText != out.FinalText {
		t.Errorf("replay FinalText = %q, want %q", out2.FinalText, out.FinalText)
	}
}
