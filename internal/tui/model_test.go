package tui

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/progress"
)

// fakeFeed is a test double for Feed: Next() is driven manually via
// send/queue rather than a real ticker, so tests are synchronous and
// deterministic — no real tea.Program, no LLM/network involved anywhere in
// this package's tests.
type fakeFeed struct {
	mode   Mode
	closed bool
}

func (f *fakeFeed) Next() tea.Cmd { return nil } // tests drive FrameMsg directly
func (f *fakeFeed) Mode() Mode    { return f.mode }
func (f *fakeFeed) Close() error  { f.closed = true; return nil }

// sendKey applies a key press to m and, when Update returns a tea.Cmd (e.g.
// the async transcript load fired by drilling into an agent), synchronously
// runs it and feeds the resulting Msg back through Update — mirroring what a
// real tea.Program's event loop does, without needing one. This is what
// makes the transcript-load-off-thread behavior (a tea.Cmd, not an inline
// call) observable in a synchronous test.
func sendKey(m Model, key string) Model {
	var km tea.KeyMsg
	switch key {
	case "enter":
		km = tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		km = tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		km = tea.KeyMsg{Type: tea.KeyTab}
	case "backspace":
		km = tea.KeyMsg{Type: tea.KeyBackspace}
	default:
		km = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	}
	next, cmd := m.Update(km)
	return runCmd(next.(Model), cmd)
}

// runCmd executes cmd (if any) and feeds its resulting Msg back through
// Update, once. Sufficient for this package's tests: none of the commands
// under test (loadTranscriptCmd) return a tea.Batch/Sequence of their own.
func runCmd(m Model, cmd tea.Cmd) Model {
	if cmd == nil {
		return m
	}
	msg := cmd()
	if msg == nil {
		return m
	}
	next, _ := m.Update(msg)
	return next.(Model)
}

func sendFrame(m Model, fr Frame) Model {
	next, cmd := m.Update(FrameMsg(fr))
	return runCmd(next.(Model), cmd)
}

func baseFrame() Frame {
	return Frame{
		HasSnapshot: true,
		Snapshot: progress.Status{
			ScanKind: "sweep",
			Stage:    "verify",
			ActiveAgents: []progress.AgentStatus{
				{Role: "verifier", Label: "candidate A", Started: time.Unix(1000, 0), Activity: "reading file"},
			},
		},
		World: WorldState{
			HasTallies: true,
			Tallies:    domain.FindingTallies{OpenByTier: map[int]int{2: 3}, Fixed: 1},
		},
		Agents: []AgentView{
			{Role: "verifier", Label: "candidate A", Live: true, Started: time.Unix(1000, 0), Activity: "reading file"},
			{Role: "finder", Label: "nil-safety", Lens: "nil-safety", Started: time.Unix(500, 0), FinishedAt: time.Unix(600, 0), Status: "ok", Candidates: 2},
		},
	}
}

func TestUpdate_LiveAgentAppearsAndUpdatesActivity(t *testing.T) {
	m := NewModel(context.Background(), &fakeFeed{}, nil)

	m = sendFrame(m, Frame{
		HasSnapshot: true,
		Agents: []AgentView{
			{Role: "verifier", Label: "candidate A", Live: true, Activity: "starting"},
		},
	})
	if len(m.frame.Agents) != 1 || m.frame.Agents[0].Activity != "starting" {
		t.Fatalf("agent not present after first frame: %+v", m.frame.Agents)
	}

	m = sendFrame(m, Frame{
		HasSnapshot: true,
		Agents: []AgentView{
			{Role: "verifier", Label: "candidate A", Live: true, Activity: "reading file"},
		},
	})
	if m.frame.Agents[0].Activity != "reading file" {
		t.Fatalf("activity not updated: %+v", m.frame.Agents[0])
	}
}

func TestUpdate_LiveAndHistoricalMerge(t *testing.T) {
	m := NewModel(context.Background(), &fakeFeed{}, nil)
	m = sendFrame(m, baseFrame())

	if len(m.frame.Agents) != 2 {
		t.Fatalf("Agents = %d, want 2 (live + historical)", len(m.frame.Agents))
	}
	var sawLive, sawHistorical bool
	for _, a := range m.frame.Agents {
		if a.Live {
			sawLive = true
		} else if a.Status == "ok" {
			sawHistorical = true
		}
	}
	if !sawLive || !sawHistorical {
		t.Fatalf("expected both live and historical agents, got %+v", m.frame.Agents)
	}
}

func TestUpdate_StaleFrameRendersIdle(t *testing.T) {
	m := NewModel(context.Background(), &fakeFeed{}, nil)
	m = sendFrame(m, Frame{HasSnapshot: true, Stale: true, World: WorldState{HasTallies: true}})

	view := stripANSI(m.View())
	if !strings.Contains(view, "idle") {
		t.Fatalf("stale frame did not render idle cockpit:\n%s", view)
	}
}

