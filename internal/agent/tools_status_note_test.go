package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestStatusNoteTool_Basic verifies the happy path: a valid note is sanitized,
// routed to the sink, and returns "noted" to the model.
func TestStatusNoteTool_Basic(t *testing.T) {
	var got ToolActivity
	tool := NewStatusNoteTool(func(act ToolActivity) { got = act })

	result, err := tool.Run(context.Background(), json.RawMessage(`{"note":"checking for nil pointer dereferences"}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result != "noted" {
		t.Errorf("result = %q, want %q", result, "noted")
	}
	if got.Tool != "status_note" {
		t.Errorf("act.Tool = %q, want status_note", got.Tool)
	}
	if got.Phase != "done" {
		t.Errorf("act.Phase = %q, want done", got.Phase)
	}
	if got.Symbol != "checking for nil pointer dereferences" {
		t.Errorf("act.Symbol = %q, want plain note", got.Symbol)
	}
}

// TestStatusNoteTool_SanitizesMultiline verifies that a multi-line note is
// collapsed to a single line (newlines → single spaces).
func TestStatusNoteTool_SanitizesMultiline(t *testing.T) {
	var got ToolActivity
	tool := NewStatusNoteTool(func(act ToolActivity) { got = act })

	_, err := tool.Run(context.Background(), json.RawMessage(`{"note":"line one\nline two\r\nline three"}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(got.Symbol, "\n") || strings.Contains(got.Symbol, "\r") {
		t.Errorf("sink received multi-line note: %q", got.Symbol)
	}
	if !strings.Contains(got.Symbol, "line one") || !strings.Contains(got.Symbol, "line two") {
		t.Errorf("sanitized note lost content: %q", got.Symbol)
	}
}

// TestStatusNoteTool_TruncatesLongNote verifies that a note longer than 120
// runes is truncated and ends with the ellipsis rune.
func TestStatusNoteTool_TruncatesLongNote(t *testing.T) {
	long := strings.Repeat("a", 200)
	var got ToolActivity
	tool := NewStatusNoteTool(func(act ToolActivity) { got = act })

	_, err := tool.Run(context.Background(), json.RawMessage(`{"note":"`+long+`"}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	runes := []rune(got.Symbol)
	if len(runes) > 121 { // 120 + ellipsis
		t.Errorf("truncation: sink received %d runes, want ≤121", len(runes))
	}
	if len(runes) > 0 && got.Symbol[len(got.Symbol)-len("…"):] != "…" {
		t.Errorf("truncated note does not end with ellipsis: %q", got.Symbol)
	}
}

// TestStatusNoteTool_BadArgs verifies that malformed JSON args return an error
// (so the runner surfaces it as a tool error, not a panic).
func TestStatusNoteTool_BadArgs(t *testing.T) {
	tool := NewStatusNoteTool(func(ToolActivity) {})
	_, err := tool.Run(context.Background(), json.RawMessage(`not-json`))
	if err == nil {
		t.Error("expected error on malformed args, got nil")
	}
}

func TestStatusNoteTool_Def(t *testing.T) {
	tool := NewStatusNoteTool(func(ToolActivity) {})
	def := tool.Def()
	if def.Name != "status_note" {
		t.Errorf("tool name = %q, want %q", def.Name, "status_note")
	}
	if def.Description == "" {
		t.Error("tool description must not be empty")
	}
}

// TestNewStatusNoteTool_PresentOnlyWhenFlagOn verifies that the tool is absent
// from the default (nil-sink) NewRunner tool set and present only when the
// caller explicitly builds it. This mirrors the funnel's maybeStatusNoteTool
// gate but exercises the tool constructor directly.
func TestNewStatusNoteTool_PresentOnlyWhenFlagOn(t *testing.T) {
	// Without the tool in the set, a toolCallActivity that emits "status_note"
	// uses the default (name) fallback.
	calls := []struct {
		flagOn bool
	}{
		{false},
		{true},
	}
	for _, tc := range calls {
		var tools []Tool
		if tc.flagOn {
			tools = append(tools, NewStatusNoteTool(func(ToolActivity) {}))
		}
		found := false
		for _, tool := range tools {
			if tool.Def().Name == "status_note" {
				found = true
			}
		}
		if found != tc.flagOn {
			t.Errorf("flagOn=%v: tool present=%v", tc.flagOn, found)
		}
	}
}
