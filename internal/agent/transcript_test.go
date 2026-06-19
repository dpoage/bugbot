package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/llm"
)

func TestTranscript_RoundTrip(t *testing.T) {
	fc := newFakeClient(
		toolResp("c1", "echo", `{"v":"hi"}`, 10, 4),
		textResp("answer", 8, 3),
	)
	r := NewRunner(fc, []Tool{echoTool{name: "echo"}}, "sys")
	out, err := r.Run(context.Background(), "task")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var buf bytes.Buffer
	if err := out.Transcript.SaveJSONL(&buf); err != nil {
		t.Fatalf("SaveJSONL: %v", err)
	}

	loaded, err := LoadJSONL(&buf)
	if err != nil {
		t.Fatalf("LoadJSONL: %v", err)
	}
	if len(loaded.Events) != len(out.Transcript.Events) {
		t.Fatalf("event count: loaded %d, original %d", len(loaded.Events), len(out.Transcript.Events))
	}
	// Spot-check kinds and key fields survived.
	var sawToolResult, sawAssistant bool
	for _, ev := range loaded.Events {
		switch ev.Kind {
		case EventToolResult:
			sawToolResult = true
			if ev.ToolName != "echo" || ev.ToolCallID != "c1" {
				t.Errorf("tool result event lost fields: %+v", ev)
			}
		case EventAssistant:
			sawAssistant = true
		}
	}
	if !sawToolResult || !sawAssistant {
		t.Error("round-trip lost event kinds")
	}
}

func TestReplayClient_ReproducesRun(t *testing.T) {
	// Record a run.
	fc := newFakeClient(
		toolResp("c1", "echo", `{"v":"hi"}`, 10, 4),
		textResp("recorded answer", 8, 3),
	)
	r := NewRunner(fc, []Tool{echoTool{name: "echo"}}, "sys")
	rec, err := r.Run(context.Background(), "task")
	if err != nil {
		t.Fatalf("record Run: %v", err)
	}

	// Replay it through a fresh runner with the same tools.
	replay, err := NewReplayClient(rec.Transcript, llm.Capabilities{})
	if err != nil {
		t.Fatalf("NewReplayClient: %v", err)
	}
	r2 := NewRunner(replay, []Tool{echoTool{name: "echo"}}, "sys")
	out, err := r2.Run(context.Background(), "task")
	if err != nil {
		t.Fatalf("replay Run: %v", err)
	}
	if out.FinalText != "recorded answer" {
		t.Errorf("replay FinalText = %q", out.FinalText)
	}
	if out.Iterations != 2 {
		t.Errorf("replay Iterations = %d, want 2", out.Iterations)
	}
}

func TestReplayClient_Divergence(t *testing.T) {
	// Record a run that calls a tool then finishes.
	fc := newFakeClient(
		toolResp("c1", "echo", `{"v":"hi"}`, 1, 1),
		textResp("done", 1, 1),
	)
	r := NewRunner(fc, []Tool{echoTool{name: "echo"}}, "sys")
	rec, _ := r.Run(context.Background(), "task")

	replay, err := NewReplayClient(rec.Transcript, llm.Capabilities{})
	if err != nil {
		t.Fatalf("NewReplayClient: %v", err)
	}
	// Replay against a runner with NO tools: the recorded tool call will produce
	// an "unknown tool" error result, but the tool-result *ID* still matches, so
	// the structure check passes — this is the lenient design. To force a real
	// divergence we exhaust the responses by looping more than recorded.
	r2 := NewRunner(replay, []Tool{echoTool{name: "echo"}}, "sys")
	if _, err := r2.Run(context.Background(), "task"); err != nil {
		t.Fatalf("first replay should succeed: %v", err)
	}
	// A second run reuses the same exhausted client -> divergence/exhaustion.
	_, err = r2.Run(context.Background(), "task")
	if err == nil {
		t.Fatal("expected exhaustion error on replay reuse")
	}
}

func TestReplayClient_FromResponses(t *testing.T) {
	replay := NewReplayClientFromResponses([]llm.Response{
		{Text: "one", StopReason: llm.StopEndTurn},
	}, llm.Capabilities{ContextWindow: 1234})
	if replay.Capabilities().ContextWindow != 1234 {
		t.Error("capabilities not threaded through")
	}
	resp, err := replay.Complete(context.Background(), llm.Request{})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "one" {
		t.Errorf("Text = %q", resp.Text)
	}
	if _, err := replay.Complete(context.Background(), llm.Request{}); err == nil {
		t.Error("expected exhaustion on second call")
	}
}

// flakyWriter records every byte it accepts and returns an error once a
// caller-supplied threshold is reached. The error is sticky: every subsequent
// Write returns (0, err) without consuming any bytes. This lets the test
// stage "the disk filled up after K bytes" and observe what SaveJSONL does
// with the partial progress.
type flakyWriter struct {
	written  bytes.Buffer
	errAfter int
	err      error
}

