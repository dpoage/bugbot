package progress

import (
	"errors"
	"testing"
	"time"
)

// TestAgentScope_StartEmitToolCallFinish verifies the full lifecycle emits the
// bracketing events with the scope's role/label, and that EmitToolCall produces
// a KindToolCall event.
func TestAgentScope_StartEmitToolCallFinish(t *testing.T) {
	var rec recordingSink
	scope := NewAgentScope(&rec, RoleReproducer, "nil deref in parser").Start()
	scope.EmitToolCall("start", "read_file", "parser.go", 10, 40, "", "", 0, "")
	scope.Finish(1234, 5*time.Second, errors.New("boom"))

	evs := rec.snapshot()
	if len(evs) != 3 {
		t.Fatalf("got %d events, want 3: %+v", len(evs), evs)
	}

	if evs[0].Kind != KindAgentStarted || evs[0].Role != RoleReproducer || evs[0].Label != "nil deref in parser" {
		t.Errorf("started event = %+v", evs[0])
	}

	tc := evs[1]
	if tc.Kind != KindToolCall || tc.Phase != "start" || tc.Tool != "read_file" ||
		tc.File != "parser.go" || tc.Line != 10 || tc.EndLine != 40 ||
		tc.Role != RoleReproducer || tc.Label != "nil deref in parser" {
		t.Errorf("tool_call event = %+v", tc)
	}

	fin := evs[2]
	if fin.Kind != KindAgentFinished || fin.Role != RoleReproducer || fin.Label != "nil deref in parser" {
		t.Errorf("finished kind/role/label = %+v", fin)
	}
	if fin.Tokens != 1234 {
		t.Errorf("finished Tokens = %d, want 1234", fin.Tokens)
	}
	if fin.Duration != 5*time.Second {
		t.Errorf("finished Duration = %s, want 5s", fin.Duration)
	}
	if fin.Err != "boom" {
		t.Errorf("finished Err = %q, want %q", fin.Err, "boom")
	}
}

// TestAgentScope_EventsShareAgentID verifies that every event a single
// AgentScope emits (Start, EmitToolCall, Finish) carries the SAME non-empty
// AgentID — the invariant progress.AgentEventKey and every consumer (the
// snapshot accumulator, the pane, the TUI's action feed) depend on to fold a
// run's events without colliding against a concurrent run sharing the same
// (role, label). See bugbot-r7ub.
func TestAgentScope_EventsShareAgentID(t *testing.T) {
	var rec recordingSink
	scope := NewAgentScope(&rec, RoleReproducer, "dup title").Start()
	scope.EmitToolCall("start", "read_file", "a.go", 1, 0, "", "", 0, "")
	scope.Finish(0, time.Second, nil)

	evs := rec.snapshot()
	if len(evs) != 3 {
		t.Fatalf("got %d events, want 3: %+v", len(evs), evs)
	}
	id := evs[0].AgentID
	if id == "" {
		t.Fatal("Started event has empty AgentID")
	}
	for i, ev := range evs {
		if ev.AgentID != id {
			t.Errorf("event[%d] (%s) AgentID = %q, want %q (Started's id)", i, ev.Kind, ev.AgentID, id)
		}
	}
}

// TestAgentScope_DistinctScopesMintDistinctAgentIDs verifies two AgentScopes
// built for the identical (role, label) — e.g. two reproducer agents on
// duplicate open finding titles — get distinct AgentIDs, which is what lets
// AgentEventKey disambiguate them.
func TestAgentScope_DistinctScopesMintDistinctAgentIDs(t *testing.T) {
	var rec recordingSink
	a := NewAgentScope(&rec, RoleReproducer, "dup title").Start()
	b := NewAgentScope(&rec, RoleReproducer, "dup title").Start()

	evs := rec.snapshot()
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(evs), evs)
	}
	if evs[0].AgentID == "" || evs[1].AgentID == "" {
		t.Fatalf("expected non-empty AgentIDs, got %+v", evs)
	}
	if evs[0].AgentID == evs[1].AgentID {
		t.Fatalf("two independent scopes for the same (role, label) minted the SAME AgentID: %q", evs[0].AgentID)
	}
	_ = a
	_ = b
}

// TestAgentScope_FinishSuccessHasNoErr verifies a nil error leaves Err empty,
// matching the funnel's existing emitAgentFinished contract.
func TestAgentScope_FinishSuccessHasNoErr(t *testing.T) {
	var rec recordingSink
	NewAgentScope(&rec, RolePatchProver, "fix").Finish(0, time.Second, nil)

	evs := rec.snapshot()
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	if evs[0].Err != "" {
		t.Errorf("Err = %q, want empty on success", evs[0].Err)
	}
}

// TestAgentScope_EmptyToolDropped verifies an empty tool name emits nothing, so a
// zero-value EmitToolCall never clears a prior meaningful note.
func TestAgentScope_EmptyToolDropped(t *testing.T) {
	var rec recordingSink
	scope := NewAgentScope(&rec, RoleCartographer, "pkg/foo")
	scope.EmitToolCall("start", "", "", 0, 0, "", "", 0, "")

	if evs := rec.snapshot(); len(evs) != 0 {
		t.Fatalf("empty tool emitted %d events, want 0: %+v", len(evs), evs)
	}
}

// TestAgentScope_EmitToolCallRoutesToScope verifies EmitToolCall produces a
// KindToolCall event bound to the scope — this is the structured replacement for
// ActivitySink that the funnel bridges via agent.ToolActivity.
func TestAgentScope_EmitToolCallRoutesToScope(t *testing.T) {
	var rec recordingSink
	scope := NewAgentScope(&rec, RoleSeverity, "3 findings")
	scope.EmitToolCall("start", "grep", "internal/", 0, 0, "", "TODO", 0, "")

	evs := rec.snapshot()
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	ev := evs[0]
	if ev.Kind != KindToolCall || ev.Phase != "start" || ev.Tool != "grep" ||
		ev.File != "internal/" || ev.Pattern != "TODO" ||
		ev.Role != RoleSeverity || ev.Label != "3 findings" {
		t.Errorf("tool_call event = %+v", ev)
	}
}

// TestAgentScope_NilSinkIsNoOp verifies a zero-value/nil-sink scope never
// panics, so unobserved runs pay nothing.
func TestAgentScope_NilSinkIsNoOp(t *testing.T) {
	scope := NewAgentScope(nil, RoleFinder, "lens").Start()
	scope.EmitToolCall("start", "read_file", "main.go", 0, 0, "", "", 0, "")
	scope.Finish(1, time.Second, errors.New("e"))
	// AgentScope{} zero value too.
	var zero AgentScope
	zero.Start()
	zero.EmitToolCall("done", "grep", "", 0, 0, "", "pat", 3, "")
	zero.Finish(0, 0, nil)
}