func TestUpdate_ScreenNavigation(t *testing.T) {
	m := NewModel(context.Background(), &fakeFeed{}, nil)
	m = sendFrame(m, baseFrame())

	if m.screen != screenCockpit {
		t.Fatalf("initial screen = %v, want Cockpit", m.screen)
	}

	m = sendKey(m, "tab")
	if m.screen != screenAgents {
		t.Fatalf("after tab, screen = %v, want Agents", m.screen)
	}

	m = sendKey(m, "enter")
	if m.screen != screenAgentDetail {
		t.Fatalf("after enter, screen = %v, want AgentDetail", m.screen)
	}
	if !m.transcriptLoaded {
		t.Fatal("expected transcript load attempt on drill-in")
	}
	if m.transcriptNote != "no transcript" {
		t.Fatalf("transcriptNote = %q, want %q (no TranscriptPath seeded)", m.transcriptNote, "no transcript")
	}

	m = sendKey(m, "esc")
	if m.screen != screenAgents {
		t.Fatalf("after esc, screen = %v, want back to Agents", m.screen)
	}

	m = sendKey(m, "tab")
	m = sendKey(m, "tab")
	if m.screen != screenLeads {
		t.Fatalf("after two more tabs, screen = %v, want Leads", m.screen)
	}
}

func TestUpdate_FilterNarrowsAgentList(t *testing.T) {
	m := NewModel(context.Background(), &fakeFeed{}, nil)
	m = sendFrame(m, baseFrame())
	m = sendKey(m, "tab") // -> Agents

	m = sendKey(m, "/")
	if !m.filtering {
		t.Fatal("expected filtering=true after /")
	}
	for _, r := range "finder" {
		m = sendKey(m, string(r))
	}
	idx := m.visibleAgentIndices()
	if len(idx) != 1 || m.frame.Agents[idx[0]].Role != "finder" {
		t.Fatalf("filter %q matched %v, want just the finder", m.filter, idx)
	}

	m = sendKey(m, "enter") // accept filter, stop editing
	if m.filtering {
		t.Fatal("expected filtering=false after enter")
	}
}

func TestUpdate_QuitReturnsTeaQuit(t *testing.T) {
	m := NewModel(context.Background(), &fakeFeed{}, nil)
	km := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")}
	next, cmd := m.Update(km)
	nm := next.(Model)
	if !nm.quitting {
		t.Fatal("expected quitting=true after q")
	}
	if cmd == nil {
		t.Fatal("expected a tea.Cmd (tea.Quit) after q")
	}
}

// TestView_GoldenCockpitIdle and TestView_GoldenCockpitActive lock down the
// Cockpit screen's rendered text (ANSI stripped), following the golden-frame
// approach in internal/progress/pane_test.go.
func TestView_GoldenCockpitIdle(t *testing.T) {
	m := NewModel(context.Background(), &fakeFeed{}, nil)
	m = sendFrame(m, Frame{
		World: WorldState{HasTallies: true, Tallies: domain.FindingTallies{OpenByTier: map[int]int{1: 2}}},
	})

	got := stripANSI(m.View())
	for _, want := range []string{
		"bugbot — idle",
		"no scan or daemon running",
		"World state",
		"open: T1=2",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("golden idle view missing %q, got:\n%s", want, got)
		}
	}
}

func TestView_GoldenCockpitActive(t *testing.T) {
	m := NewModel(context.Background(), &fakeFeed{}, nil)
	m = sendFrame(m, baseFrame())

	got := stripANSI(m.View())
	for _, want := range []string{
		"bugbot — active",
		"scan: kind=sweep",
		"stage: verify",
		"verifier",
		"candidate A",
		"reading file",
		"World state",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("golden active view missing %q, got:\n%s", want, got)
		}
	}
}

func TestView_GoldenAgentsScreen(t *testing.T) {
	m := NewModel(context.Background(), &fakeFeed{}, nil)
	m = sendFrame(m, baseFrame())
	m = sendKey(m, "tab")

	got := stripANSI(m.View())
	for _, want := range []string{
		"Agents",
		"verifier",
		"finder",
		"nil-safety",
		"running",
		"ok",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("golden agents view missing %q, got:\n%s", want, got)
		}
	}
}

// stripANSI wraps progress.StripANSI so this package's tests do not
// reimplement ANSI-stripping.
func stripANSI(s string) string { return progress.StripANSI(s) }