func (f *flakyWriter) Write(p []byte) (int, error) {
	remaining := f.errAfter - f.written.Len()
	if remaining <= 0 {
		return 0, f.err
	}
	if len(p) <= remaining {
		f.written.Write(p)
		return len(p), nil
	}
	f.written.Write(p[:remaining])
	return remaining, f.err
}

// TestSaveJSONL_FlushesPrefixOnEncodeError verifies that when the underlying
// writer errors mid-encoding, SaveJSONL still best-effort Flushes the already
// encoded bytes to the writer. Without the best-effort flush, those bytes
// would remain stuck in the bufio buffer and a downstream LoadJSONL would
// see an empty file.
func TestSaveJSONL_FlushesPrefixOnEncodeError(t *testing.T) {
	// Build a transcript whose events are large enough to be individually
	// recognizable on reload, but not so large that the bufio default
	// buffer swallows everything in a single write.
	tr := NewTranscript()
	const n = 5
	payload := "this is a longish result payload so each event encodes to more than 100 bytes"
	for i := 0; i < n; i++ {
		tr.Events = append(tr.Events, Event{
			Kind:       EventToolResult,
			Step:       i + 1,
			ToolName:   "echo",
			ToolCallID: "c1",
			Result:     payload,
		})
	}

	// Use a bufio.Writer with a 64-byte buffer so writes to the underlying
	// flaky writer happen frequently and we can stage the failure at a
	// known event boundary.
	bw := bufio.NewWriterSize(&flakyBuffer{flakyWriter: flakyWriter{
		errAfter: 0, // will be set after we know the first event size
		err:      errors.New("disk full"),
	}}, 64)

	// First pass: write to a counting buffer to find the byte boundary
	// after exactly k complete events.
	var probe bytes.Buffer
	probeBW := bufio.NewWriter(&probe)
	probeEnc := json.NewEncoder(probeBW)
	for i := range tr.Events {
		if err := probeEnc.Encode(&tr.Events[i]); err != nil {
			t.Fatalf("probe encode %d: %v", i, err)
		}
	}
	if err := probeBW.Flush(); err != nil {
		t.Fatalf("probe flush: %v", err)
	}
	lines := bytes.Split(probe.Bytes(), []byte("\n"))
	// lines has a trailing "" from the final \n.
	if len(lines) < 3 {
		t.Fatalf("probe produced too few lines: %d", len(lines))
	}
	// errAfter sits exactly after the second event's terminating newline
	// (so two complete events are persisted before the writer errors).
	boundary := 0
	complete := 0
	for i, l := range lines {
		if len(l) == 0 {
			continue
		}
		complete++
		boundary += len(l) + 1 // +1 for the newline
		if complete == 2 {
			break
		}
		_ = i
	}
	if complete < 2 {
		t.Fatalf("could not stage 2 complete events: complete=%d", complete)
	}

	// Now use that boundary in the real (failing) write.
	fw := &flakyWriter{errAfter: boundary, err: errors.New("disk full")}
	saveErr := tr.SaveJSONL(fw)
	if saveErr == nil {
		t.Fatal("expected SaveJSONL to surface the underlying writer error, got nil")
	}
	if !strings.Contains(saveErr.Error(), "disk full") {
		t.Errorf("SaveJSONL error = %v, want containing 'disk full'", saveErr)
	}

	// The best-effort Flush must have pushed the encoded prefix to fw
	// before SaveJSONL returned. The exact byte count may differ by a
	// flush boundary's worth, but we expect AT LEAST `boundary` bytes
	// (the bytes that were already in the bufio buffer when the error
	// surfaced from the underlying writer).
	if fw.written.Len() < boundary {
		t.Errorf("flakyWriter received %d bytes, want >= %d (the staged boundary)",
			fw.written.Len(), boundary)
	}

	// LoadJSONL must recover the events that fit in the persisted prefix.
	loaded, err := LoadJSONL(&fw.written)
	if err != nil {
		t.Fatalf("LoadJSONL on the persisted prefix: %v", err)
	}
	if len(loaded.Events) < 1 {
		t.Fatalf("LoadJSONL recovered %d events from a %d-byte prefix; expected at least 1",
			len(loaded.Events), fw.written.Len())
	}
	// Every recovered event must match the originals (same kind, step,
	// tool name, payload).
	for i, ev := range loaded.Events {
		if ev.Kind != EventToolResult {
			t.Errorf("recovered event %d: kind = %q, want %q", i, ev.Kind, EventToolResult)
		}
		if ev.Step < 1 || ev.Step > n {
			t.Errorf("recovered event %d: step = %d, want in [1,%d]", i, ev.Step, n)
		}
		if ev.ToolName != "echo" {
			t.Errorf("recovered event %d: tool = %q, want %q", i, ev.ToolName, "echo")
		}
		if ev.Result != payload {
			t.Errorf("recovered event %d: result = %q, want %q", i, ev.Result, payload)
		}
	}
	_ = bw
}

// flakyBuffer embeds flakyWriter so we can pass it through bufio.NewWriterSize
// in the failure test (kept distinct so the struct stays an io.Writer for
// bufio without conflicting with the test's direct use of the field).
type flakyBuffer struct {
	flakyWriter
}
