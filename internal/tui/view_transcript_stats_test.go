package tui

import (
	"strings"
	"testing"
	"time"

	llmkit "github.com/dpoage/llmkit"
	"github.com/dpoage/llmkit/agent"
)

// completionEv is a successful completion event at step carrying resp.
func completionEv(step int, at time.Time, resp llmkit.Response) llmkit.Event {
	return llmkit.Event{Kind: llmkit.KindCompletion, Step: step, Time: at, SchemaVersion: llmkit.EventSchemaVersion,
		Completion: &llmkit.CompletionEvent{Response: resp}}
}

// toolRunEv is a tool_run event at step for a call to name.
func toolRunEv(step int, at time.Time, name, result string, isErr bool) llmkit.Event {
	return llmkit.Event{Kind: llmkit.KindToolRun, Step: step, Time: at, SchemaVersion: llmkit.EventSchemaVersion,
		ToolRun: &llmkit.ToolRunEvent{Call: llmkit.ToolCall{ID: name + "-call", Name: name}, Result: result, IsError: isErr}}
}

// ── computeTranscriptStats ───────────────────────────────────────────────────

func TestComputeTranscriptStats_Empty(t *testing.T) {
	for name, got := range map[string]transcriptStats{
		"nil transcript":   computeTranscriptStats(nil),
		"empty transcript": computeTranscriptStats(&agent.Transcript{}),
	} {
		if got.Steps != 0 || got.AssistantN != 0 || got.InputTokens != 0 || got.OutputTokens != 0 ||
			got.ToolCalls != 0 || got.ToolErrors != 0 || got.WallTime != 0 || len(got.AbnormalStopReasons) != 0 {
			t.Errorf("%s: got %+v, want all-zero", name, got)
		}
	}
}

func TestComputeTranscriptStats_FailedCompletion(t *testing.T) {
	// A failed completion carries no usage and is not a recorded turn.
	failed := completionEv(1, time.Time{}, llmkit.Response{})
	failed.Completion.Err = "boom"
	tr := &agent.Transcript{Record: []llmkit.Event{failed, completionEv(2, time.Time{}, llmkit.Response{Text: "hi"})}}
	got := computeTranscriptStats(tr)
	if got.InputTokens != 0 || got.OutputTokens != 0 {
		t.Errorf("failed completion should contribute zero tokens, got %+v", got)
	}
	if got.AssistantN != 1 || got.Steps != 2 {
		t.Errorf("AssistantN/Steps not tallied: %+v", got)
	}
}

func TestComputeTranscriptStats_TokensCacheAndTools(t *testing.T) {
	base := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	tr := &agent.Transcript{Record: []llmkit.Event{
		completionEv(1, base, llmkit.Response{
			ToolCalls:  []llmkit.ToolCall{{Name: "grep"}, {Name: "read"}},
			StopReason: llmkit.StopToolUse,
			Usage:      llmkit.Usage{InputTokens: 100, OutputTokens: 20, CacheReadInputTokens: 40, CacheCreationInputTokens: 5}}),
		toolRunEv(1, base.Add(2*time.Second), "grep", "", false),
		toolRunEv(1, base.Add(3*time.Second), "read", "", true),
		completionEv(2, base.Add(10*time.Second), llmkit.Response{
			StopReason: llmkit.StopEndTurn,
			Usage:      llmkit.Usage{InputTokens: 50, OutputTokens: 10}}),
	}}
	got := computeTranscriptStats(tr)
	if got.Steps != 2 {
		t.Errorf("Steps = %d, want 2", got.Steps)
	}
	if got.AssistantN != 2 {
		t.Errorf("AssistantN = %d, want 2", got.AssistantN)
	}
	if got.InputTokens != 150 || got.OutputTokens != 30 {
		t.Errorf("tokens in=%d out=%d, want in=150 out=30", got.InputTokens, got.OutputTokens)
	}
	if got.CacheReadTokens != 40 || got.CacheCreationTokens != 5 {
		t.Errorf("cache read=%d create=%d, want read=40 create=5", got.CacheReadTokens, got.CacheCreationTokens)
	}
	if got.ToolCalls != 2 {
		t.Errorf("ToolCalls = %d, want 2", got.ToolCalls)
	}
	if got.ToolErrors != 1 {
		t.Errorf("ToolErrors = %d, want 1", got.ToolErrors)
	}
	if got.WallTime != 10*time.Second {
		t.Errorf("WallTime = %s, want 10s", got.WallTime)
	}
	if len(got.AbnormalStopReasons) != 0 {
		t.Errorf("AbnormalStopReasons = %v, want none (tool_use/end_turn are normal)", got.AbnormalStopReasons)
	}
}

func TestComputeTranscriptStats_AbnormalStopReason(t *testing.T) {
	tr := &agent.Transcript{Record: []llmkit.Event{
		completionEv(1, time.Time{}, llmkit.Response{StopReason: llmkit.StopMaxTokens, Usage: llmkit.Usage{InputTokens: 1}}),
		completionEv(2, time.Time{}, llmkit.Response{StopReason: llmkit.StopError, Usage: llmkit.Usage{InputTokens: 1}}),
		completionEv(3, time.Time{}, llmkit.Response{StopReason: llmkit.StopMaxTokens, Usage: llmkit.Usage{InputTokens: 1}}),
	}}
	got := computeTranscriptStats(tr)
	want := []llmkit.StopReason{llmkit.StopMaxTokens, llmkit.StopError}
	if len(got.AbnormalStopReasons) != len(want) {
		t.Fatalf("AbnormalStopReasons = %v, want %v", got.AbnormalStopReasons, want)
	}
	for i, r := range want {
		if got.AbnormalStopReasons[i] != r {
			t.Errorf("AbnormalStopReasons[%d] = %q, want %q", i, got.AbnormalStopReasons[i], r)
		}
	}
}

// ── renderTranscript ─────────────────────────────────────────────────────────

func TestRenderTranscript_SummaryHeaderPresent(t *testing.T) {
	tr := &agent.Transcript{Record: []llmkit.Event{
		completionEv(1, time.Time{}, llmkit.Response{Text: "doing work",
			StopReason: llmkit.StopToolUse,
			Usage:      llmkit.Usage{InputTokens: 100, OutputTokens: 20}}),
		toolRunEv(1, time.Time{}, "grep", "ok", false),
	}}
	out := renderTranscript(tr)
	if !strings.Contains(out, "Transcript summary") {
		t.Errorf("renderTranscript output missing summary header:\n%s", out)
	}
	if !strings.Contains(out, "in=100") || !strings.Contains(out, "out=20") {
		t.Errorf("renderTranscript output missing token totals:\n%s", out)
	}
	if !strings.Contains(out, "steps=1") {
		t.Errorf("renderTranscript output missing step count:\n%s", out)
	}
}