// TestUpdate_DrilldownSurvivesReorderedFrame is the B2 regression: drilling
// into an agent, then receiving a FrameMsg where mergeAgents' sort has
// reshuffled positions (a new, earlier-started agent inserted ahead of it),
// must NOT swap which agent the detail screen shows. detailIdx is re-resolved
// by agentKey identity, not held as a raw position.
func TestUpdate_DrilldownSurvivesReorderedFrame(t *testing.T) {
	m := NewModel(context.Background(), &fakeFeed{}, nil)
	m = sendFrame(m, baseFrame())
	m = sendKey(m, "tab") // -> Agents; cursor 0 is the live "verifier/candidate A" (Started=1000)

	m = sendKey(m, "enter")
	if m.screen != screenAgentDetail {
		t.Fatalf("screen = %v, want AgentDetail", m.screen)
	}
	wantKey := m.detailKey
	if wantKey == "" {
		t.Fatal("detailKey not set after drill-in")
	}
	if got := m.frame.Agents[m.detailIdx]; got.Role != "verifier" || got.Label != "candidate A" {
		t.Fatalf("drilled into %+v, want verifier/candidate A", got)
	}

	// A refreshed frame: the SAME agent (identical key) plus a brand-new
	// agent that started earlier, so sort-by-Started puts it at index 0 and
	// pushes the original agent to a different position than before.
	reordered := Frame{
		HasSnapshot: true,
		Agents: []AgentView{
			{Role: "finder", Label: "new-lens", Lens: "new-lens", Live: true, Started: time.Unix(1, 0)},
			{Role: "verifier", Label: "candidate A", Live: true, Started: time.Unix(1000, 0), Activity: "still going"},
		},
	}
	m = sendFrame(m, reordered)

	if m.detailKey != wantKey {
		t.Fatalf("detailKey changed from %q to %q across a reorder", wantKey, m.detailKey)
	}
	if m.detailIdx < 0 || m.detailIdx >= len(m.frame.Agents) {
		t.Fatalf("detailIdx = %d out of range after reorder", m.detailIdx)
	}
	got := m.frame.Agents[m.detailIdx]
	if got.Role != "verifier" || got.Label != "candidate A" {
		t.Fatalf("after reorder, detail resolved to %+v, want verifier/candidate A (position shifted, identity must not)", got)
	}
	if got.Activity != "still going" {
		t.Fatalf("resolved agent's Activity = %q, want the reordered frame's updated value", got.Activity)
	}
	// The header rendered on screen must match the same identity, not
	// whatever now sits at the old position.
	view := stripANSI(m.View())
	if !strings.Contains(view, "candidate A") || !strings.Contains(view, "still going") {
		t.Fatalf("detail view did not track the reordered agent:\n%s", view)
	}
}

// TestUpdate_TranscriptLoadsFromRealFile is the B1 regression: drilling into
// an agent whose TranscriptPath points at a real JSONL file must load and
// render it via a tea.Cmd (loadTranscriptCmd), NOT inline on the Update
// thread. It asserts the off-thread property directly: right after the
// "enter" keypress — before the returned Cmd is executed — the transcript
// must still be unset and the note must read "loading transcript...". Only
// running the Cmd (as a real tea.Program's event loop would) populates it.
// A synchronous (inline) implementation would fail this test by having the
// transcript already populated at that first checkpoint.
func TestUpdate_TranscriptLoadsFromRealFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")

	tr := &agent.Transcript{Events: []agent.Event{
		{Kind: agent.EventAssistant, Step: 1, Text: "investigating the nil check"},
		{Kind: agent.EventToolResult, Step: 1, ToolName: "read", Result: "func foo() {}"},
	}}
	var buf bytes.Buffer
	if err := tr.SaveJSONL(&buf); err != nil {
		t.Fatalf("SaveJSONL: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	m := NewModel(context.Background(), &fakeFeed{}, nil)
	m = sendFrame(m, Frame{
		HasSnapshot: true,
		Agents: []AgentView{
			{Role: "reproducer", Label: "nil deref", Lens: "nil-safety", UnitID: "u1", TranscriptPath: path},
		},
	})
	m = sendKey(m, "tab") // -> Agents

	// Fire "enter" WITHOUT running its Cmd yet, to observe the state the
	// off-thread load leaves behind before it resolves.
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if m.screen != screenAgentDetail {
		t.Fatalf("screen = %v, want AgentDetail immediately after enter", m.screen)
	}
	if cmd == nil {
		t.Fatal("enter on an agent with a TranscriptPath must return a load Cmd, got nil")
	}
	if m.transcript != nil {
		t.Fatal("transcript already populated before the load Cmd ran — load is not off-thread")
	}
	if m.transcriptNote != "loading transcript..." {
		t.Fatalf("transcriptNote = %q before the Cmd ran, want %q", m.transcriptNote, "loading transcript...")
	}

	// Now run the Cmd, as a real tea.Program's event loop would, and feed its
	// resulting Msg back through Update.
	m = runCmd(m, cmd)

	if !m.transcriptLoaded {
		t.Fatal("transcriptLoaded = false, want true after the load Cmd resolved")
	}
	if m.transcript == nil {
		t.Fatalf("transcript = nil, want a loaded *agent.Transcript (note=%q)", m.transcriptNote)
	}
	if len(m.transcript.Events) != 2 {
		t.Fatalf("loaded %d events, want 2", len(m.transcript.Events))
	}

	view := stripANSI(m.View())
	for _, want := range []string{"investigating the nil check", "read", "func foo"} {
		if !strings.Contains(view, want) {
			t.Errorf("agent detail view missing %q from rendered transcript, got:\n%s", want, view)
		}
	}
}
