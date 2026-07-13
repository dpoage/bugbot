package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/llm"
)

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

func TestComputeTranscriptStats_NilUsage(t *testing.T) {
	// An assistant event with a nil Usage pointer (e.g. a still-streaming
	// transcript captured mid-turn) must not panic and must not contribute
	// to the token totals.
	tr := &agent.Transcript{Events: []agent.Event{
		{Kind: agent.EventAssistant, Step: 1, Text: "hi", Usage: nil},
	}}
	got := computeTranscriptStats(tr)
	if got.InputTokens != 0 || got.OutputTokens != 0 {
		t.Errorf("nil Usage should contribute zero tokens, got %+v", got)
	}
	if got.AssistantN != 1 || got.Steps != 1 {
		t.Errorf("AssistantN/Steps not tallied: %+v", got)
	}
}

func TestComputeTranscriptStats_TokensCacheAndTools(t *testing.T) {
	base := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	tr := &agent.Transcript{Events: []agent.Event{
		{Kind: agent.EventAssistant, Step: 1, Time: base,
			ToolCalls:  []llm.ToolCall{{Name: "grep"}, {Name: "read"}},
			StopReason: llm.StopToolUse,
			Usage:      &llm.Usage{InputTokens: 100, OutputTokens: 20, CacheReadInputTokens: 40, CacheCreationInputTokens: 5}},
		{Kind: agent.EventToolResult, Step: 1, Time: base.Add(2 * time.Second), ToolName: "grep", IsError: false},
		{Kind: agent.EventToolResult, Step: 1, Time: base.Add(3 * time.Second), ToolName: "read", IsError: true},
		{Kind: agent.EventAssistant, Step: 2, Time: base.Add(10 * time.Second),
			StopReason: llm.StopEndTurn,
			Usage:      &llm.Usage{InputTokens: 50, OutputTokens: 10}},
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
	tr := &agent.Transcript{Events: []agent.Event{
		{Kind: agent.EventAssistant, Step: 1, StopReason: llm.StopMaxTokens, Usage: &llm.Usage{InputTokens: 1}},
		{Kind: agent.EventAssistant, Step: 2, StopReason: llm.StopError, Usage: &llm.Usage{InputTokens: 1}},
		{Kind: agent.EventAssistant, Step: 3, StopReason: llm.StopMaxTokens, Usage: &llm.Usage{InputTokens: 1}},
	}}
	got := computeTranscriptStats(tr)
	want := []llm.StopReason{llm.StopMaxTokens, llm.StopError}
	if len(got.AbnormalStopReasons) != len(want) {
		t.Fatalf("AbnormalStopReasons = %v, want %v", got.AbnormalStopReasons, want)
	}
	for i, r := range want {
		if got.AbnormalStopReasons[i] != r {
			t.Errorf("AbnormalStopReasons[%d] = %q, want %q", i, got.AbnormalStopReasons[i], r)
		}
	}
}

// ── renderTranscript summary header ─────────────────────────────────────────

func TestRenderTranscript_SummaryHeaderPresent(t *testing.T) {
	tr := &agent.Transcript{Events: []agent.Event{
		{Kind: agent.EventAssistant, Step: 1, Text: "doing work",
			StopReason: llm.StopToolUse,
			Usage:      &llm.Usage{InputTokens: 100, OutputTokens: 20}},
		{Kind: agent.EventToolResult, Step: 1, ToolName: "grep", Result: "ok"},
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
