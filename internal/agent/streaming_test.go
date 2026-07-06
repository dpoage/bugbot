package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/llm"
)

// hookClient wraps a fakeClient and calls onComplete after every Complete
// call returns, letting a test observe runner-internal state (like the
// transcript file on disk) mid-run, before the run itself finishes.
type hookClient struct {
	*fakeClient
	onComplete func()
}

func (h *hookClient) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	resp, err := h.fakeClient.Complete(ctx, req)
	if h.onComplete != nil {
		h.onComplete()
	}
	return resp, err
}

// TestTranscript_StreamsIncrementally verifies WithTranscriptDir writes each
// transcript event to disk AS IT IS RECORDED rather than only after the run
// completes: a hook fired from inside the first Complete call (i.e. while the
// run is still in its tool-execution turn) must already find a valid,
// line-by-line-parseable JSONL prefix on disk (at least the first request
// event), proving a stuck run can be tailed live.
func TestTranscript_StreamsIncrementally(t *testing.T) {
	dir := t.TempDir()

	var midRunLines int
	var midRunErr error
	fc := newFakeClient(
		toolResp("call-1", "read_file", `{"path":"a.go"}`, 5, 5),
		textResp("done", 5, 5),
	)
	hooked := &hookClient{fakeClient: fc, onComplete: func() {
		if midRunErr != nil || midRunLines != 0 {
			return // only capture the FIRST completion's mid-run state
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) == 0 {
			midRunErr = err
			return
		}
		data, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
		if err != nil {
			midRunErr = err
			return
		}
		lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		// Each line must be independently loadable as a transcript (proves the
		// file is valid JSONL up to this point, not a half-written blob).
		for _, ln := range lines {
			if ln == "" {
				continue
			}
			if _, lerr := LoadJSONL(strings.NewReader(ln)); lerr != nil {
				midRunErr = lerr
				return
			}
			midRunLines++
		}
	}}

	tools := []Tool{&stubReadFileTool{}}
	r := NewRunner(hooked, tools, "sys", WithTranscriptDir(dir))

	if _, err := r.Run(context.Background(), "streaming task"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if midRunErr != nil {
		t.Fatalf("mid-run transcript read failed: %v", midRunErr)
	}
	// The first Complete's onComplete fires after recordRequest (before the
	// call) and recordAssistant (after the call, inside complete()) have both
	// already streamed — so at least the request event for step 1 must be on
	// disk already, before the run has executed the tool or taken its second
	// turn.
	if midRunLines == 0 {
		t.Fatal("expected at least one streamed transcript line visible mid-run, got 0")
	}

	// After the run finishes, the file must still be a fully valid, complete
	// transcript (streaming didn't corrupt or truncate anything).
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("ReadDir: entries=%v err=%v", entries, err)
	}
	f, err := os.Open(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()
	tr, err := LoadJSONL(f)
	if err != nil {
		t.Fatalf("LoadJSONL: %v", err)
	}
	// request+assistant (turn 1) + tool_result + request+assistant (turn 2) = 5.
	if len(tr.Events) != 5 {
		t.Errorf("Events = %d, want 5 (2 requests, 2 assistants, 1 tool_result)", len(tr.Events))
	}
}

// stubReadFileTool is a minimal Tool implementation for TestTranscript_StreamsIncrementally.
type stubReadFileTool struct{}

func (stubReadFileTool) Def() llm.ToolDef {
	return llm.ToolDef{Name: "read_file", Description: "reads a file"}
}

func (stubReadFileTool) Run(ctx context.Context, args json.RawMessage) (string, error) {
	return "file contents", nil
}
