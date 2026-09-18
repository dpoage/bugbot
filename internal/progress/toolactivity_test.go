package progress

import (
	"context"
	"strings"
	"testing"

	llmkit "github.com/dpoage/llmkit"
	"github.com/dpoage/llmkit/agent"
)

// TestExtractToolActivity_Mapping verifies the structured extractor populates
// the correct ToolActivity fields for each tool type.
func TestExtractToolActivity_Mapping(t *testing.T) {
	tests := []struct {
		name string
		call llmkit.ToolCall
		want ToolActivity
	}{
		{
			name: "read_file with path and range",
			call: llmkit.ToolCall{Name: "read_file", Arguments: []byte(`{"path":"cmd/main.go","start_line":10,"end_line":40}`)},
			want: ToolActivity{Tool: "read_file", File: "cmd/main.go", Line: 10, EndLine: 40},
		},
		{
			name: "read_file no path",
			call: llmkit.ToolCall{Name: "read_file", Arguments: []byte(`{}`)},
			want: ToolActivity{Tool: "read_file"},
		},
		{
			name: "read_symbol",
			call: llmkit.ToolCall{Name: "read_symbol", Arguments: []byte(`{"symbol":"Runner","path":"agent.go"}`)},
			want: ToolActivity{Tool: "read_symbol", Symbol: "Runner", File: "agent.go"},
		},
		{
			name: "grep with pattern and dir",
			call: llmkit.ToolCall{Name: "grep", Arguments: []byte(`{"pattern":"TODO","dir":"internal/"}`)},
			want: ToolActivity{Tool: "grep", Pattern: "TODO", File: "internal/"},
		},
		{
			name: "find_definition",
			call: llmkit.ToolCall{Name: "find_definition", Arguments: []byte(`{"symbol":"Runner","file":"runner.go"}`)},
			want: ToolActivity{Tool: "find_definition", Symbol: "Runner", File: "runner.go"},
		},
		{
			name: "find_references",
			call: llmkit.ToolCall{Name: "find_references", Arguments: []byte(`{"symbol":"Emit"}`)},
			want: ToolActivity{Tool: "find_references", Symbol: "Emit"},
		},
		{
			name: "find_implementations",
			call: llmkit.ToolCall{Name: "find_implementations", Arguments: []byte(`{"symbol":"Tool"}`)},
			want: ToolActivity{Tool: "find_implementations", Symbol: "Tool"},
		},
		{
			name: "find_usages",
			call: llmkit.ToolCall{Name: "find_usages", Arguments: []byte(`{"symbol":"Sink"}`)},
			want: ToolActivity{Tool: "find_usages", Symbol: "Sink"},
		},
		{
			name: "list_dir",
			call: llmkit.ToolCall{Name: "list_dir", Arguments: []byte(`{"dir":"internal/agent"}`)},
			want: ToolActivity{Tool: "list_dir", File: "internal/agent"},
		},
		{
			name: "list_dir via directory field",
			call: llmkit.ToolCall{Name: "list_dir", Arguments: []byte(`{"directory":"src"}`)},
			want: ToolActivity{Tool: "list_dir", File: "src"},
		},
		{
			name: "list_dir empty defaults to dot",
			call: llmkit.ToolCall{Name: "list_dir", Arguments: []byte(`{}`)},
			want: ToolActivity{Tool: "list_dir", File: "."},
		},
		{
			name: "sandbox_exec",
			call: llmkit.ToolCall{Name: "sandbox_exec", Arguments: []byte(`{}`)},
			want: ToolActivity{Tool: "sandbox_exec", Symbol: "sandbox"},
		},
		{
			name: "post_lead",
			call: llmkit.ToolCall{Name: "post_lead", Arguments: []byte(`{}`)},
			want: ToolActivity{Tool: "post_lead"},
		},
		{
			name: "status_note",
			call: llmkit.ToolCall{Name: "status_note", Arguments: []byte(`{"note":"checking parser"}`)},
			want: ToolActivity{Tool: "status_note", Symbol: "checking parser"},
		},
		{
			name: "write_repro_file",
			call: llmkit.ToolCall{Name: "write_repro_file", Arguments: []byte(`{"path":"repro_test.go","contents":"package main"}`)},
			want: ToolActivity{Tool: "write_repro_file", File: "repro_test.go"},
		},
		{
			name: "delete_repro_file",
			call: llmkit.ToolCall{Name: "delete_repro_file", Arguments: []byte(`{"path":"repro_test.go"}`)},
			want: ToolActivity{Tool: "delete_repro_file", File: "repro_test.go"},
		},
		{
			name: "workspace",
			call: llmkit.ToolCall{Name: "workspace", Arguments: []byte(`{"argv":["exec","go","test","./..."]}`)},
			want: ToolActivity{Tool: "workspace", Symbol: "exec go test ./..."},
		},
		{
			name: "workspace truncates long argv",
			call: llmkit.ToolCall{Name: "workspace", Arguments: []byte(`{"argv":["exec","go","test","-run","` + strings.Repeat("x", 130) + `"]}`)},
			want: ToolActivity{Tool: "workspace", Symbol: "exec go test -run " + strings.Repeat("x", 101) + "…"},
		},
		{
			name: "unknown tool",
			call: llmkit.ToolCall{Name: "some_custom_tool", Arguments: []byte(`{}`)},
			want: ToolActivity{Tool: "some_custom_tool"},
		},
		{
			name: "malformed JSON args",
			call: llmkit.ToolCall{Name: "read_file", Arguments: []byte(`not-valid-json`)},
			want: ToolActivity{Tool: "read_file"}, // zero fields; no panic
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractToolActivity(tc.call)
			if got.Tool != tc.want.Tool {
				t.Errorf("Tool = %q, want %q", got.Tool, tc.want.Tool)
			}
			if got.File != tc.want.File {
				t.Errorf("File = %q, want %q", got.File, tc.want.File)
			}
			if got.Symbol != tc.want.Symbol {
				t.Errorf("Symbol = %q, want %q", got.Symbol, tc.want.Symbol)
			}
			if got.Pattern != tc.want.Pattern {
				t.Errorf("Pattern = %q, want %q", got.Pattern, tc.want.Pattern)
			}
			if got.Line != tc.want.Line {
				t.Errorf("Line = %d, want %d", got.Line, tc.want.Line)
			}
			if got.EndLine != tc.want.EndLine {
				t.Errorf("EndLine = %d, want %d", got.EndLine, tc.want.EndLine)
			}
		})
	}
}

