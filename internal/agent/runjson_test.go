package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dpoage/bugbot/internal/llm"
)

type finding struct {
	File    string `json:"file"`
	Message string `json:"message"`
}

// findingWithRefuted is the production finding shape — it has a boolean
// "refuted" field that the historical finding struct omits. The native
// schema tests use this struct so the schema's required=["file","message",
// "refuted"] check actually exercises the missing-required-field branch
// of validateSchema.
type findingWithRefuted struct {
	File    string `json:"file"`
	Message string `json:"message"`
	Refuted bool   `json:"refuted"`
}

func TestRunJSON_DirectParse(t *testing.T) {
	fc := newFakeClient(textResp(`{"file":"a.go","message":"bug"}`, 5, 5))
	r := NewRunner(fc, nil, "sys")

	var got finding
	out, err := r.RunJSON(context.Background(), "find a bug", nil, &got)
	if err != nil {
		t.Fatalf("RunJSON: %v", err)
	}
	if got.File != "a.go" || got.Message != "bug" {
		t.Errorf("parsed = %+v", got)
	}
	if out.Iterations != 1 {
		t.Errorf("Iterations = %d, want 1 (no repair)", out.Iterations)
	}
}

// TestRunJSONContinue_PreservesPriorConversation is the core continuation
// contract (bugbot-z6ay): a second RunJSONContinue call must NOT reseed the
// conversation — it must append its task onto the FULL history of the prior
// round (including round 1's tool-driven investigation), not just round 1's
// final answer. This is what lets a revision round's feedback land in the
// same conversation the model already investigated in, instead of asking it
// to re-orient from scratch.
func TestRunJSONContinue_PreservesPriorConversation(t *testing.T) {
	fc := newFakeClient(
		toolResp("c1", "echo", `{"v":"orient"}`, 10, 4),
		textResp(`{"file":"a.go","message":"round1"}`, 8, 3),
		textResp(`{"file":"a.go","message":"round2"}`, 8, 3),
	)
	tools := []Tool{echoTool{name: "echo"}}
	r := NewRunner(fc, tools, "sys")

	var got1 finding
	out1, err := r.RunJSON(context.Background(), "investigate and report", nil, &got1)
	if err != nil {
		t.Fatalf("round1 RunJSON: %v", err)
	}
	if len(fc.requests) != 2 {
		t.Fatalf("round1 issued %d requests, want 2 (orient + final)", len(fc.requests))
	}
	// Round 1's Outcome must carry the full conversation (seed + tool call +
	// tool result + final assistant answer) for RunJSONContinue to build on.
	if want := 4; len(out1.Messages) != want {
		t.Fatalf("round1 Outcome.Messages = %d entries, want %d (user, assistant tool-call, tool-result, assistant final)", len(out1.Messages), want)
	}

	var got2 finding
	out2, err := r.RunJSONContinue(context.Background(), out1, "feedback: fix it", nil, &got2)
	if err != nil {
		t.Fatalf("round2 RunJSONContinue: %v", err)
	}
	// Exactly one more completion was needed: continuation means the model
	// did NOT re-issue the orientation tool call it already made in round 1.
	if len(fc.requests) != 3 {
		t.Fatalf("round2 issued %d total requests, want 3 (round1's 2 plus round2's 1) -- extra requests mean round 2 re-investigated instead of continuing", len(fc.requests))
	}
	round2Req := fc.requests[2]
	if len(round2Req.Messages) != len(out1.Messages)+1 {
		t.Fatalf("round2 request has %d messages, want %d (round1's full history plus round2's new task turn)", len(round2Req.Messages), len(out1.Messages)+1)
	}
	// Round 2's request must carry round 1's messages verbatim as a prefix --
	// specifically the tool call and tool result, proving the model literally
	// saw its own prior investigation rather than a reseeded conversation.
	for i, want := range out1.Messages {
		got := round2Req.Messages[i]
		if got.Role != want.Role || got.Content != want.Content || got.ToolCallID != want.ToolCallID {
			t.Errorf("round2 request message %d = %+v, want round1 history entry %+v", i, got, want)
		}
	}
	sawToolCall, sawToolResult := false, false
	for _, m := range round2Req.Messages {
		if m.Role == llm.RoleAssistant && len(m.ToolCalls) > 0 {
			sawToolCall = true
		}
		if m.Role == llm.RoleToolResult {
			sawToolResult = true
		}
	}
	if !sawToolCall || !sawToolResult {
		t.Errorf("round2 request missing round1's investigation: sawToolCall=%v sawToolResult=%v", sawToolCall, sawToolResult)
	}
	if got2.Message != "round2" {
		t.Errorf("round2 parsed = %+v, want message=round2", got2)
	}
	if out2 == nil {
		t.Fatal("round2 Outcome is nil")
	}
}

// TestRunJSONContinue_NilPrevDegradesToReseed verifies that a nil prev
// Outcome is a safe no-op fallback to plain reseeding, so a caller need not
// special-case round 1 with a nil check before calling RunJSONContinue.
func TestRunJSONContinue_NilPrevDegradesToReseed(t *testing.T) {
	fc := newFakeClient(textResp(`{"file":"a.go","message":"bug"}`, 5, 5))
	r := NewRunner(fc, nil, "sys")

	var got finding
	if _, err := r.RunJSONContinue(context.Background(), nil, "find a bug", nil, &got); err != nil {
		t.Fatalf("RunJSONContinue with nil prev: %v", err)
	}
	if len(fc.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(fc.requests))
	}
	if len(fc.requests[0].Messages) != 1 {
		t.Errorf("request messages = %d, want 1 (reseeded, no prior history)", len(fc.requests[0].Messages))
	}
	if got.File != "a.go" {
		t.Errorf("parsed = %+v", got)
	}
}

func TestRunJSON_StripsMarkdownFences(t *testing.T) {
	fc := newFakeClient(textResp("```json\n{\"file\":\"b.go\",\"message\":\"x\"}\n```", 5, 5))
	r := NewRunner(fc, nil, "sys")

	var got finding
	if _, err := r.RunJSON(context.Background(), "task", nil, &got); err != nil {
		t.Fatalf("RunJSON: %v", err)
	}
	if got.File != "b.go" {
		t.Errorf("parsed = %+v", got)
	}
}

func TestRunJSON_RepairSucceeds(t *testing.T) {
	// First answer is not JSON; repair round-trip returns valid JSON.
	fc := newFakeClient(
		textResp("here is the answer: not json at all", 5, 5),
		textResp(`{"file":"c.go","message":"fixed"}`, 5, 5),
	)
	r := NewRunner(fc, nil, "sys")

	var got finding
	out, err := r.RunJSON(context.Background(), "task", json.RawMessage(`{"type":"object"}`), &got)
	if err != nil {
		t.Fatalf("RunJSON should succeed after repair: %v", err)
	}
	if got.File != "c.go" {
		t.Errorf("parsed = %+v", got)
	}
	// Two completions happened (original + repair). The repair Run is fresh, so
	// the returned outcome's Iterations is 1, but two requests hit the client.
	if len(fc.requests) != 2 {
		t.Errorf("client calls = %d, want 2", len(fc.requests))
	}
	// The repair prompt must mention the parse failure.
	repairTask := fc.requests[1].Messages[0].Content
	if !strings.Contains(repairTask, "failed to parse") {
		t.Errorf("repair prompt missing parse-failure note:\n%s", repairTask)
	}
	_ = out
}

