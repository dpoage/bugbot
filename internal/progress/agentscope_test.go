package progress

import (
	"errors"
	"testing"
	"time"
)

// TestAgentScope_StartActivityFinish verifies the full lifecycle emits the
// three bracketing events with the scope's role/label, and that Finish carries
// tokens, duration, and the error message.
func TestAgentScope_StartActivityFinish(t *testing.T) {
	var rec recordingSink
	scope := NewAgentScope(&rec, RoleReproducer, "nil deref in parser").Start()
	scope.Activity("reading parser.go")
	scope.Finish(1234, 5*time.Second, errors.New("boom"))

	evs := rec.snapshot()
	if len(evs) != 3 {
		t.Fatalf("got %d events, want 3: %+v", len(evs), evs)
	}

	if evs[0].Kind != KindAgentStarted || evs[0].Role != RoleReproducer || evs[0].Label != "nil deref in parser" {
		t.Errorf("started event = %+v", evs[0])
	}

	if evs[1].Kind != KindAgentActivity || evs[1].Activity != "reading parser.go" ||
		evs[1].Role != RoleReproducer || evs[1].Label != "nil deref in parser" {
		t.Errorf("activity event = %+v", evs[1])
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

// TestAgentScope_EmptyActivityDropped verifies a blank note emits nothing, so a
// runner that produces no per-turn note never clears a prior meaningful one.
func TestAgentScope_EmptyActivityDropped(t *testing.T) {
	var rec recordingSink
	scope := NewAgentScope(&rec, RoleCartographer, "pkg/foo")
	scope.Activity("")

	if evs := rec.snapshot(); len(evs) != 0 {
		t.Fatalf("empty activity emitted %d events, want 0: %+v", len(evs), evs)
	}
}

// TestAgentScope_ActivitySinkRoutesToScope verifies the callback returned by
// ActivitySink emits a KindAgentActivity bound to the scope — this is exactly
// what is handed to agent.WithActivitySink and agent.NewStatusNoteTool.
func TestAgentScope_ActivitySinkRoutesToScope(t *testing.T) {
	var rec recordingSink
	sink := NewAgentScope(&rec, RoleSeverity, "3 findings").ActivitySink()
	sink("re-ranking by reachability")

	evs := rec.snapshot()
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	if evs[0].Kind != KindAgentActivity || evs[0].Role != RoleSeverity ||
		evs[0].Label != "3 findings" || evs[0].Activity != "re-ranking by reachability" {
		t.Errorf("activity event = %+v", evs[0])
	}
}

// TestAgentScope_NilSinkIsNoOp verifies a zero-value/nil-sink scope never
// panics, so unobserved runs pay nothing.
func TestAgentScope_NilSinkIsNoOp(t *testing.T) {
	scope := NewAgentScope(nil, RoleFinder, "lens").Start()
	scope.Activity("x")
	scope.Finish(1, time.Second, errors.New("e"))
	// AgentScope{} zero value too.
	var zero AgentScope
	zero.Start()
	zero.Activity("y")
	zero.Finish(0, 0, nil)
}
