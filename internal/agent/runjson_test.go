package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type finding struct {
	File    string `json:"file"`
	Message string `json:"message"`
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

func TestStripThinkBlocks(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "no think block",
			in:   `{"file":"a.go"}`,
			want: `{"file":"a.go"}`,
		},
		{
			name: "think before json",
			in:   "<think>let me reason about this</think>\n{\"file\":\"a.go\"}",
			want: `{"file":"a.go"}`,
		},
		{
			name: "thinking tag variant",
			in:   "<thinking>reason</thinking>{\"x\":1}",
			want: `{"x":1}`,
		},
		{
			name: "case insensitive",
			in:   "<THINK>reason</Think>{\"x\":1}",
			want: `{"x":1}`,
		},
		{
			name: "multiline think",
			in:   "<think>\nline one\nline two\n</think>\n{\"x\":1}",
			want: `{"x":1}`,
		},
		{
			name: "multiple consecutive blocks",
			in:   "<think>a</think><think>b</think>\n{\"x\":1}",
			want: `{"x":1}`,
		},
		{
			name: "unclosed trailing think (truncation)",
			in:   "<think>truncated reasoning with no close",
			want: ``,
		},
		{
			name: "closed block then unclosed truncation",
			in:   "<think>first</think><think>truncated",
			want: ``,
		},
		{
			name: "literal <think> inside json string is preserved",
			in:   `{"note":"contains <think> literally","x":1}`,
			want: `{"note":"contains <think> literally","x":1}`,
		},
		{
			name: "think before fenced json (fence left for stripFences)",
			in:   "<think>reason</think>\n```json\n{\"x\":1}\n```",
			want: "```json\n{\"x\":1}\n```",
		},
		{
			name: "embedded closing tag inside json not over-stripped",
			in:   "<think>r</think>{\"note\":\"a </think> b\"}",
			want: `{"note":"a </think> b"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := strings.TrimSpace(stripThinkBlocks(tc.in)); got != tc.want {
				t.Errorf("stripThinkBlocks(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
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
