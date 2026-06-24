package funnel

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/dpoage/bugbot/internal/progress"
)

// TestRecordToolIssue_DedupCountsAndEmits pins the single chokepoint both the
// objective sink and the subjective report_tool_issue tool funnel through:
// entries dedup by (source, tool, severity) with a running count, and every
// call emits exactly one KindToolUnhealthy progress event.
func TestRecordToolIssue_DedupCountsAndEmits(t *testing.T) {
	st, repo := openFixture(t)
	var mu sync.Mutex
	var events []progress.Event
	sink := progress.SinkFunc(func(ev progress.Event) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	})
	f, err := New(RoleClients{Finder: newScriptedClient(), Verifier: newScriptedClient()}, st, repo, Options{Progress: sink})
	if err != nil {
		t.Fatal(err)
	}

	result := &Result{}
	f.recordToolIssue(result, "infra", "sandbox_exec", "high", "runtime down", progress.RoleVerifier, "cand")
	f.recordToolIssue(result, "infra", "sandbox_exec", "high", "runtime down again", progress.RoleVerifier, "cand")
	f.recordToolIssue(result, "agent", "codenav", "medium", "never returns results", progress.RoleFinder, "lens")

	// Two identical (source,tool,severity) tuples collapse to one entry (count 2);
	// the differing tuple is a separate entry (count 1).
	if len(result.Stats.ToolIssues) != 2 {
		t.Fatalf("ToolIssues = %+v, want 2 entries", result.Stats.ToolIssues)
	}
	var infra, agent *ToolIssue
	for i := range result.Stats.ToolIssues {
		switch result.Stats.ToolIssues[i].Source {
		case "infra":
			infra = &result.Stats.ToolIssues[i]
		case "agent":
			agent = &result.Stats.ToolIssues[i]
		}
	}
	if infra == nil || infra.Tool != "sandbox_exec" || infra.Severity != "high" || infra.Count != 2 {
		t.Errorf("infra entry = %+v, want sandbox_exec/high/count=2", infra)
	}
	if agent == nil || agent.Tool != "codenav" || agent.Severity != "medium" || agent.Count != 1 {
		t.Errorf("agent entry = %+v, want codenav/medium/count=1", agent)
	}

	// Every record emits one KindToolUnhealthy event carrying tool + severity.
	if len(events) != 3 {
		t.Fatalf("emitted %d events, want 3", len(events))
	}
	for _, ev := range events {
		if ev.Kind != progress.KindToolUnhealthy {
			t.Errorf("event kind = %q, want %q", ev.Kind, progress.KindToolUnhealthy)
		}
		if ev.Tool == "" || ev.Severity == "" {
			t.Errorf("event missing tool/severity: %+v", ev)
		}
	}
}

// TestMaybeReportToolIssueTool_GateAndRecord verifies the flag gates the tool's
// existence and that invoking it records an "agent"-sourced issue through the
// same chokepoint, emitting a KindToolUnhealthy event.
func TestMaybeReportToolIssueTool_GateAndRecord(t *testing.T) {
	st, repo := openFixture(t)

	// Flag off: no tool.
	fOff, err := New(RoleClients{Finder: newScriptedClient(), Verifier: newScriptedClient()}, st, repo, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if tool := fOff.maybeReportToolIssueTool(&Result{}, progress.RoleFinder, "lens"); tool != nil {
		t.Fatalf("ToolComplaints off must yield a nil tool, got %T", tool)
	}

	// Flag on: tool present; invoking it records an agent complaint.
	var events []progress.Event
	sink := progress.SinkFunc(func(ev progress.Event) { events = append(events, ev) })
	fOn, err := New(RoleClients{Finder: newScriptedClient(), Verifier: newScriptedClient()}, st, repo,
		Options{Progress: sink, Features: FeatureFlags{ToolComplaints: true}})
	if err != nil {
		t.Fatal(err)
	}
	result := &Result{}
	tool := fOn.maybeReportToolIssueTool(result, progress.RoleFinder, "lens")
	if tool == nil {
		t.Fatal("ToolComplaints on must yield a tool")
	}
	if got := tool.Def().Name; got != "report_tool_issue" {
		t.Fatalf("tool name = %q, want report_tool_issue", got)
	}
	if _, err := tool.Run(context.Background(), json.RawMessage(`{"tool":"codenav","severity":"high","summary":"never returns results"}`)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Stats.ToolIssues) != 1 {
		t.Fatalf("ToolIssues = %+v, want 1", result.Stats.ToolIssues)
	}
	if ti := result.Stats.ToolIssues[0]; ti.Source != "agent" || ti.Tool != "codenav" || ti.Severity != "high" {
		t.Errorf("entry = %+v, want agent/codenav/high", ti)
	}
	if len(events) != 1 || events[0].Kind != progress.KindToolUnhealthy {
		t.Errorf("events = %+v, want one KindToolUnhealthy", events)
	}
}
