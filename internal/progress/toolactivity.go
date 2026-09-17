package progress

import (
	"context"
	"encoding/json"
	"strings"

	llmkit "github.com/dpoage/llmkit"
	"github.com/dpoage/llmkit/agent"
)

// ToolActivity describes one tool-call execution, as routed through
// AgentScope.EmitToolCall as a KindToolCall event. bugbot owns this type: the
// runner (llmkit/agent) delivers raw tool lifecycle events and consumers do
// their own tool-name to structured-activity mapping.
//
// Phase is "start" (emitted immediately before execution) or "done" (emitted
// immediately after, with Count and Err filled from the result).
//
// Fields are a flat superset; only the relevant subset is populated for each
// tool:
//   - read_file      → File, Line (start offset), EndLine (end offset)
//   - read_symbol    → Symbol, File
//   - grep           → Pattern, File (dir or file target, optional)
//   - find_definition/references/implementations/usages → Symbol, File, Line
//   - list_dir       → File (the directory path)
//   - run_tests      → File (package/dir), Symbol (summary label)
//   - sandbox_exec   → Symbol ("sandbox")
//   - status_note    → Tool="status_note", Symbol (the note text, truncated)
//   - write_repro_file/delete_repro_file → File (the repo-relative path)
//   - workspace      → Symbol (the argv joined with spaces, truncated)
//   - post_lead      → (no extra fields)
//   - unknown        → Tool (name only)
//
// Count is set on Phase="done": for grep it is the hit count; for
// find_references/find_usages it is the reference count; for read_file it is
// the number of lines read. Zero when the tool does not produce a count.
//
// Err is the tool error string on Phase="done", or "" on success.
type ToolActivity struct {
	// Phase is "start" or "done".
	Phase string
	// Tool is the tool name (e.g. "read_file", "grep", "status_note").
	Tool string
	// File is the repo-relative file or directory path, when applicable.
	File string
	// Line is the 1-based start line (read_file, find_*).
	Line int
	// EndLine is the 1-based end line for a read window (read_file only).
	EndLine int
	// Symbol is the symbol name (read_symbol, find_*) or, for status_note,
	// the sanitized note text.
	Symbol string
	// Pattern is the grep regex.
	Pattern string
	// Count is the result count (hits, refs, lines) on Phase="done".
	Count int
	// Err is the tool error string on Phase="done", or "" on success.
	Err string
}

// Hooks returns the agent.Hooks that route a runner's tool lifecycle through
// this scope as KindToolCall events: ToolStart emits a Phase="start" event
// before each Tool.Run, ToolEnd a Phase="done" event after it carrying the
// result count (on success) or the error text (on failure — the same
// "ERROR: "-prefixed string the model sees). Wire with agent.WithHooks(scope.Hooks()).
//
// The hooks are safe for concurrent use (with agent.WithParallelTools they
// fire from per-call goroutines): they only read the call arguments and
// AgentScope.EmitToolCall is concurrency-safe.
func (s AgentScope) Hooks() agent.Hooks {
	emit := func(act ToolActivity) {
		s.EmitToolCall(act.Phase, act.Tool, act.File, act.Line, act.EndLine, act.Symbol, act.Pattern, act.Count, act.Err)
	}
	return agent.Hooks{
		ToolStart: func(ctx context.Context, ev agent.ToolEvent) {
			act := extractToolActivity(ev.Call)
			act.Phase = "start"
			emit(act)
		},
		ToolEnd: func(ctx context.Context, ev agent.ToolEvent) {
			act := extractToolActivity(ev.Call)
			act.Phase = "done"
			if ev.IsError {
				act.Err = ev.Result
			} else {
				act.Count = countFromResult(ev.Call.Name, ev.Result)
			}
			emit(act)
		},
	}
}

// extractToolActivity maps one LLM tool call to a ToolActivity with all
// structured fields populated from the call's JSON arguments. Phase is NOT set
// here — the caller stamps "start" or "done" after calling this function.
//
// Argument parsing is best-effort: a JSON failure leaves fields at their zero
// values, which produce a sane (if sparse) ToolActivity. Safe for concurrent
// use: reads only the call argument bytes.
func extractToolActivity(call llmkit.ToolCall) ToolActivity {
	act := ToolActivity{Tool: call.Name}

	// Decode the relevant arguments for each tool. Only the fields this tool
	// can produce are read; unused JSON keys are silently ignored.
	var args struct {
		Path      string   `json:"path"`
		Dir       string   `json:"dir"`
		Directory string   `json:"directory"`
		Pattern   string   `json:"pattern"`
		Symbol    string   `json:"symbol"`
		File      string   `json:"file"`
		Line      int      `json:"line"`
		StartLine int      `json:"start_line"`
		EndLine   int      `json:"end_line"`
		Note      string   `json:"note"`
		Argv      []string `json:"argv"`
	}
	_ = json.Unmarshal(call.Arguments, &args)

	switch call.Name {
	case "read_file":
		act.File = args.Path
		// Honor both start_line/end_line and plain line/end_line naming.
		if args.StartLine > 0 {
			act.Line = args.StartLine
		} else {
			act.Line = args.Line
		}
		act.EndLine = args.EndLine
	case "read_symbol":
		act.Symbol = args.Symbol
		act.File = args.Path
		if act.File == "" {
			act.File = args.File
		}
	case "grep":
		act.Pattern = args.Pattern
		act.File = args.Path
		if act.File == "" {
			act.File = args.Dir
		}
	case "find_definition", "find_references", "find_implementations",
		"find_usages":
		act.Symbol = args.Symbol
		act.File = args.File
		if act.File == "" {
			act.File = args.Path
		}
		act.Line = args.Line
	case "list_dir":
		act.File = args.Dir
		if act.File == "" {
			act.File = args.Directory
		}
		if act.File == "" {
			act.File = "."
		}
	case "run_tests":
		act.File = args.Dir
		if act.File == "" {
			act.File = args.Path
		}
	case "sandbox_exec":
		act.Symbol = "sandbox"
	case "status_note":
		// Note text is truncated to 120 runes (same as statusNoteTool.Run).
		note := strings.Join(strings.Fields(args.Note), " ")
		runes := []rune(note)
		if len(runes) > 120 {
			note = string(runes[:119]) + "…"
		}
		act.Symbol = note
	case "write_repro_file", "delete_repro_file":
		act.File = args.Path
	case "workspace":
		// Argv is joined and truncated to 120 runes (same as status_note).
		cmd := strings.Join(strings.Fields(strings.Join(args.Argv, " ")), " ")
		runes := []rune(cmd)
		if len(runes) > 120 {
			cmd = string(runes[:119]) + "…"
		}
		act.Symbol = cmd
	case "post_lead":
		// No structured fields; Tool="post_lead" is sufficient.
	default:
		// Unknown tool: Tool name is the only useful field.
	}
	return act
}

// countFromResult extracts a result count from a tool's output string.
// For most tools the count is 0 (line count is expensive to compute and not
// worth it for observability). For grep we count newline-separated matches.
// This is best-effort: a failure returns 0.
func countFromResult(toolName, result string) int {
	switch toolName {
	case "grep":
		if result == "" {
			return 0
		}
		return strings.Count(result, "\n") + 1
	}
	return 0
}
