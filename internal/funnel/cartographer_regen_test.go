// cartographer_regen_test.go tests the synthetic tool-call event emission
// added to summarizePackage: read_file start/done pairs per included member
// file, summarize_package start/done around RunJSON, all bracketed by the
// KindAgentStarted / KindAgentFinished events the AgentScope produces.
package funnel

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/store"
)

// regenRecordingSink captures every progress.Event emitted during a
// summarizePackage call. Safe for concurrent use.
type regenRecordingSink struct {
	mu     sync.Mutex
	events []progress.Event
}

func (r *regenRecordingSink) Handle(ev progress.Event) {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
}

func (r *regenRecordingSink) snapshot() []progress.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]progress.Event, len(r.events))
	copy(out, r.events)
	return out
}

// openRegenFixture opens a funnel backed by the openCartographyFixture git
// repo and layers additional files on top. It returns a recording sink, the
// funnel, sorted member list, and a dummy fps map.
func openRegenFixture(t *testing.T, files map[string]string) (*regenRecordingSink, *Funnel, []string, map[string]string) {
	t.Helper()

	st, repo := openCartographyFixture(t)
	t.Cleanup(func() { _ = st.Close() })

	root := repo.Root()

	// Write the caller-supplied files into the fixture repo root.
	for rel, content := range files {
		abs := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	sink := &regenRecordingSink{}
	f, err := New(RoleClients{Finder: newScriptedClient(), Verifier: newScriptedClient()},
		st, repo, Options{
			Features: FeatureFlags{Cartographer: true},
			Progress: sink,
		})
	if err != nil {
		t.Fatal(err)
	}

	// Build sorted member list and a dummy fps map.
	var members []string
	fps := make(map[string]string)
	for rel := range files {
		members = append(members, rel)
		fps[rel] = "fake-fp-" + rel
	}
	// Sort for determinism (matches packagesSpanned output).
	for i := 0; i < len(members)-1; i++ {
		for j := i + 1; j < len(members); j++ {
			if members[i] > members[j] {
				members[i], members[j] = members[j], members[i]
			}
		}
	}

	return sink, f, members, fps
}

// filterToolPhase returns KindToolCall events with the given tool and phase.
func filterToolPhase(evs []progress.Event, tool, phase string) []progress.Event {
	var out []progress.Event
	for _, ev := range evs {
		if ev.Kind == progress.KindToolCall && ev.Tool == tool && ev.Phase == phase {
			out = append(out, ev)
		}
	}
	return out
}

// newScriptedClientWithFallback returns a scriptedClient whose fallback
// response is the given JSON body.
func newScriptedClientWithFallback(body string) *scriptedClient {
	c := newScriptedClient()
	c.fallback = body
	return c
}

// TestSummarizePackage_EventSequence verifies the happy-path event ordering:
// agent_started → read_file start/done pairs (one per member) →
// summarize_package start → summarize_package done → agent_finished.
func TestSummarizePackage_EventSequence(t *testing.T) {
	const pkg = "evtpkg"
	files := map[string]string{
		pkg + "/alpha.go": "package evtpkg\n\nfunc Alpha() {}\n",
		pkg + "/beta.go":  "package evtpkg\n\nfunc Beta() {}\n",
		pkg + "/gamma.go": "package evtpkg\n\nfunc Gamma() {}\n",
	}

	sink, f, members, fps := openRegenFixture(t, files)
	ctx := context.Background()

	summary, err := f.summarizePackage(ctx, newScriptedClientWithFallback(`{"summary":"Happy path summary."}`), nil, pkg, members, fps)
	if err != nil {
		t.Fatalf("summarizePackage returned error: %v", err)
	}
	if summary == "" {
		t.Fatal("summary is empty")
	}

	evs := sink.snapshot()

	// Must start with agent_started.
	if len(evs) == 0 {
		t.Fatal("no events recorded")
	}
	if evs[0].Kind != progress.KindAgentStarted {
		t.Errorf("first event = %v, want KindAgentStarted", evs[0].Kind)
	}
	if evs[0].Role != progress.RoleCartographer {
		t.Errorf("started event Role = %q, want %q", evs[0].Role, progress.RoleCartographer)
	}
	if evs[0].Label != pkg {
		t.Errorf("started event Label = %q, want %q", evs[0].Label, pkg)
	}

	// Must end with agent_finished.
	last := evs[len(evs)-1]
	if last.Kind != progress.KindAgentFinished {
		t.Errorf("last event = %v, want KindAgentFinished", last.Kind)
	}

	// Collect KindToolCall events in emission order.
	var tcEvs []progress.Event
	for _, ev := range evs {
		if ev.Kind == progress.KindToolCall {
			tcEvs = append(tcEvs, ev)
		}
	}

	// Expect: 2 events per file (start+done) + 2 for summarize_package (start+done).
	wantTC := len(members)*2 + 2
	if len(tcEvs) != wantTC {
		t.Fatalf("got %d KindToolCall events, want %d; events: %+v", len(tcEvs), wantTC, tcEvs)
	}

	// First 2*N events are read_file start/done pairs in member order.
	for i, rel := range members {
		startEv := tcEvs[i*2]
		doneEv := tcEvs[i*2+1]

		if startEv.Tool != "read_file" || startEv.Phase != "start" {
			t.Errorf("member[%d] start: got tool=%q phase=%q", i, startEv.Tool, startEv.Phase)
		}
		if startEv.File != rel {
			t.Errorf("member[%d] start File = %q, want %q", i, startEv.File, rel)
		}
		if startEv.Line != 1 {
			t.Errorf("member[%d] start Line = %d, want 1", i, startEv.Line)
		}

		if doneEv.Tool != "read_file" || doneEv.Phase != "done" {
			t.Errorf("member[%d] done: got tool=%q phase=%q", i, doneEv.Tool, doneEv.Phase)
		}
		if doneEv.File != rel {
			t.Errorf("member[%d] done File = %q, want %q", i, doneEv.File, rel)
		}
		if doneEv.Count <= 0 {
			t.Errorf("member[%d] done Count = %d, want > 0", i, doneEv.Count)
		}
		if doneEv.EndLine != doneEv.Count {
			t.Errorf("member[%d] done EndLine = %d, want %d (== Count)", i, doneEv.EndLine, doneEv.Count)
		}
		if doneEv.Err != "" {
			t.Errorf("member[%d] done Err = %q, want empty", i, doneEv.Err)
		}
	}

	// Last 2 events are summarize_package start then done.
	spStart := tcEvs[len(tcEvs)-2]
	spDone := tcEvs[len(tcEvs)-1]

	if spStart.Tool != "summarize_package" || spStart.Phase != "start" {
		t.Errorf("summarize_package start: got tool=%q phase=%q", spStart.Tool, spStart.Phase)
	}
	if spStart.File != pkg {
		t.Errorf("summarize_package start File = %q, want %q", spStart.File, pkg)
	}
	if spStart.Count != len(members) {
		t.Errorf("summarize_package start Count = %d, want %d", spStart.Count, len(members))
	}

	if spDone.Tool != "summarize_package" || spDone.Phase != "done" {
		t.Errorf("summarize_package done: got tool=%q phase=%q", spDone.Tool, spDone.Phase)
	}
	if spDone.Err != "" {
		t.Errorf("summarize_package done Err = %q, want empty on success", spDone.Err)
	}
}

// TestSummarizePackage_UnreadableFile verifies that an unreadable file emits a
// read_file done with a non-empty Err and that summarization still proceeds
// for the readable members.
func TestSummarizePackage_UnreadableFile(t *testing.T) {
	const pkg = "unrpkg"
	files := map[string]string{
		pkg + "/good.go": "package unrpkg\n\nfunc Good() {}\n",
	}

	sink, f, members, fps := openRegenFixture(t, files)
	root := f.repo.Root()

	// Add a ghost member that does not exist on disk.
	ghost := pkg + "/ghost.go"
	members = append(members, ghost)
	fps[ghost] = "ghost-fp"
	// Sort for determinism.
	for i := 0; i < len(members)-1; i++ {
		for j := i + 1; j < len(members); j++ {
			if members[i] > members[j] {
				members[i], members[j] = members[j], members[i]
			}
		}
	}

	// Confirm ghost truly does not exist.
	if _, statErr := os.Stat(filepath.Join(root, ghost)); statErr == nil {
		t.Fatalf("ghost file %q should not exist", ghost)
	}

	ctx := context.Background()
	_, err := f.summarizePackage(ctx, newScriptedClientWithFallback(`{"summary":"Partial summary."}`), nil, pkg, members, fps)
	if err != nil {
		t.Fatalf("summarizePackage returned error: %v", err)
	}

	evs := sink.snapshot()
	// Find the done event for the ghost file.
	var ghostDone *progress.Event
	for i := range evs {
		ev := &evs[i]
		if ev.Kind == progress.KindToolCall && ev.Tool == "read_file" && ev.Phase == "done" && ev.File == ghost {
			ghostDone = ev
			break
		}
	}
	if ghostDone == nil {
		t.Fatalf("no read_file done event for ghost file %q; all events: %+v", ghost, evs)
	}
	if ghostDone.Err == "" {
		t.Errorf("read_file done for ghost file: Err is empty, want non-empty error string")
	}
}

// TestSummarizePackage_RunJSONError verifies that a RunJSON failure produces a
// summarize_package done event with Err set and returns the error to the caller.
func TestSummarizePackage_RunJSONError(t *testing.T) {
	const pkg = "errpkg"
	files := map[string]string{
		pkg + "/main.go": "package errpkg\n\nfunc Main() {}\n",
	}

	sink, f, members, fps := openRegenFixture(t, files)
	ctx := context.Background()

	// errClient always returns an error from Complete.
	bad := &errClient{err: errFakeLLM}
	_, err := f.summarizePackage(ctx, bad, nil, pkg, members, fps)
	if err == nil {
		t.Fatal("expected error from summarizePackage, got nil")
	}

	evs := sink.snapshot()
	var spDone *progress.Event
	for i := range evs {
		ev := &evs[i]
		if ev.Kind == progress.KindToolCall && ev.Tool == "summarize_package" && ev.Phase == "done" {
			spDone = ev
			break
		}
	}
	if spDone == nil {
		t.Fatalf("no summarize_package done event found; events: %+v", evs)
	}
	if spDone.Err == "" {
		t.Errorf("summarize_package done Err is empty; want error string from failed RunJSON")
	}
}

// TestSummarizePackage_NilSink verifies that a nil progress sink does not
// panic and produces the summary normally.
func TestSummarizePackage_NilSink(t *testing.T) {
	const pkg = "nilsinkpkg"
	files := map[string]string{
		pkg + "/a.go": "package nilsinkpkg\n\nfunc A() {}\n",
	}

	ctx := context.Background()
	tmpDir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Use openCartographyFixture for the repo, write our files there.
	st2, repo := openCartographyFixture(t)
	t.Cleanup(func() { _ = st2.Close() })
	root := repo.Root()
	for rel, content := range files {
		abs := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Funnel with nil Progress — must not panic.
	f, err := New(RoleClients{Finder: newScriptedClient(), Verifier: newScriptedClient()},
		st2, repo, Options{
			Features: FeatureFlags{Cartographer: true},
			Progress: nil,
		})
	if err != nil {
		t.Fatal(err)
	}

	members := []string{pkg + "/a.go"}
	fps := map[string]string{pkg + "/a.go": "fp-a"}

	summary, err := f.summarizePackage(ctx, newScriptedClientWithFallback(`{"summary":"Nil sink summary."}`), nil, pkg, members, fps)
	if err != nil {
		t.Fatalf("summarizePackage with nil sink returned error: %v", err)
	}
	if summary == "" {
		t.Fatal("summary is empty with nil sink")
	}
}