// TestRunJSON_RepairAppendsToStreamedTranscript is a regression test for the
// streamed-transcript / repair interaction. Historically closeStream nilled
// streamFile/streamEnc but left streamPath set while streamAppend reopened
// with os.Create (O_TRUNC) — so repair()'s completion wiped the main run's
// already-streamed events down to just the two repair events. The first fix
// disarmed streaming entirely at closeStream, which protected the file but
// made the repair round-trip INVISIBLE on disk: a repair that produced the
// final (possibly still-unparseable) answer left no trace in the JSONL,
// making failures like an hallucinated schema-violating repair output
// undiagnosable post-hoc (bugbot-9fac).
//
// The current contract: streamAppend opens with O_APPEND (never truncates)
// and closeStream keeps streamPath armed, so repair()'s request+assistant
// turns APPEND to the same on-disk file. This asserts: exactly one file on
// disk, holding the main run's events UNTRUNCATED and FIRST, followed by the
// repair's events — matching the in-memory Outcome.Transcript exactly.
func TestRunJSON_RepairAppendsToStreamedTranscript(t *testing.T) {
	dir := t.TempDir()
	fc := newFakeClient(
		textResp("here is the answer: not json at all", 5, 5),
		textResp(`{"file":"c.go","message":"fixed"}`, 5, 5),
	)
	r := NewRunner(fc, nil, "sys", WithTranscriptDir(dir))

	var got finding
	out, err := r.RunJSON(context.Background(), "task", json.RawMessage(`{"type":"object"}`), &got)
	if err != nil {
		t.Fatalf("RunJSON should succeed after repair: %v", err)
	}
	if got.File != "c.go" {
		t.Errorf("parsed = %+v", got)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 transcript file (repair must append, never create a second), got %d: %v", len(entries), entries)
	}

	f, err := os.Open(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()
	loaded, err := LoadJSONL(f)
	if err != nil {
		t.Fatalf("LoadJSONL: %v", err)
	}

	// Main run: request+assistant. Repair: request+assistant. All on disk,
	// in order — the main run's events first (no truncation), the repair's
	// appended after.
	if len(loaded.Events) != 4 {
		t.Fatalf("on-disk events = %d, want 4 (main run request+assistant, then repair request+assistant); on-disk=%+v",
			len(loaded.Events), loaded.Events)
	}
	mainIdx, repairIdx := -1, -1
	for i, ev := range loaded.Events {
		if ev.Kind == EventAssistant && ev.Text == "here is the answer: not json at all" {
			mainIdx = i
		}
		if ev.Kind == EventAssistant && ev.Text == `{"file":"c.go","message":"fixed"}` {
			repairIdx = i
		}
	}
	if mainIdx == -1 {
		t.Errorf("on-disk transcript missing the main run's assistant text (truncated by reopen?); got %+v", loaded.Events)
	}
	if repairIdx == -1 {
		t.Errorf("on-disk transcript missing the repair's assistant text (repair invisible on disk); got %+v", loaded.Events)
	}
	if mainIdx != -1 && repairIdx != -1 && mainIdx > repairIdx {
		t.Errorf("main-run assistant event at %d AFTER repair's at %d — append order violated", mainIdx, repairIdx)
	}

	// The in-memory Outcome.Transcript matches the on-disk picture.
	if len(out.Transcript.Events) != 4 {
		t.Errorf("in-memory Transcript.Events = %d, want 4 (main run + repair)", len(out.Transcript.Events))
	}
}

func TestRunJSON_RepairFails(t *testing.T) {
	fc := newFakeClient(
		textResp("garbage", 5, 5),
		textResp("still garbage", 5, 5),
	)
	r := NewRunner(fc, nil, "sys")

	var got finding
	_, err := r.RunJSON(context.Background(), "task", nil, &got)
	if err == nil {
		t.Fatal("expected error after failed repair")
	}
	if !strings.Contains(err.Error(), "after one repair") {
		t.Errorf("error = %v, want 'after one repair'", err)
	}
}

// TestRunJSON_ForcedFinalization proves that when a finder exhausts its
// iteration cap mid-investigation (always calling tools, never finishing),
// RunJSON's reserved finalization turn still recovers the JSON answer instead of
// failing on dangling exploration prose. This is the core fix for finders that
// hit MaxIterations on a large chunk.
func TestRunJSON_ForcedFinalization(t *testing.T) {
	const maxIter = 3
	steps := make([]scriptStep, 0, maxIter+1)
	// The model investigates every turn up to the cap, never producing an answer.
	for i := 0; i < maxIter; i++ {
		steps = append(steps, toolResp("c", "echo", `{"v":"x"}`, 1, 1))
	}
	// The reserved finalization turn: tools are dropped, and the model finally
	// emits the JSON.
	steps = append(steps, textResp(`{"file":"z.go","message":"found it"}`, 2, 2))

	fc := newFakeClient(steps...)
	r := NewRunner(fc, []Tool{echoTool{name: "echo"}}, "sys", WithLimits(Limits{MaxIterations: maxIter}))

	var got finding
	out, err := r.RunJSON(context.Background(), "audit", json.RawMessage(`{"type":"object"}`), &got)
	if err != nil {
		t.Fatalf("RunJSON should recover via finalization: %v", err)
	}
	if got.File != "z.go" || got.Message != "found it" {
		t.Errorf("parsed = %+v, want the finalization JSON", got)
	}
	if !out.Finalized {
		t.Error("Outcome.Finalized = false, want true (finalization turn should have fired)")
	}
	// The finalization request must carry NO tools so the model can only answer.
	finalReq := fc.requests[len(fc.requests)-1]
	if len(finalReq.Tools) != 0 {
		t.Errorf("finalization request carried %d tool(s), want 0", len(finalReq.Tools))
	}
	// The finalization user message must have been injected.
	lastMsg := finalReq.Messages[len(finalReq.Messages)-1]
	if lastMsg.Role != llm.RoleUser || !strings.Contains(lastMsg.Content, "STOP investigating") {
		t.Errorf("finalization message missing; last message = %+v", lastMsg)
	}
}

// TestRunJSON_ForcedFinalizationFiresOnce confirms finalization is attempted at
// most once: if the finalization turn itself does not produce JSON, RunJSON does
// not loop, but falls through to its single repair round-trip.
func TestRunJSON_ForcedFinalizationFiresOnce(t *testing.T) {
	const maxIter = 2
	steps := []scriptStep{
		toolResp("c", "echo", `{"v":"x"}`, 1, 1),
		toolResp("c", "echo", `{"v":"x"}`, 1, 1),
		// finalization turn: still not JSON.
		textResp("still just prose, sorry", 1, 1),
		// repair round-trip (a fresh run): now valid JSON.
		textResp(`{"file":"r.go","message":"repaired"}`, 1, 1),
	}
	fc := newFakeClient(steps...)
	r := NewRunner(fc, []Tool{echoTool{name: "echo"}}, "sys", WithLimits(Limits{MaxIterations: maxIter}))

	var got finding
	if _, err := r.RunJSON(context.Background(), "audit", nil, &got); err != nil {
		t.Fatalf("RunJSON: %v", err)
	}
	if got.File != "r.go" {
		t.Errorf("parsed = %+v, want repaired JSON", got)
	}
}

// TestRunJSON_MaxTokensContinuation proves the one-shot continuation retry
// stitches a JSON answer that was cut off at the output token cap back together
// so it parses, and surfaces the truncation distinctly when it still fails.
func TestRunJSON_MaxTokensContinuation(t *testing.T) {
	t.Run("continuation completes truncated JSON", func(t *testing.T) {
		fc := newFakeClient(
			maxTokensResp(`{"file":"a.go","mess`, 5, 5), // cut off mid-object
			textResp(`age":"done"}`, 5, 5),              // continuation finishes it
		)
		r := NewRunner(fc, nil, "sys")

		var got finding
		out, err := r.RunJSON(context.Background(), "task", nil, &got)
		if err != nil {
			t.Fatalf("RunJSON should stitch continuation: %v", err)
		}
		if got.File != "a.go" || got.Message != "done" {
			t.Errorf("parsed = %+v, want stitched JSON", got)
		}
		// Both completions must have happened within the same run.
		if len(fc.requests) != 2 {
			t.Errorf("client calls = %d, want 2 (initial + continuation)", len(fc.requests))
		}
		_ = out
	})

	t.Run("continuation that repeats the prefix is stitched without corruption", func(t *testing.T) {
		// The model ignores "continue from where you stopped" and restarts, repeating
		// the head of the first half before emitting the rest. A naive head+cont
		// concatenation would double `{"file":"a.go","mess` and break the JSON; the
		// stitch must trim the repeated prefix.
		fc := newFakeClient(
			maxTokensResp(`{"file":"a.go","mess`, 5, 5),        // cut off mid-object
			textResp(`{"file":"a.go","message":"done"}`, 5, 5), // restart: repeats prefix, then finishes
		)
		r := NewRunner(fc, nil, "sys")

		var got finding
		_, err := r.RunJSON(context.Background(), "task", nil, &got)
		if err != nil {
			t.Fatalf("RunJSON should stitch a repeated-prefix continuation: %v", err)
		}
		if got.File != "a.go" || got.Message != "done" {
			t.Errorf("parsed = %+v, want stitched JSON with the duplicated prefix trimmed", got)
		}
	})

	t.Run("truncation surfaced in error when unrecoverable", func(t *testing.T) {
		// Both the initial answer and its continuation stop at max_tokens and never
		// form valid JSON; after the repair round-trip also truncates, the error
		// must name the truncation.
		fc := newFakeClient(
			maxTokensResp(`{"file":"a.go"`, 5, 5),
			maxTokensResp(` ,"more`, 5, 5),
			// repair round-trip:
			maxTokensResp(`{"file":"a.go"`, 5, 5),
			maxTokensResp(` ,"more`, 5, 5),
		)
		r := NewRunner(fc, nil, "sys")

		var got finding
		_, err := r.RunJSON(context.Background(), "task", nil, &got)
		if err == nil {
			t.Fatal("expected error when JSON never completes")
		}
		if !strings.Contains(err.Error(), "truncated at the max-tokens cap") {
			t.Errorf("error = %v, want it to name the max-tokens truncation", err)
		}
	})
}

// TestRunJSON_StripsThinkBlocks proves the parse path tolerates reasoning-model
// think blocks WITHOUT spending a repair round-trip, on both the first attempt
// and the repair attempt, while leaving the raw transcript untouched.
func TestRunJSON_StripsThinkBlocks(t *testing.T) {
	t.Run("first attempt, think before json", func(t *testing.T) {
		raw := "<think>the cfg may be nil here</think>\n{\"file\":\"a.go\",\"message\":\"bug\"}"
		fc := newFakeClient(textResp(raw, 5, 5))
		r := NewRunner(fc, nil, "sys")

		var got finding
		out, err := r.RunJSON(context.Background(), "task", nil, &got)
		if err != nil {
			t.Fatalf("RunJSON: %v", err)
		}
		if got.File != "a.go" || got.Message != "bug" {
			t.Errorf("parsed = %+v", got)
		}
		if out.Iterations != 1 {
			t.Errorf("Iterations = %d, want 1 (no repair needed)", out.Iterations)
		}
		if len(fc.requests) != 1 {
			t.Errorf("client calls = %d, want 1 (think block must not trigger repair)", len(fc.requests))
		}
		// The transcript must preserve the RAW text (think block intact).
		if assistantText(out) != raw {
			t.Errorf("transcript assistant text = %q, want raw %q", assistantText(out), raw)
		}
	})

	t.Run("think plus fenced json", func(t *testing.T) {
		raw := "<think>reasoning</think>\n```json\n{\"file\":\"b.go\",\"message\":\"x\"}\n```"
		fc := newFakeClient(textResp(raw, 5, 5))
		r := NewRunner(fc, nil, "sys")

		var got finding
		if _, err := r.RunJSON(context.Background(), "task", nil, &got); err != nil {
			t.Fatalf("RunJSON: %v", err)
		}
		if got.File != "b.go" {
			t.Errorf("parsed = %+v", got)
		}
	})

	t.Run("repair attempt also strips think blocks", func(t *testing.T) {
		// First reply is unparseable; repair reply wraps valid JSON in a think
		// block. The repair must succeed, exercising stripping on the repair path.
		fc := newFakeClient(
			textResp("not json", 5, 5),
			textResp("<think>ok, valid json now</think>\n{\"file\":\"c.go\",\"message\":\"fixed\"}", 5, 5),
		)
		r := NewRunner(fc, nil, "sys")

		var got finding
		if _, err := r.RunJSON(context.Background(), "task", nil, &got); err != nil {
			t.Fatalf("RunJSON should succeed after repair: %v", err)
		}
		if got.File != "c.go" {
			t.Errorf("parsed = %+v", got)
		}
	})

	t.Run("literal think token inside json value survives", func(t *testing.T) {
		raw := `{"file":"d.go","message":"saw <think> in the source"}`
		fc := newFakeClient(textResp(raw, 5, 5))
		r := NewRunner(fc, nil, "sys")

		var got finding
		if _, err := r.RunJSON(context.Background(), "task", nil, &got); err != nil {
			t.Fatalf("RunJSON: %v", err)
		}
		if got.Message != "saw <think> in the source" {
			t.Errorf("message corrupted: %q", got.Message)
		}
	})
}

// TestRunJSON_MiniMaxM27Reasoning replays the real MiniMax-M2.7 response shape,
// confirmed live against https://api.minimax.io/v1 on 2026-06-17 (bugbot-ci0):
// reasoning is an inline <think>...</think> block at the START of
// message.content (there is NO separate reasoning_content field on the message),
// and crucially the think block itself can contain a ```go fence, with the real
// answer following in a ```json fence. parseInto (stripThinkBlocks then
// stripFences) must recover the JSON in one shot — no repair round-trip — even
// with a code fence nested inside the discarded reasoning.
func TestRunJSON_MiniMaxM27Reasoning(t *testing.T) {
	raw := "<think>\nThe function is named `add` but uses `-`:\n" +
		"```go\nfunc add(a, b int) int { return a - b }\n```\n" +
		"That is a logic error: the wrong operator is used.\n</think>\n\n" +
		"```json\n{\"file\":\"add.go\",\"message\":\"add() subtracts instead of adding\"}\n```"
	fc := newFakeClient(textResp(raw, 5, 5))
	r := NewRunner(fc, nil, "sys")

	var got finding
	out, err := r.RunJSON(context.Background(), "task", nil, &got)
	if err != nil {
		t.Fatalf("RunJSON on real MiniMax-M2.7 shape: %v", err)
	}
	if got.File != "add.go" || got.Message != "add() subtracts instead of adding" {
		t.Errorf("parsed = %+v, want the add.go finding", got)
	}
	// The whole point of agent-layer tolerance: a reasoning model's think block +
	// fenced answer must parse without burning the repair round-trip.
	if out.Iterations != 1 || len(fc.requests) != 1 {
		t.Errorf("M2.7 shape must parse without repair: iterations=%d calls=%d", out.Iterations, len(fc.requests))
	}
}

// assistantText returns the first EventAssistant text from the outcome's
// transcript, used to assert the raw model text is preserved unmodified.
func assistantText(out *Outcome) string {
	for _, ev := range out.Transcript.Events {
		if ev.Kind == EventAssistant {
			return ev.Text
		}
	}
	return ""
}

func TestStripFences(t *testing.T) {
	cases := map[string]string{
		"plain":              "plain",
		"```\nx\n```":        "x",
		"```json\n{}\n```":   "{}",
		"  ```\ny\n```  ":    "y",
		"no closing\n```\na": "no closing\n```\na", // not a leading fence -> trimmed only
	}
	for in, want := range cases {
		if got := stripFences(in); got != want {
			t.Errorf("stripFences(%q) = %q, want %q", in, got, want)
		}
	}
}

// budgetCutClient is a scripted llm.Client for budget-pressure tests: it
// always requests a tool (so the loop never naturally finishes) and reports
// a large, fixed Usage on every completion. The first N-1 completions also
// report a tool call, and the final one (the reserved finalization turn)
// returns a text answer. An optional chargeFn is called before each
// completion to model the shared pool spending.
type budgetCutClient struct {
	mu        sync.Mutex
	calls     int
	finalAt   int    // call index (1-based) at which to return text
	finalText string // text to return on the finalization turn
	perCall   int64
	chargeFn  func() // optional: called before each completion
}

func (c *budgetCutClient) Capabilities() llm.Capabilities { return llm.Capabilities{} }
func (c *budgetCutClient) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	if err := ctx.Err(); err != nil {
		return llm.Response{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.chargeFn != nil {
		c.chargeFn()
	}
	if c.calls == c.finalAt {
		return llm.Response{
			Text:       c.finalText,
			StopReason: llm.StopEndTurn,
			Usage:      llm.Usage{InputTokens: 1, OutputTokens: 1},
		}, nil
	}
	return llm.Response{
		StopReason: llm.StopToolUse,
		ToolCalls:  []llm.ToolCall{{ID: "c", Name: "echo", Arguments: []byte(`{}`)}},
		Usage:      llm.Usage{InputTokens: c.perCall, OutputTokens: c.perCall},
	}, nil
}

// TestRunJSON_BudgetPoolFinalizesAndParses proves that a RunJSON run whose
// shared pool BudgetCheck is exhausted now TAKES a finalization turn
// (outcome.Finalized==true) and, when the model emits valid JSON on that turn,
// RunJSON parses it successfully — no "empty model output" failure. This is
// the core fix for budget-pressured finders.
func TestRunJSON_BudgetPoolFinalizesAndParses(t *testing.T) {
	const maxIter = 10
	pool := NewBudgetPool(100) // tiny pool
	// finalAt = 3: first 2 calls request tools (charging pool), 3rd call is the
	// finalization turn after the pool is exhausted.
	c := &budgetCutClient{
		finalAt:   3,
		finalText: `{"file":"x.go","message":"recovered"}`,
		perCall:   60,
		chargeFn:  func() { pool.Add(60) },
	}
	r := NewRunner(c, []Tool{echoTool{name: "echo"}}, "sys", WithLimits(Limits{
		MaxIterations: maxIter,
		TokenBudget:   -1,
		BudgetCheck:   pool.Check,
	}))
	var got finding
	out, err := r.RunJSON(context.Background(), "audit", json.RawMessage(`{"type":"object"}`), &got)
	if err != nil {
		t.Fatalf("RunJSON should recover via budget-pressured finalization: %v", err)
	}
	if got.File != "x.go" || got.Message != "recovered" {
		t.Errorf("parsed = %+v, want the finalization JSON", got)
	}
	if !out.Finalized {
		t.Error("Outcome.Finalized = false, want true (budget pool stop should fire finalization)")
	}
	if out.TruncationReason != TruncBudgetPool {
		t.Errorf("TruncationReason = %q, want %q", out.TruncationReason, TruncBudgetPool)
	}
}

// TestRunJSON_PerRunTokenBudgetFinalizesAndParses is the per-run TokenBudget
// counterpart: a near-budget RunJSON run gets the reserved finalization turn
// and parses successfully when the model emits valid JSON.
func TestRunJSON_PerRunTokenBudgetFinalizesAndParses(t *testing.T) {
	const maxIter = 10
	const perRunBudget int64 = 100
	// 2 tool calls each spend 60 (cumulative 120 > 100), then on the 3rd
	// pre-turn check overBudget fires, finalization turn takes the 3rd call.
	c := &budgetCutClient{finalAt: 3, finalText: `{"file":"y.go","message":"ok"}`, perCall: 30}
	r := NewRunner(c, []Tool{echoTool{name: "echo"}}, "sys", WithLimits(Limits{
		MaxIterations: maxIter,
		TokenBudget:   perRunBudget,
	}))
	var got finding
	out, err := r.RunJSON(context.Background(), "audit", json.RawMessage(`{"type":"object"}`), &got)
	if err != nil {
		t.Fatalf("RunJSON should recover via per-run-budget finalization: %v", err)
	}
	if got.File != "y.go" {
		t.Errorf("parsed = %+v, want the finalization JSON", got)
	}
	if !out.Finalized {
		t.Error("Outcome.Finalized = false, want true (per-run budget stop should fire finalization)")
	}
	if out.TruncationReason != TruncTokenBudget {
		t.Errorf("TruncationReason = %q, want %q", out.TruncationReason, TruncTokenBudget)
	}
}

// TestRunJSON_BudgetFinalizeEmptyStillClassified covers the OR-clause of the
// bead: when the finalization turn itself yields no parseable JSON, the
// outcome is still cleanly classified as a budget stop (Truncated + budget
// reason), not a silently-empty result. The funnel's budgetStopped(outcome)
// must return true.
func TestRunJSON_BudgetFinalizeEmptyStillClassified(t *testing.T) {
	pool := NewBudgetPool(100)
	// finalization turn returns empty text — model fails to emit a useful answer.
	c := &budgetCutClient{
		finalAt:   3,
		finalText: "",
		perCall:   60,
		chargeFn:  func() { pool.Add(60) },
	}
	r := NewRunner(c, []Tool{echoTool{name: "echo"}}, "sys", WithLimits(Limits{
		MaxIterations: 10,
		TokenBudget:   -1,
		BudgetCheck:   pool.Check,
	}))
	var got finding
	_, err := r.RunJSON(context.Background(), "audit", json.RawMessage(`{"type":"object"}`), &got)
	if err == nil {
		t.Fatal("RunJSON should fail to parse empty finalization output")
	}
	// Now look at the outcome (returned alongside err per RunJSON contract).
	// We re-run and inspect via the Run path's outcome to check classification.
	// Simpler: re-check via a direct run + outcome check.
	c2 := &budgetCutClient{
		finalAt:   3,
		finalText: "",
		perCall:   60,
		chargeFn:  func() { pool.Add(60) },
	}
	r2 := NewRunner(c2, []Tool{echoTool{name: "echo"}}, "sys", WithLimits(Limits{
		MaxIterations: 10,
		TokenBudget:   -1,
		BudgetCheck:   pool.Check,
	}))
	out, _ := r2.run(context.Background(), nil, "audit", finalizationPrompt(json.RawMessage(`{"type":"object"}`)), nil)
	if !out.Truncated {
		t.Error("Outcome.Truncated = false, want true (budget stop should still mark truncated)")
	}
	if out.TruncationReason != TruncBudgetPool {
		t.Errorf("TruncationReason = %q, want %q (so funnel classifies as budget-stopped, not parse-failed)", out.TruncationReason, TruncBudgetPool)
	}
	if !out.Finalized {
		t.Error("Outcome.Finalized = false, want true (finalization turn was taken even if empty)")
	}
}

// TestRunJSON_RunPathNoExtraCall is the regression for the budget_test.go
// invariant: the public Run (finalizePrompt == "") must NOT pay an extra
// model call on a budget stop. The shared-pool overshoot bound
// (B + one in-flight call per runner) depends on this. We assert that a
// Run call into an exhausted pool issues exactly the same number of
// completions as before the fix.
func TestRunJSON_RunPathNoExtraCall(t *testing.T) {
	pool := NewBudgetPool(100)
	// bigSpendClient from budget_test.go: charges the pool, always requests a tool.
	// We import its behavior inline so this test is self-contained.
	c := &bigSpendClient{pool: pool, perCall: 60}
	r := NewRunner(c, []Tool{noopTool{}}, "sys", WithLimits(Limits{
		MaxIterations: -1,
		TokenBudget:   -1,
		BudgetCheck:   pool.Check,
	}))
	out, err := r.Run(context.Background(), "task")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.TruncationReason != TruncBudgetPool {
		t.Errorf("TruncationReason = %q, want %q", out.TruncationReason, TruncBudgetPool)
	}
	// The fix must not add an extra completion for Run (no finalizePrompt).
	// bigSpendClient charges 60 per call; pool is 100, so the pre-turn gate
	// fires on the 3rd call attempt (after 120 charged). The 2nd call may
	// have been charged, but the 3rd is gated before completion. Allow 1-2
	// calls — the precise count depends on charge order, but it MUST be < 3.
	if c.callCount.Load() > 2 {
		t.Errorf("Run (no finalize) made %d calls; the fix must NOT add an extra model call on the no-finalize path (want <= 2)", c.callCount.Load())
	}
}

// findWithCandidatesSchema is the JSON schema the bugbot finder/verifier
// historically uses for the "find a bug" answer shape. It exercises the
// "object root + required top-level fields" branch of validateSchema.
const findWithCandidatesSchema = `{
  "type": "object",
  "required": ["file", "message", "refuted"],
  "properties": {
    "file": {"type": "string"},
    "message": {"type": "string"},
    "refuted": {"type": "boolean"}
  }
}`

// validFindingJSON is one well-shaped answer to the schema above. Reused by
// the cap-on and validation-triggered-repair tests.
const validFindingJSON = `{"file":"a.go","message":"bug","refuted":false}`

// TestRunJSON_NoCapPassthrough asserts the agent-layer gate: when the client
// reports StructuredOutput==false, the wire request carries NO
// ResponseSchema, and behavior matches today's no-native-schema path (parse +
// parse-error → repair → after-one-repair error) exactly. This is the
// acceptance criterion: "RunJSON sends ResponseSchema only when
// StructuredOutput cap set".
func TestRunJSON_NoCapPassthrough(t *testing.T) {
	fc := newFakeClient(textResp(validFindingJSON, 5, 5))
	// caps is the zero value: StructuredOutput is false.
	r := NewRunner(fc, nil, "sys")

	var got findingWithRefuted
	out, err := r.RunJSON(context.Background(), "task", json.RawMessage(findWithCandidatesSchema), &got)
	if err != nil {
		t.Fatalf("RunJSON: %v", err)
	}
	if got.File != "a.go" {
		t.Errorf("parsed = %+v", got)
	}
	if len(fc.requests) != 1 {
		t.Errorf("client calls = %d, want 1 (no cap, no repair)", len(fc.requests))
	}
	// The single wire request MUST NOT carry a ResponseSchema. The agent gate
	// is supposed to drop the schema when the client can't honor it.
	if len(fc.requests[0].ResponseSchema) != 0 {
		t.Errorf("ResponseSchema on wire = %s, want empty (no-cap passthrough)", string(fc.requests[0].ResponseSchema))
	}
	_ = out
}

// TestRunJSON_CapOnCarriesSchema asserts that when the client reports
// StructuredOutput==true, every completion in a RunJSON run carries the
// ResponseSchema on the wire, so the adapter can apply grammar-constrained
// decoding. This is the "CAP ON" half of the acceptance criterion.
func TestRunJSON_CapOnCarriesSchema(t *testing.T) {
	fc := newFakeClient(textResp(validFindingJSON, 5, 5))
	fc.caps = llm.Capabilities{StructuredOutput: true}
	r := NewRunner(fc, nil, "sys")

	var got findingWithRefuted
	if _, err := r.RunJSON(context.Background(), "task", json.RawMessage(findWithCandidatesSchema), &got); err != nil {
		t.Fatalf("RunJSON: %v", err)
	}
	if got.File != "a.go" {
		t.Errorf("parsed = %+v", got)
	}
	if len(fc.requests) != 1 {
		t.Errorf("client calls = %d, want 1", len(fc.requests))
	}
	if got := string(fc.requests[0].ResponseSchema); got != findWithCandidatesSchema {
		t.Errorf("ResponseSchema on wire = %s, want the schema verbatim", got)
	}
}

// TestRunJSON_ValidationTriggersRepair asserts the core shape-validation
// contract: when the model's first answer is valid JSON but the WRONG SHAPE
// (here, a bare array where the schema requires an object), RunJSON detects
// the shape violation and routes the call through the repair path. The
// repair is a SINGLE tools-less, schema-bearing completion — not a fresh
// tool loop — so adapters that support native structured output apply
// grammar-constrained decoding on the retry and the answer is shape-correct
// on the wire. This is the "VALIDATION-TRIGGERED REPAIR" acceptance case.
func TestRunJSON_ValidationTriggersRepair(t *testing.T) {
	// First answer is a bare JSON array — parses, but validateSchema
	// detects the root-type mismatch against the schema's "object" type.
	// The inner object is ALSO schema-invalid (missing required "refuted")
	// so the rescue scan (rescueBody) cannot salvage it and the repair path
	// genuinely fires. Repair returns a correct-shape object.
	fc := newFakeClient(
		textResp(`[{"file":"a.go","message":"bug"}]`, 5, 5),
		textResp(validFindingJSON, 5, 5),
	)
	fc.caps = llm.Capabilities{StructuredOutput: true}
	r := NewRunner(fc, nil, "sys")

	var got findingWithRefuted
	out, err := r.RunJSON(context.Background(), "task", json.RawMessage(findWithCandidatesSchema), &got)
	if err != nil {
		t.Fatalf("RunJSON should succeed after shape-repair: %v", err)
	}
	if got.File != "a.go" || got.Message != "bug" || got.Refuted != false {
		t.Errorf("parsed = %+v, want the valid finding", got)
	}
	// First call ran the main loop with schema; second call is the
	// single-completion repair. Total: 2 requests.
	if len(fc.requests) != 2 {
		t.Fatalf("client calls = %d, want 2 (main + repair)", len(fc.requests))
	}
	// The repair request MUST be tools-less so Google/Anthropic also honor
	// the native schema on the retry.
	repairReq := fc.requests[1]
	if len(repairReq.Tools) != 0 {
		t.Errorf("repair request carried %d tool(s), want 0 (tools-less constrained completion)", len(repairReq.Tools))
	}
	// The repair request MUST carry the schema (capability on) so the
	// adapter applies grammar-constrained decoding.
	if string(repairReq.ResponseSchema) != findWithCandidatesSchema {
		t.Errorf("repair ResponseSchema = %s, want the schema verbatim", string(repairReq.ResponseSchema))
	}
	// The repair prompt must mention the parse/shape failure so the model
	// knows why its previous output was rejected.
	if !strings.Contains(repairReq.Messages[0].Content, "failed to parse") {
		t.Errorf("repair prompt missing parse-failure note:\n%s", repairReq.Messages[0].Content)
	}
	_ = out
}

// TestRunJSON_ValidationTriggersRepair_MissingRequired is the same
// shape-violation routing but exercises the "missing required field" branch
// of validateSchema (object root, type matches, but a required key is
// absent). The repair should still fire and ultimately succeed.
func TestRunJSON_ValidationTriggersRepair_MissingRequired(t *testing.T) {
	// First answer is an object missing the schema-required "refuted" key.
	// Repair returns a correct-shape object.
	fc := newFakeClient(
		textResp(`{"file":"a.go","message":"bug"}`, 5, 5),
		textResp(validFindingJSON, 5, 5),
	)
	fc.caps = llm.Capabilities{StructuredOutput: true}
	r := NewRunner(fc, nil, "sys")

	var got findingWithRefuted
	if _, err := r.RunJSON(context.Background(), "task", json.RawMessage(findWithCandidatesSchema), &got); err != nil {
		t.Fatalf("RunJSON should succeed after missing-required repair: %v", err)
	}
	if got.File != "a.go" {
		t.Errorf("parsed = %+v", got)
	}
	if len(fc.requests) != 2 {
		t.Errorf("client calls = %d, want 2 (main + repair)", len(fc.requests))
	}
}

// TestRunJSON_RescuesProseWrappedAnswer covers bugbot-9fac's dominant live
// failure mode: a weak model prefixing the final JSON with prose ("Based on
// my investigation, ... {plan}"), which fails stripBody's leading-value
// parse ("invalid character 'B' looking for beginning of value"). The
// schema-guided rescue scan must extract the embedded schema-valid object
// and succeed WITHOUT spending the repair round-trip.
func TestRunJSON_RescuesProseWrappedAnswer(t *testing.T) {
	fc := newFakeClient(
		textResp("Based on my investigation, the bug is clear. Here is the finding:\n"+validFindingJSON, 5, 5),
	)
	r := NewRunner(fc, nil, "sys")

	var got findingWithRefuted
	if _, err := r.RunJSON(context.Background(), "task", json.RawMessage(findWithCandidatesSchema), &got); err != nil {
		t.Fatalf("RunJSON should rescue a prose-wrapped schema-valid answer: %v", err)
	}
	if got.File != "a.go" || got.Message != "bug" {
		t.Errorf("parsed = %+v, want the valid finding", got)
	}
	if len(fc.requests) != 1 {
		t.Errorf("client calls = %d, want 1 (rescue must not spend the repair round-trip)", len(fc.requests))
	}
}

// TestRunJSON_RescuesInnerObjectFromWrappedArray: a bare-array wrap of a
// schema-valid object is rescued to that inner object — the schema is the
// arbiter of WHICH embedded candidate is the answer, so the array root
// (schema-invalid) is skipped and the inner object accepted, with no repair.
func TestRunJSON_RescuesInnerObjectFromWrappedArray(t *testing.T) {
	fc := newFakeClient(
		textResp(`[`+validFindingJSON+`]`, 5, 5),
	)
	r := NewRunner(fc, nil, "sys")

	var got findingWithRefuted
	if _, err := r.RunJSON(context.Background(), "task", json.RawMessage(findWithCandidatesSchema), &got); err != nil {
		t.Fatalf("RunJSON should rescue the inner object of a wrapped array: %v", err)
	}
	if got.File != "a.go" {
		t.Errorf("parsed = %+v", got)
	}
	if len(fc.requests) != 1 {
		t.Errorf("client calls = %d, want 1 (rescue must not spend the repair round-trip)", len(fc.requests))
	}
}

// TestRunJSON_MangledHeadStillRepairs pins the rescue's boundary using the
// live 2026-07-17 the_cloud shape: a final answer whose JSON head was
// swallowed (`": {"repro/x_test.go": "...
// "}, "cmd": [...]`) leaves NO complete embedded value that satisfies the
// schema — the leading string literal and the bare files map both fail — so
// the rescue must NOT fire and the repair round-trip proceeds as before.
func TestRunJSON_MangledHeadStillRepairs(t *testing.T) {
	fc := newFakeClient(
		textResp(`": {"file": "a.go"}, "message": "bug", "refuted": false}`, 5, 5),
		textResp(validFindingJSON, 5, 5),
	)
	r := NewRunner(fc, nil, "sys")

	var got findingWithRefuted
	if _, err := r.RunJSON(context.Background(), "task", json.RawMessage(findWithCandidatesSchema), &got); err != nil {
		t.Fatalf("RunJSON should succeed via repair: %v", err)
	}
	if len(fc.requests) != 2 {
		t.Errorf("client calls = %d, want 2 (mangled head is not rescuable; repair must fire)", len(fc.requests))
	}
}

// TestRunJSON_RepairOutputRescuedFromProse: the rescue also applies to the
// REPAIR completion's output — a repair reply that wraps a schema-valid
// answer in prose still counts instead of failing the whole call.
func TestRunJSON_RepairOutputRescuedFromProse(t *testing.T) {
	fc := newFakeClient(
		textResp("no json here at all", 5, 5),
		textResp("Sure! Here is the corrected JSON:\n"+validFindingJSON, 5, 5),
	)
	r := NewRunner(fc, nil, "sys")

	var got findingWithRefuted
	if _, err := r.RunJSON(context.Background(), "task", json.RawMessage(findWithCandidatesSchema), &got); err != nil {
		t.Fatalf("RunJSON should rescue the repair's prose-wrapped answer: %v", err)
	}
	if got.File != "a.go" {
		t.Errorf("parsed = %+v", got)
	}
	if len(fc.requests) != 2 {
		t.Errorf("client calls = %d, want 2 (main + repair)", len(fc.requests))
	}
}

// TestRunJSON_RepairStillWrongShape asserts that when the repair's reply is
// STILL the wrong shape, RunJSON returns the canonical "did not parse as JSON
// after one repair" error (with a wrapping shape error so callers can
// distinguish a shape violation from a parse failure via errors.Is/Wraps).
func TestRunJSON_RepairStillWrongShape(t *testing.T) {
	// First answer is a bare array (wrong shape). Repair also returns a
	// bare array. Both calls have the same shape violation.
	fc := newFakeClient(
		textResp(`[{"file":"a.go"}]`, 5, 5),
		textResp(`[{"file":"a.go"}]`, 5, 5),
	)
	fc.caps = llm.Capabilities{StructuredOutput: true}
	r := NewRunner(fc, nil, "sys")

	var got findingWithRefuted
	_, err := r.RunJSON(context.Background(), "task", json.RawMessage(findWithCandidatesSchema), &got)
	if err == nil {
		t.Fatal("expected error after still-wrong repair")
	}
	if !strings.Contains(err.Error(), "after one repair") {
		t.Errorf("error = %v, want 'after one repair'", err)
	}
	// The wrapped error must be the shape violation, not a parse failure —
	// callers can errors.As/errors.Is to classify the failure mode.
	if !strings.Contains(err.Error(), "root JSON type") {
		t.Errorf("error = %v, want the wrapped shape-violation message", err)
	}
}

// TestRunJSON_ParseFailureWrapsSentinel locks the contract the reproducer's
// revision loop depends on: a RunJSON failure caused by the model's OWN output
// (unparseable JSON or a schema violation, even after the repair round) wraps
// [ErrUnparseableOutput], while an infrastructure failure in the underlying
// tool loop is returned unwrapped and must NOT match the sentinel. The
// distinction is what lets a caller treat a bad answer as recoverable (revise)
// without swallowing a real transport failure.
func TestRunJSON_ParseFailureWrapsSentinel(t *testing.T) {
	t.Run("unparseable after repair", func(t *testing.T) {
		fc := newFakeClient(
			textResp("garbage", 5, 5),
			textResp("still garbage", 5, 5),
		)
		r := NewRunner(fc, nil, "sys")
		var got finding
		_, err := r.RunJSON(context.Background(), "task", nil, &got)
		if err == nil {
			t.Fatal("expected error after failed repair")
		}
		if !errors.Is(err, ErrUnparseableOutput) {
			t.Errorf("errors.Is(err, ErrUnparseableOutput) = false; err = %v", err)
		}
		if !strings.Contains(err.Error(), "after one repair") {
			t.Errorf("message regressed; err = %v", err)
		}
	})

	t.Run("schema violation after repair", func(t *testing.T) {
		// A bare array where the schema requires an object: valid JSON, wrong
		// shape — exercises the validateSchema wrap site, not the unmarshal one.
		fc := newFakeClient(
			textResp(`[{"file":"a.go"}]`, 5, 5),
			textResp(`[{"file":"a.go"}]`, 5, 5),
		)
		fc.caps = llm.Capabilities{StructuredOutput: true}
		r := NewRunner(fc, nil, "sys")
		var got findingWithRefuted
		_, err := r.RunJSON(context.Background(), "task", json.RawMessage(findWithCandidatesSchema), &got)
		if err == nil {
			t.Fatal("expected schema-violation error")
		}
		if !errors.Is(err, ErrUnparseableOutput) {
			t.Errorf("errors.Is(err, ErrUnparseableOutput) = false; err = %v", err)
		}
	})

	t.Run("infra error is not wrapped", func(t *testing.T) {
		// The underlying loop fails (transport): RunJSON returns it unwrapped,
		// so it carries the transport cause but NOT the parse sentinel.
		boom := errors.New("connection reset")
		fc := newFakeClient(scriptStep{err: boom})
		r := NewRunner(fc, nil, "sys")
		var got finding
		_, err := r.RunJSON(context.Background(), "task", nil, &got)
		if err == nil {
			t.Fatal("expected infra error")
		}
		if errors.Is(err, ErrUnparseableOutput) {
			t.Errorf("infra error must not match ErrUnparseableOutput; err = %v", err)
		}
		if !errors.Is(err, boom) {
			t.Errorf("infra error should preserve the transport cause; err = %v", err)
		}
	})
}

// deepCandidatesSchema mirrors the production finder schema's nested shape: an
// object with a "candidates" array of objects carrying an enum severity, an
// integer line with a minimum, a min-length evidence string, and
// additionalProperties:false. It exercises every branch validateSchema adds
// over the old shallow root check.
const deepCandidatesSchema = `{
  "type":"object",
  "properties":{
    "candidates":{
      "type":"array",
      "items":{
        "type":"object",
        "properties":{
          "file":{"type":"string","minLength":1},
          "line":{"type":"integer","minimum":1},
          "severity":{"type":"string","enum":["critical","high","medium","low"]},
          "evidence":{"type":"string","minLength":1}
        },
        "required":["file","line","severity","evidence"],
        "additionalProperties":false
      }
    }
  },
  "required":["candidates"],
  "additionalProperties":false
}`

// filesMapSchema mirrors the repro/patch "files" contract: an object whose
// values are constrained by an additionalProperties subschema (string) and
// which must hold at least one entry (minProperties).
const filesMapSchema = `{
  "type":"object",
  "properties":{
    "files":{"type":"object","additionalProperties":{"type":"string"},"minProperties":1}
  },
  "required":["files"],
  "additionalProperties":false
}`

// oneCandidate wraps a single candidate body in the {"candidates":[...]} root
// so test cases can focus on the one field under test.
func oneCandidate(fields string) string {
	return `{"candidates":[` + fields + `]}`
}

// validCandidate is one well-shaped candidate object for deepCandidatesSchema.
const validCandidate = `{"file":"a.go","line":7,"severity":"high","evidence":"x"}`

// TestValidateSchema pins down the deep validator's contract: the historical
// root-level cases (preserved verbatim error phrasing) plus the nested
// type/required/enum/minimum/minLength/additionalProperties/minItems/
// minProperties branches that the old shallow check ignored.
func TestValidateSchema(t *testing.T) {
	deep := json.RawMessage(deepCandidatesSchema)
	files := json.RawMessage(filesMapSchema)
	root := json.RawMessage(findWithCandidatesSchema)
	cases := []struct {
		name    string
		schema  json.RawMessage
		body    string
		wantErr bool
		wantMsg string
	}{
		// --- preserved root-level behavior ---
		{name: "nil schema is a no-op", schema: nil, body: `not even json`},
		{name: "matching object with all required passes", schema: root, body: validFindingJSON},
		{name: "root type mismatch: bare array", schema: root, body: `[{"file":"a.go"}]`,
			wantErr: true, wantMsg: "root JSON type"},
		{name: "root missing required field", schema: root, body: `{"file":"a.go","message":"bug"}`,
			wantErr: true, wantMsg: `root object missing required field "refuted"`},
		{name: "schema requires nothing: any object passes", schema: json.RawMessage(`{"type":"object"}`), body: `{}`},
		{name: "schema with no type: any value passes", schema: json.RawMessage(`{}`), body: `42`},
		// --- deep: nested object/array recursion ---
		{name: "valid nested candidate passes", schema: deep, body: oneCandidate(validCandidate)},
		{name: "empty candidates array passes (found nothing)", schema: deep, body: `{"candidates":[]}`},
		{name: "nested missing required field", schema: deep,
			body:    oneCandidate(`{"file":"a.go","line":7,"severity":"high"}`),
			wantErr: true, wantMsg: `candidates[0]: missing required field "evidence"`},
		{name: "nested bad enum", schema: deep,
			body:    oneCandidate(`{"file":"a.go","line":7,"severity":"blocker","evidence":"x"}`),
			wantErr: true, wantMsg: `candidates[0].severity: value "blocker" is not one of the allowed values`},
		{name: "nested wrong type (string where integer)", schema: deep,
			body:    oneCandidate(`{"file":"a.go","line":"7","severity":"high","evidence":"x"}`),
			wantErr: true, wantMsg: `candidates[0].line: JSON type "string" does not match schema type "integer"`},
		{name: "integer keyword rejects a fractional number", schema: deep,
			body:    oneCandidate(`{"file":"a.go","line":7.5,"severity":"high","evidence":"x"}`),
			wantErr: true, wantMsg: `candidates[0].line: JSON type "number" does not match schema type "integer"`},
		{name: "minimum violation", schema: deep,
			body:    oneCandidate(`{"file":"a.go","line":0,"severity":"high","evidence":"x"}`),
			wantErr: true, wantMsg: `candidates[0].line: value 0 is below the minimum 1`},
		{name: "minLength violation on required string", schema: deep,
			body:    oneCandidate(`{"file":"a.go","line":7,"severity":"high","evidence":""}`),
			wantErr: true, wantMsg: `candidates[0].evidence: string length 0 is below the minimum 1`},
		{name: "additionalProperties:false rejects an unknown key", schema: deep,
			body:    oneCandidate(`{"file":"a.go","line":7,"severity":"high","evidence":"x","cwe":"CWE-20"}`),
			wantErr: true, wantMsg: `candidates[0]: unexpected property "cwe"`},
		// --- deep: free-form map (additionalProperties subschema + minProperties) ---
		{name: "files map with string values passes", schema: files, body: `{"files":{"a_test.go":"package a"}}`},
		{name: "files map empty violates minProperties", schema: files, body: `{"files":{}}`,
			wantErr: true, wantMsg: `files: object has 0 properties, fewer than the required minimum 1`},
		{name: "files map non-string value rejected by additionalProperties schema", schema: files,
			body:    `{"files":{"a_test.go":123}}`,
			wantErr: true, wantMsg: `files.a_test.go: JSON type "number" does not match schema type "string"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSchema(tc.schema, []byte(tc.body))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("validateSchema(%s) = nil, want error containing %q", tc.body, tc.wantMsg)
				}
				if tc.wantMsg != "" && !strings.Contains(err.Error(), tc.wantMsg) {
					t.Errorf("error = %q, want substring %q", err.Error(), tc.wantMsg)
				}
			} else if err != nil {
				t.Errorf("validateSchema(%s) = %v, want nil", tc.body, err)
			}
		})
	}
}

// TestRunJSON_DeepValidationTriggersRepair is the headline regression for the
// deep validator: an answer that is valid JSON, the right ROOT shape, AND
// unmarshals cleanly into the typed struct — but carries a CONTRACT violation
// the old shallow root check could never see (an out-of-enum severity). The
// strengthened validateSchema rejects it and routes the call through the single
// repair round-trip, which returns a schema-valid answer.
func TestRunJSON_DeepValidationTriggersRepair(t *testing.T) {
	const enumSchema = `{
  "type":"object",
  "required":["severity"],
  "properties":{"severity":{"type":"string","enum":["high","low"]}},
  "additionalProperties":false
}`
	type sev struct {
		Severity string `json:"severity"`
	}
	// First answer: a wrong-enum value. It parses, has the required root key,
	// and unmarshals into sev{Severity:"blocker"} without error — exactly the
	// silent corruption the shallow check let through. Repair returns a valid
	// enum value.
	fc := newFakeClient(
		textResp(`{"severity":"blocker"}`, 5, 5),
		textResp(`{"severity":"high"}`, 5, 5),
	)
	fc.caps = llm.Capabilities{StructuredOutput: true}
	r := NewRunner(fc, nil, "sys")

	var got sev
	if _, err := r.RunJSON(context.Background(), "task", json.RawMessage(enumSchema), &got); err != nil {
		t.Fatalf("RunJSON should succeed after deep-validation repair: %v", err)
	}
	if got.Severity != "high" {
		t.Errorf("parsed severity = %q, want %q (the repaired value)", got.Severity, "high")
	}
	if len(fc.requests) != 2 {
		t.Fatalf("client calls = %d, want 2 (main + repair)", len(fc.requests))
	}
	// The repair prompt must name the enum violation so the model can fix it.
	if !strings.Contains(fc.requests[1].Messages[0].Content, "allowed values") {
		t.Errorf("repair prompt missing the enum-violation detail:\n%s", fc.requests[1].Messages[0].Content)
	}
}

// TestRunJSON_TrailingCommaNoRepair proves that a final answer which is a
// schema-valid JSON object followed by a trailing comma is parsed successfully
// in the first attempt — no repair round-trip. Exactly one model completion is
// consumed; a repair would require a second.
func TestRunJSON_TrailingCommaNoRepair(t *testing.T) {
	// Queue EXACTLY one completion: if stripBody correctly extracts the leading
	// JSON value, RunJSON parses and succeeds without repair. A repair would
	// consume the (unscripted) second slot and produce "(unscripted)", which
	// would itself fail to parse and cause an error.
	fc := newFakeClient(
		textResp(`{"file":"a.go","message":"bug","refuted":false},`, 5, 5),
	)
	r := NewRunner(fc, nil, "sys")

	var got findingWithRefuted
	out, err := r.RunJSON(context.Background(), "task", json.RawMessage(findWithCandidatesSchema), &got)
	if err != nil {
		t.Fatalf("RunJSON: %v", err)
	}
	if got.File != "a.go" || got.Message != "bug" || got.Refuted {
		t.Errorf("parsed = %+v", got)
	}
	// Single completion: proves no repair round-trip happened.
	if fc.callCount() != 1 {
		t.Errorf("client calls = %d, want 1 (no repair)", fc.callCount())
	}
	_ = out
}

// TestRunJSON_DoubleValueParsesFirst proves that when the model emits two
// concatenated JSON objects, RunJSON parses and returns the first one.
func TestRunJSON_DoubleValueParsesFirst(t *testing.T) {
	first := `{"file":"first.go","message":"first bug","refuted":false}`
	second := `{"file":"second.go","message":"second bug","refuted":true}`
	fc := newFakeClient(
		textResp(first+second, 5, 5),
	)
	r := NewRunner(fc, nil, "sys")

	var got findingWithRefuted
	if _, err := r.RunJSON(context.Background(), "task", json.RawMessage(findWithCandidatesSchema), &got); err != nil {
		t.Fatalf("RunJSON: %v", err)
	}
	if got.File != "first.go" || got.Message != "first bug" || got.Refuted {
		t.Errorf("parsed = %+v, want first object", got)
	}
	if fc.callCount() != 1 {
		t.Errorf("client calls = %d, want 1 (no repair)", fc.callCount())
	}
}

// TestRunJSON_TruncatedLeadingValueErrors proves that an INCOMPLETE leading
// JSON value (truncated mid-object) is NOT rescued by the trailing-content
// fix: stripBody returns the raw body, the parse fails, repair fires, and when
// the repair also fails, RunJSON returns a non-nil error.
func TestRunJSON_TruncatedLeadingValueErrors(t *testing.T) {
	fc := newFakeClient(
		textResp(`{"files":{"a.go":"x"}`, 5, 5), // missing closing }
		textResp(`still truncated {`, 5, 5),     // repair also unparseable
	)
	r := NewRunner(fc, nil, "sys")

	var got struct {
		Files map[string]string `json:"files"`
	}
	_, err := r.RunJSON(context.Background(), "task", nil, &got)
	if err == nil {
		t.Fatal("expected non-nil error for truncated leading JSON value")
	}
}

// TestRunJSON_EmptyBodyErrors confirms an empty/whitespace body still errors
// via the "empty model output" path, before any JSON extraction is attempted.
// The first two empty turns are absorbed by the bugbot-kpp2 empty-turn-nudge
// cap (maxEmptyTurnNudges=2); the third exhausts it and the loop breaks with
// an empty FinalText, and the fourth is the one repair completion — also
// scripted empty so the repair path fails the same way.
func TestRunJSON_EmptyBodyErrors(t *testing.T) {
	fc := newFakeClient(
		textResp("   ", 5, 5),
		textResp("", 5, 5),
		textResp("", 5, 5),
		textResp("", 5, 5),
	)
	r := NewRunner(fc, nil, "sys")

	var got finding
	_, err := r.RunJSON(context.Background(), "task", nil, &got)
	if err == nil {
		t.Fatal("expected error for empty body")
	}
	if !strings.Contains(err.Error(), "empty model output") {
		t.Errorf("error = %v, want to contain 'empty model output'", err)
	}
}

// TestRunJSON_EmptyTurnNudgeRecovers verifies bugbot-kpp2: a think-only turn
// (zero tool calls, text that strips to empty) is nudged rather than treated
// as the model's final answer, so a subsequent tool call and valid JSON
// answer still complete the run cleanly with no repair needed.
func TestRunJSON_EmptyTurnNudgeRecovers(t *testing.T) {
	fc := newFakeClient(
		thinkOnlyResp("let me plan this out...", 10, 5),
		toolResp("c1", "echo", `{"v":"orient"}`, 10, 4),
		textResp(`{"file":"a.go","message":"bug"}`, 8, 3),
	)
	r := NewRunner(fc, []Tool{echoTool{name: "echo"}}, "sys")

	var got finding
	out, err := r.RunJSON(context.Background(), "find a bug", nil, &got)
	if err != nil {
		t.Fatalf("RunJSON: %v", err)
	}
	if got.File != "a.go" || got.Message != "bug" {
		t.Errorf("parsed = %+v", got)
	}
	// 3 completions: think-only nudge, tool call, final answer.
	if out.Iterations != 3 {
		t.Errorf("Iterations = %d, want 3", out.Iterations)
	}
	foundNudge := false
	for _, m := range out.Messages {
		if m.Role == llm.RoleUser && m.Content == emptyTurnNudge {
			foundNudge = true
		}
	}
	if !foundNudge {
		t.Error("conversation does not contain the empty-turn nudge message")
	}
}

// TestRunJSON_EmptyTurnNudgeCapExhausted verifies the bugbot-kpp2 nudge cap:
// three consecutive think-only turns exhaust maxEmptyTurnNudges (2 nudges),
// the loop breaks on the third with an empty FinalText, and RunJSON proceeds
// through its normal parse-failure/repair path — exactly 3 main-loop
// completions plus one repair completion, no infinite loop.
func TestRunJSON_EmptyTurnNudgeCapExhausted(t *testing.T) {
	fc := newFakeClient(
		thinkOnlyResp("first thought", 10, 5),
		thinkOnlyResp("second thought", 10, 5),
		thinkOnlyResp("third thought", 10, 5),
		textResp(`{"file":"a.go","message":"repaired"}`, 5, 5),
	)
	r := NewRunner(fc, nil, "sys")

	var got finding
	out, err := r.RunJSON(context.Background(), "find a bug", nil, &got)
	if err != nil {
		t.Fatalf("RunJSON: %v", err)
	}
	if got.File != "a.go" || got.Message != "repaired" {
		t.Errorf("parsed = %+v", got)
	}
	if fc.callCount() != 4 {
		t.Errorf("completions = %d, want 4 (3 main-loop + 1 repair)", fc.callCount())
	}
	nudgeCount := 0
	for _, m := range out.Messages {
		if m.Role == llm.RoleUser && m.Content == emptyTurnNudge {
			nudgeCount++
		}
	}
	if nudgeCount != maxEmptyTurnNudges {
		t.Errorf("nudge count = %d, want %d (cap)", nudgeCount, maxEmptyTurnNudges)
	}
}