// TestCountFromResult pins the result-count rule: grep counts
// newline-separated hits, every other tool counts 0.
func TestCountFromResult(t *testing.T) {
	if got := countFromResult("grep", "a.go:1\nb.go:5\nc.go:9\n"); got != 4 {
		t.Errorf("grep count = %d, want 4", got)
	}
	if got := countFromResult("grep", ""); got != 0 {
		t.Errorf("grep count on empty result = %d, want 0", got)
	}
	if got := countFromResult("read_file", "line one\nline two\n"); got != 0 {
		t.Errorf("read_file count = %d, want 0 (only grep produces a count)", got)
	}
}

// TestAgentScopeHooks_ToolLifecycle pins the start/done emission contract of
// AgentScope.Hooks: ToolStart emits Phase="start" with the mapped fields,
// ToolEnd emits Phase="done" with the grep result count on success and the
// error text (what the model sees, "ERROR: " prefix included) on failure.
func TestAgentScopeHooks_ToolLifecycle(t *testing.T) {
	var rec recordingSink
	scope := NewAgentScope(&rec, RoleFinder, "lens")
	hooks := scope.Hooks()

	hooks.ToolStart(context.Background(), agent.ToolEvent{
		Step: 1,
		Call: llmkit.ToolCall{Name: "grep", Arguments: []byte(`{"pattern":"TODO","dir":"internal/"}`)},
	})
	hooks.ToolEnd(context.Background(), agent.ToolEvent{
		Step:   1,
		Call:   llmkit.ToolCall{Name: "grep", Arguments: []byte(`{"pattern":"TODO","dir":"internal/"}`)},
		Result: "a.go:1\nb.go:2\n",
	})
	hooks.ToolEnd(context.Background(), agent.ToolEvent{
		Step:    2,
		Call:    llmkit.ToolCall{Name: "run_tests", Arguments: []byte(`{"dir":"pkg/foo"}`)},
		Result:  "ERROR: tool run_tests failed: boom",
		IsError: true,
	})

	evs := rec.snapshot()
	if len(evs) != 3 {
		t.Fatalf("emitted %d events, want 3", len(evs))
	}
	start, done, failed := evs[0], evs[1], evs[2]
	if start.Phase != "start" || start.Tool != "grep" || start.Pattern != "TODO" || start.File != "internal/" {
		t.Errorf("start event = %+v, want grep start with pattern/dir", start)
	}
	if done.Phase != "done" || done.Count != 3 || done.Err != "" {
		t.Errorf("done event = %+v, want grep done with count=3, no err", done)
	}
	if failed.Phase != "done" || failed.Tool != "run_tests" || failed.File != "pkg/foo" || failed.Err == "" ||
		!strings.Contains(failed.Err, "ERROR: tool run_tests failed: boom") {
		t.Errorf("failed event = %+v, want run_tests done with the error text in Err", failed)
	}
}

// TestSanitizeNote pins the status_note display rule — whitespace collapsed to
// single spaces, truncation to 120 runes with a trailing ellipsis — and that
// the extractor's derived status_note activity uses the SAME rule as the
// helper (one owner; a divergence here would make the tool's own emission and
// the hook-derived one disagree).
func TestSanitizeNote(t *testing.T) {
	if got := SanitizeNote("  checking   for\tnil derefs\n"); got != "checking for nil derefs" {
		t.Errorf("SanitizeNote collapse = %q, want %q", got, "checking for nil derefs")
	}
	if got, want := SanitizeNote(strings.Repeat("a", 200)), strings.Repeat("a", 119)+"…"; got != want {
		t.Errorf("SanitizeNote truncation = %d runes, want %d (119 runes + ellipsis)",
			len([]rune(got)), len([]rune(want)))
	}
	if got := SanitizeNote(""); got != "" {
		t.Errorf("SanitizeNote empty = %q, want empty", got)
	}
	note := strings.Repeat("x", 200)
	call := llmkit.ToolCall{Name: "status_note", Arguments: []byte(`{"note":"` + note + `"}`)}
	if got := extractToolActivity(call).Symbol; got != SanitizeNote(note) {
		t.Errorf("extractor Symbol disagrees with SanitizeNote on a long note")
	}
}
