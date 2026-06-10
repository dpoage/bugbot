package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/llm"
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

	r := NewRunner(fc, []Tool{grep, readFile, listDir}, "You are a code comprehension agent.")
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
		case EventRequest:
			nReq++
		case EventAssistant:
			nAsst++
		case EventToolResult:
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
	replay, err := NewReplayClient(out.Transcript, llm.Capabilities{})
	if err != nil {
		t.Fatalf("NewReplayClient: %v", err)
	}
	r2 := NewRunner(replay, []Tool{grep, readFile, listDir}, "ignored")
	out2, err := r2.Run(context.Background(), "Where is foo defined and what does it return?")
	if err != nil {
		t.Fatalf("replay Run: %v", err)
	}
	if out2.FinalText != out.FinalText {
		t.Errorf("replay FinalText = %q, want %q", out2.FinalText, out.FinalText)
	}
}

func TestRun_AutosaveTranscriptDir(t *testing.T) {
	dir := t.TempDir()
	fc := newFakeClient(textResp("done", 1, 1))
	r := NewRunner(fc, nil, "sys", WithTranscriptDir(dir))

	if _, err := r.Run(context.Background(), "My Task!"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 transcript file, got %d", len(entries))
	}
	name := entries[0].Name()
	if !strings.HasSuffix(name, ".jsonl") {
		t.Errorf("transcript name = %q, want .jsonl suffix", name)
	}
	if !strings.Contains(name, "my-task") {
		t.Errorf("transcript name = %q, want slug 'my-task'", name)
	}

	// The saved file must be a loadable transcript.
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()
	tr, err := LoadJSONL(f)
	if err != nil {
		t.Fatalf("LoadJSONL: %v", err)
	}
	if len(tr.Events) == 0 {
		t.Error("saved transcript is empty")
	}
}

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"Hello World":            "hello-world",
		"  trim--me  ":           "trim-me",
		"":                       "run",
		"!!!":                    "run",
		strings.Repeat("a", 100): strings.Repeat("a", 48),
	}
	for in, want := range cases {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
}
