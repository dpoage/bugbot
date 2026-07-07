package progress

import (
	"testing"
)

// TestDescribe_KindToolCall covers describeToolCallBase + describeToolCallDone
// for all tool types, including Phase=start and Phase=done with count/err
// variants, plus the malformed/empty-tool edge cases.
func TestDescribe_KindToolCall(t *testing.T) {
	tests := []struct {
		name string
		ev   Event
		want string
	}{
		// read_file — start
		{
			name: "read_file start with range",
			ev:   Event{Kind: KindToolCall, Tool: "read_file", Phase: "start", File: "pkg/foo.go", Line: 10, EndLine: 40},
			want: "read_file pkg/foo.go:10-40",
		},
		{
			name: "read_file start with start line only",
			ev:   Event{Kind: KindToolCall, Tool: "read_file", Phase: "start", File: "pkg/foo.go", Line: 10},
			want: "read_file pkg/foo.go:10",
		},
		{
			name: "read_file start no path",
			ev:   Event{Kind: KindToolCall, Tool: "read_file", Phase: "start"},
			want: "read_file",
		},
		// read_file — done suffixes
		{
			name: "read_file done with count",
			ev:   Event{Kind: KindToolCall, Tool: "read_file", Phase: "done", File: "pkg/foo.go", Line: 10, EndLine: 40, Count: 30},
			want: "read_file pkg/foo.go:10-40 [done, 30 lines]",
		},
		{
			name: "read_file done zero count",
			ev:   Event{Kind: KindToolCall, Tool: "read_file", Phase: "done", File: "pkg/foo.go"},
			want: "read_file pkg/foo.go [done]",
		},
		{
			name: "read_file done with error",
			ev:   Event{Kind: KindToolCall, Tool: "read_file", Phase: "done", File: "pkg/foo.go", Err: "file not found"},
			want: "read_file pkg/foo.go [done, error: file not found]",
		},
		// grep — start
		{
			name: "grep start with pattern and dir",
			ev:   Event{Kind: KindToolCall, Tool: "grep", Phase: "start", Pattern: "TODO", File: "internal/"},
			want: `grep "TODO" in internal/`,
		},
		{
			name: "grep start no dir",
			ev:   Event{Kind: KindToolCall, Tool: "grep", Phase: "start", Pattern: "TODO"},
			want: `grep "TODO"`,
		},
		{
			name: "grep start empty pattern",
			ev:   Event{Kind: KindToolCall, Tool: "grep", Phase: "start"},
			want: `grep "…"`,
		},
		// grep — done suffixes
		{
			name: "grep done with hits",
			ev:   Event{Kind: KindToolCall, Tool: "grep", Phase: "done", Pattern: "TODO", Count: 3},
			want: `grep "TODO" [done, 3 hits]`,
		},
		{
			name: "grep done zero hits",
			ev:   Event{Kind: KindToolCall, Tool: "grep", Phase: "done", Pattern: "TODO", Count: 0},
			want: `grep "TODO" [done, 0 hits]`,
		},
		{
			name: "grep done with error",
			ev:   Event{Kind: KindToolCall, Tool: "grep", Phase: "done", Pattern: "X", Err: "exit 1"},
			want: `grep "X" [done, error: exit 1]`,
		},
		// find_references — done with refs
		{
			name: "find_references done with count",
			ev:   Event{Kind: KindToolCall, Tool: "find_references", Phase: "done", Symbol: "Emit", Count: 7},
			want: "find_references Emit [done, 7 refs]",
		},
		{
			name: "find_references done zero count",
			ev:   Event{Kind: KindToolCall, Tool: "find_references", Phase: "done", Symbol: "Emit"},
			want: "find_references Emit [done]",
		},
		// find_usages — done with refs
		{
			name: "find_usages done with count",
			ev:   Event{Kind: KindToolCall, Tool: "find_usages", Phase: "done", Symbol: "Sink", Count: 2},
			want: "find_usages Sink [done, 2 refs]",
		},
		// find_implementations — done with refs
		{
			name: "find_implementations done with count",
			ev:   Event{Kind: KindToolCall, Tool: "find_implementations", Phase: "done", Symbol: "Tool", Count: 4},
			want: "find_implementations Tool [done, 4 refs]",
		},
		// find_definition — start
		{
			name: "find_definition start",
			ev:   Event{Kind: KindToolCall, Tool: "find_definition", Phase: "start", Symbol: "Runner", File: "runner.go"},
			want: "find_definition Runner in runner.go",
		},
		// read_symbol
		{
			name: "read_symbol start with file",
			ev:   Event{Kind: KindToolCall, Tool: "read_symbol", Phase: "start", Symbol: "Runner", File: "agent.go"},
			want: "read_symbol Runner in agent.go",
		},
		{
			name: "read_symbol start no file",
			ev:   Event{Kind: KindToolCall, Tool: "read_symbol", Phase: "start", Symbol: "Runner"},
			want: "read_symbol Runner",
		},
		{
			name: "read_symbol start no fields",
			ev:   Event{Kind: KindToolCall, Tool: "read_symbol", Phase: "start"},
			want: "read_symbol",
		},
		// list_dir
		{
			name: "list_dir start",
			ev:   Event{Kind: KindToolCall, Tool: "list_dir", Phase: "start", File: "internal/agent"},
			want: "list_dir internal/agent",
		},
		{
			name: "list_dir done",
			ev:   Event{Kind: KindToolCall, Tool: "list_dir", Phase: "done", File: "internal/agent"},
			want: "list_dir internal/agent [done]",
		},
		// run_tests
		{
			name: "run_tests start",
			ev:   Event{Kind: KindToolCall, Tool: "run_tests", Phase: "start", File: "./internal/..."},
			want: "run_tests ./internal/...",
		},
		// sandbox_exec
		{
			name: "sandbox_exec start",
			ev:   Event{Kind: KindToolCall, Tool: "sandbox_exec", Phase: "start"},
			want: "sandbox_exec",
		},
		{
			name: "sandbox_exec done with error",
			ev:   Event{Kind: KindToolCall, Tool: "sandbox_exec", Phase: "done", Err: "exit 1"},
			want: "sandbox_exec [done, error: exit 1]",
		},
		// status_note
		{
			name: "status_note start",
			ev:   Event{Kind: KindToolCall, Tool: "status_note", Phase: "start", Symbol: "checking parser"},
			want: "status_note: checking parser",
		},
		// post_lead
		{
			name: "post_lead start",
			ev:   Event{Kind: KindToolCall, Tool: "post_lead", Phase: "start"},
			want: "post_lead",
		},
		// write_repro_file
		{
			name: "write_repro_file start with path",
			ev:   Event{Kind: KindToolCall, Tool: "write_repro_file", Phase: "start", File: "repro_test.go"},
			want: "write_repro_file repro_test.go",
		},
		{
			name: "write_repro_file start no path",
			ev:   Event{Kind: KindToolCall, Tool: "write_repro_file", Phase: "start"},
			want: "write_repro_file",
		},
		// delete_repro_file
		{
			name: "delete_repro_file start with path",
			ev:   Event{Kind: KindToolCall, Tool: "delete_repro_file", Phase: "start", File: "repro_test.go"},
			want: "delete_repro_file repro_test.go",
		},
		{
			name: "delete_repro_file start no path",
			ev:   Event{Kind: KindToolCall, Tool: "delete_repro_file", Phase: "start"},
			want: "delete_repro_file",
		},
		// workspace
		{
			name: "workspace start with symbol",
			ev:   Event{Kind: KindToolCall, Tool: "workspace", Phase: "start", Symbol: "exec go test ./..."},
			want: "workspace exec go test ./...",
		},
		{
			name: "workspace start no symbol",
			ev:   Event{Kind: KindToolCall, Tool: "workspace", Phase: "start"},
			want: "workspace",
		},
		// summarize_package — explicit case (not generic fallback)
		{
			name: "summarize_package start with pkg and count",
			ev:   Event{Kind: KindToolCall, Tool: "summarize_package", Phase: "start", File: "internal/funnel", Count: 3},
			want: "summarizing internal/funnel [3 files]",
		},
		{
			name: "summarize_package start with pkg no count",
			ev:   Event{Kind: KindToolCall, Tool: "summarize_package", Phase: "start", File: "internal/funnel"},
			want: "summarizing internal/funnel",
		},
		{
			name: "summarize_package start no file",
			ev:   Event{Kind: KindToolCall, Tool: "summarize_package", Phase: "start"},
			want: "summarize_package",
		},
		{
			name: "summarize_package done with count",
			ev:   Event{Kind: KindToolCall, Tool: "summarize_package", Phase: "done", File: "internal/funnel", Count: 3},
			want: "summarizing internal/funnel [3 files] [done, 3 files]",
		},
		{
			name: "summarize_package done no count",
			ev:   Event{Kind: KindToolCall, Tool: "summarize_package", Phase: "done", File: "internal/funnel"},
			want: "summarizing internal/funnel [done]",
		},
		{
			name: "summarize_package done with error",
			ev:   Event{Kind: KindToolCall, Tool: "summarize_package", Phase: "done", File: "internal/funnel", Err: "context deadline exceeded"},
			want: "summarizing internal/funnel [done, error: context deadline exceeded]",
		},
		// unknown tool
		{
			name: "unknown tool start",
			ev:   Event{Kind: KindToolCall, Tool: "my_custom_tool", Phase: "start"},
			want: "my_custom_tool",
		},
		{
			name: "unknown tool done",
			ev:   Event{Kind: KindToolCall, Tool: "my_custom_tool", Phase: "done"},
			want: "my_custom_tool [done]",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Describe(tc.ev)
			if got != tc.want {
				t.Errorf("Describe() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDescribe_Generic covers describeGeneric for all non-KindToolCall kinds.
func TestDescribe_Generic(t *testing.T) {
	tests := []struct {
		name string
		ev   Event
		want string
	}{
		{
			name: "KindAgentStarted",
			ev:   Event{Kind: KindAgentStarted, Role: "finder", Label: "nil-deref"},
			want: "agent started: finder [nil-deref]",
		},
		{
			name: "KindAgentFinished",
			ev:   Event{Kind: KindAgentFinished, Role: "verifier", Label: "nil-deref"},
			want: "agent done: verifier [nil-deref]",
		},
		{
			name: "KindStageStarted",
			ev:   Event{Kind: KindStageStarted, Stage: "persist"},
			want: "stage: persist",
		},
		{
			name: "KindStageFinished",
			ev:   Event{Kind: KindStageFinished, Stage: "persist"},
			want: "stage done: persist",
		},
		{
			name: "KindFindingVerified",
			ev:   Event{Kind: KindFindingVerified, Title: "nil deref in main"},
			want: "verified: nil deref in main",
		},
		{
			name: "KindFindingKilled",
			ev:   Event{Kind: KindFindingKilled, Title: "false positive"},
			want: "killed: false positive",
		},
		{
			name: "KindReproAttempt",
			ev:   Event{Kind: KindReproAttempt, Attempt: 1, MaxAttempts: 3, Verdict: "exit_zero"},
			want: "repro attempt 1/3: exit_zero",
		},
		{
			name: "KindToolUnhealthy",
			ev:   Event{Kind: KindToolUnhealthy, Tool: "sandbox", Severity: "high"},
			want: "tool unhealthy: sandbox (high)",
		},
		{
			name: "KindScanStarted",
			ev:   Event{Kind: KindScanStarted, ScanKind: "fuzzer"},
			want: "scan started: fuzzer",
		},
		{
			name: "KindScanFinished",
			ev:   Event{Kind: KindScanFinished, ScanKind: "fuzzer"},
			want: "scan finished: fuzzer",
		},
		{
			name: "unknown kind",
			ev:   Event{Kind: "some_future_kind"},
			want: "some_future_kind",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Describe(tc.ev)
			if got != tc.want {
				t.Errorf("Describe() = %q, want %q", got, tc.want)
			}
		})
	}
}
