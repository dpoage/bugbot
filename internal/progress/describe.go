package progress

import (
	"fmt"
	"strconv"
)

// Describe returns a short, single-line human-readable description of ev,
// suitable for the live pane, log, and status.json activity notes. It is the
// canonical formatter for KindToolCall and a sane fallback for all other kinds.
//
// KindToolCall output examples:
//
//	read_file pkg/foo.go:10-40         (start, with range)
//	grep "TODO" in internal/           (start, with dir)
//	find_references Runner             (start, symbol)
//	status_note: checking parser       (start/done, status note)
//	read_file pkg/foo.go [done, 30 lines]
//	grep "TODO" [done, 3 hits]
//	sandbox_exec [done, error: exit 1]
//
// All other kinds fall back to a minimal "kind: key-field" line so a
// caller that switches on Kind still gets a non-empty string for display.
func Describe(ev Event) string {
	if ev.Kind != KindToolCall {
		return describeGeneric(ev)
	}
	base := describeToolCallBase(ev)
	if ev.Phase == "done" {
		return base + describeToolCallDone(ev)
	}
	return base
}

// describeToolCallBase builds the "tool target" portion of a KindToolCall line,
// shared between Phase=start and Phase=done.
func describeToolCallBase(ev Event) string {
	switch ev.Tool {
	case "read_file":
		if ev.File == "" {
			return "read_file"
		}
		loc := ev.File
		if ev.Line > 0 {
			if ev.EndLine > ev.Line {
				loc += fmt.Sprintf(":%d-%d", ev.Line, ev.EndLine)
			} else {
				loc += fmt.Sprintf(":%d", ev.Line)
			}
		}
		return "read_file " + loc
	case "read_symbol":
		if ev.Symbol != "" && ev.File != "" {
			return "read_symbol " + ev.Symbol + " in " + ev.File
		}
		if ev.Symbol != "" {
			return "read_symbol " + ev.Symbol
		}
		return "read_symbol"
	case "grep":
		pat := ev.Pattern
		if pat == "" {
			pat = "…"
		}
		s := "grep " + strconv.Quote(pat)
		if ev.File != "" {
			s += " in " + ev.File
		}
		return s
	case "find_definition":
		return describeNavTool("find_definition", ev)
	case "find_references":
		return describeNavTool("find_references", ev)
	case "find_implementations":
		return describeNavTool("find_implementations", ev)
	case "find_usages":
		return describeNavTool("find_usages", ev)
	case "list_dir":
		if ev.File != "" {
			return "list_dir " + ev.File
		}
		return "list_dir"
	case "run_tests":
		if ev.File != "" {
			return "run_tests " + ev.File
		}
		return "run_tests"
	case "sandbox_exec":
		return "sandbox_exec"
	case "status_note":
		if ev.Symbol != "" {
			return "status_note: " + ev.Symbol
		}
		return "status_note"
	case "write_repro_file":
		if ev.File != "" {
			return "write_repro_file " + ev.File
		}
		return "write_repro_file"
	case "delete_repro_file":
		if ev.File != "" {
			return "delete_repro_file " + ev.File
		}
		return "delete_repro_file"
	case "run_repro":
		if ev.Symbol != "" {
			return "run_repro " + ev.Symbol
		}
		return "run_repro"
	case "post_lead":
		return "post_lead"
	case "summarize_package":
		if ev.File != "" && ev.Count > 0 {
			return fmt.Sprintf("summarizing %s [%d files]", ev.File, ev.Count)
		}
		if ev.File != "" {
			return "summarizing " + ev.File
		}
		return "summarize_package"
	default:
		return ev.Tool
	}
}

// describeNavTool renders a find_* or read_symbol call with symbol and optional file.
func describeNavTool(tool string, ev Event) string {
	if ev.Symbol != "" {
		s := tool + " " + ev.Symbol
		if ev.File != "" {
			s += " in " + ev.File
		}
		return s
	}
	return tool
}

// describeToolCallDone renders the " [done, ...]" suffix for Phase=done.
func describeToolCallDone(ev Event) string {
	if ev.Err != "" {
		return " [done, error: " + ev.Err + "]"
	}
	switch ev.Tool {
	case "grep":
		return fmt.Sprintf(" [done, %d hits]", ev.Count)
	case "find_references", "find_usages", "find_implementations":
		if ev.Count > 0 {
			return fmt.Sprintf(" [done, %d refs]", ev.Count)
		}
		return " [done]"
	case "read_file":
		if ev.Count > 0 {
			return fmt.Sprintf(" [done, %d lines]", ev.Count)
		}
		return " [done]"
	case "summarize_package":
		if ev.Count > 0 {
			return fmt.Sprintf(" [done, %d files]", ev.Count)
		}
		return " [done]"
	}
	return " [done]"
}

// describeGeneric renders a non-KindToolCall event as a minimal one-liner.
func describeGeneric(ev Event) string {
	switch ev.Kind {
	case KindAgentStarted:
		return fmt.Sprintf("agent started: %s [%s]", ev.Role, ev.Label)
	case KindAgentFinished:
		return fmt.Sprintf("agent done: %s [%s]", ev.Role, ev.Label)
	case KindStageStarted:
		return "stage: " + ev.Stage
	case KindStageFinished:
		return "stage done: " + ev.Stage
	case KindFindingVerified:
		return "verified: " + ev.Title
	case KindFindingKilled:
		return "killed: " + ev.Title
	case KindReproAttempt:
		return fmt.Sprintf("repro attempt %d/%d: %s", ev.Attempt, ev.MaxAttempts, ev.Verdict)
	case KindToolUnhealthy:
		return "tool unhealthy: " + ev.Tool + " (" + ev.Severity + ")"
	case KindScanStarted:
		return "scan started: " + ev.ScanKind
	case KindScanFinished:
		return "scan finished: " + ev.ScanKind
	default:
		return string(ev.Kind)
	}
}
