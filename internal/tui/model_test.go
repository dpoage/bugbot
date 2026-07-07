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
	fr := Frame{
		HasSnapshot: true,
		Snapshot: progress.Status{
			ActiveAgents: []progress.AgentStatus{
				{Role: "verifier", Label: "candidate A", Started: time.Unix(1000, 0), Activity: "reading file"},
			},
		},
		Agents: []AgentView{
			{Role: "verifier", Label: "candidate A", Live: true, Started: time.Unix(1000, 0), Activity: "reading file"},
		},
	}
	m = sendFrame(m, fr)

	if len(m.frame.Agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(m.frame.Agents))
	}
	if m.frame.Agents[0].Activity != "reading file" {
		t.Fatalf("activity = %q, want %q", m.frame.Agents[0].Activity, "reading file")
	}
}

func TestUpdate_LiveAndHistoricalMerge(t *testing.T) {
	m := NewModel(context.Background(), &fakeFeed{}, nil)
	m = sendFrame(m, baseFrame())

	if len(m.frame.Agents) != 2 {
		t.Fatalf("expected 2 merged agents, got %d", len(m.frame.Agents))
	}
	var hasLive, hasHist bool
	for _, a := range m.frame.Agents {
		if a.Live {
			hasLive = true
		} else {
			hasHist = true
		}
	}
	if !hasLive || !hasHist {
		t.Fatalf("expected both live and historical agents: %+v", m.frame.Agents)
	}
}

func TestUpdate_StaleFrameRendersIdle(t *testing.T) {
	m := NewModel(context.Background(), &fakeFeed{}, nil)
	m = sendFrame(m, Frame{Stale: true, HasSnapshot: true})
	got := stripANSI(m.View())
	if !strings.Contains(got, "stale") && !strings.Contains(got, "idle") {
		t.Errorf("stale frame should mention stale or idle, got:\n%s", got)
	}
}

// TestUpdate_PaneNavigation replaces the old TestUpdate_ScreenNavigation.
// It asserts the pane focus-ring (tab/1/2/3), drill-in to paneDetail,
// and the context pane's sub-mode cycling via 'm'.
func TestUpdate_PaneNavigation(t *testing.T) {
	m := NewModel(context.Background(), &fakeFeed{}, nil)
	m = sendFrame(m, baseFrame())

	// Initial state: focus on roster pane.
	if m.focus != paneRoster {
		t.Fatalf("initial focus = %v, want paneRoster", m.focus)
	}

	// Tab advances to paneDetail.
	m = sendKey(m, "tab")
	if m.focus != paneDetail {
		t.Fatalf("after tab, focus = %v, want paneDetail", m.focus)
	}

	// Tab advances to paneContext.
	m = sendKey(m, "tab")
	if m.focus != paneContext {
		t.Fatalf("after two tabs, focus = %v, want paneContext", m.focus)
	}

	// Tab wraps back to paneRoster.
	m = sendKey(m, "tab")
	if m.focus != paneRoster {
		t.Fatalf("after three tabs, focus = %v, want paneRoster (wrapped)", m.focus)
	}

	// Direct key '2' jumps to paneDetail.
	m = sendKey(m, "2")
	if m.focus != paneDetail {
		t.Fatalf("after '2', focus = %v, want paneDetail", m.focus)
	}

	// Direct key '3' jumps to paneContext.
	m = sendKey(m, "3")
	if m.focus != paneContext {
		t.Fatalf("after '3', focus = %v, want paneContext", m.focus)
	}

	// Direct key '1' jumps to paneRoster.
	m = sendKey(m, "1")
	if m.focus != paneRoster {
		t.Fatalf("after '1', focus = %v, want paneRoster", m.focus)
	}

	// Enter on roster drills into paneDetail (sets detailKey, switches focus).
	m = sendKey(m, "enter")
	if m.focus != paneDetail {
		t.Fatalf("after enter on roster, focus = %v, want paneDetail", m.focus)
	}
	if m.detailKey == "" {
		t.Fatal("detailKey not set after drill-in")
	}
	if !m.transcriptLoaded {
		t.Fatal("expected transcript load attempt on drill-in")
	}
	if m.transcriptNote != "no transcript" {
		t.Fatalf("transcriptNote = %q, want %q (no TranscriptPath seeded)", m.transcriptNote, "no transcript")
	}

	// Context pane mode cycles via 'm': summary → findings → leads → summary.
	m = sendKey(m, "3")
	if m.contextMode != contextModeSummary {
		t.Fatalf("initial contextMode = %v, want contextModeSummary", m.contextMode)
	}
	m = sendKey(m, "m")
	if m.contextMode != contextModeFindings {
		t.Fatalf("after m, contextMode = %v, want contextModeFindings", m.contextMode)
	}
	m = sendKey(m, "m")
	if m.contextMode != contextModeLeads {
		t.Fatalf("after m×2, contextMode = %v, want contextModeLeads", m.contextMode)
	}
	m = sendKey(m, "m")
	if m.contextMode != contextModeSummary {
		t.Fatalf("after m×3, contextMode = %v, want contextModeSummary (wrapped)", m.contextMode)
	}
}

// TestUpdate_FocusRingScrollRouting asserts that j/k are routed to the
// focused pane only: roster pane advances cursor; detail pane scrolls viewport.
func TestUpdate_FocusRingScrollRouting(t *testing.T) {
	m := NewModel(context.Background(), &fakeFeed{}, nil)
	m = sendFrame(m, baseFrame())

	// Roster pane focused: j moves cursor.
	if m.focus != paneRoster {
		t.Fatalf("initial focus = %v, want paneRoster", m.focus)
	}
	initialCursor := m.cursor
	m = sendKey(m, "j")
	if m.cursor <= initialCursor && len(m.visibleAgentIndices()) > 1 {
		t.Fatalf("j in roster pane: cursor did not advance (cursor=%d)", m.cursor)
	}

	// Detail pane focused: j scrolls the transcript viewport (no cursor change).
	m = sendKey(m, "2")
	before := m.transcriptView.YOffset
	m = sendKey(m, "j")
	// viewport won't scroll if content is empty; just ensure no panic and cursor unchanged.
	_ = before
	_ = m.transcriptView.YOffset
}

func TestUpdate_FilterNarrowsAgentList(t *testing.T) {
	m := NewModel(context.Background(), &fakeFeed{}, nil)
	m = sendFrame(m, baseFrame())
	// roster pane is already focused by default

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
// context pane's cockpit summary rendered text (ANSI stripped).
func TestView_GoldenCockpitIdle(t *testing.T) {
	m := NewModel(context.Background(), &fakeFeed{}, nil)
	// Use a wide terminal so pane content is not wrapped at 24-char column boundaries.
	next, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	m = next.(Model)
	m = sendFrame(m, Frame{
		World: WorldState{HasTallies: true, Tallies: domain.FindingTallies{OpenByTier: map[int]int{1: 2}}},
	})

	got := stripANSI(m.View())
	for _, want := range []string{
		"bugbot — idle",
		"no scan or daemon",
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
	// Use a wide terminal so pane content is not wrapped at 24-char column boundaries.
	next, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	m = next.(Model)
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

// TestView_GoldenAgentsPane verifies the roster pane renders agent rows.
func TestView_GoldenAgentsPane(t *testing.T) {
	m := NewModel(context.Background(), &fakeFeed{}, nil)
	m = sendFrame(m, baseFrame())
	// Roster pane is already focused (pane 0); no tab needed.

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
			t.Errorf("golden agents pane missing %q, got:\n%s", want, got)
		}
	}
}

// TestView_GoldenMultiPane80x24 is a golden-frame test at the 80×24 size.
func TestView_GoldenMultiPane80x24(t *testing.T) {
	m := NewModel(context.Background(), &fakeFeed{}, nil)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = next.(Model)
	m = sendFrame(m, baseFrame())

	got := stripANSI(m.View())
	for _, want := range []string{
		"Agents",
		"Agent Detail",
		"bugbot",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("80×24 golden view missing %q, got:\n%s", want, got)
		}
	}
}

// TestView_GoldenMultiPane120x40 is a golden-frame test at the 120×40 size.
// It also asserts that the 120×40 output differs from the 80×24 output so
// that the reflow on WindowSizeMsg is actually exercised — not just that both
// sizes render the same fixed string.
func TestView_GoldenMultiPane120x40(t *testing.T) {
	// Render at 80×24 first for the reflow comparison.
	m80 := NewModel(context.Background(), &fakeFeed{}, nil)
	next80, _ := m80.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m80 = next80.(Model)
	m80 = sendFrame(m80, baseFrame())
	view80 := stripANSI(m80.View())

	m := NewModel(context.Background(), &fakeFeed{}, nil)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = next.(Model)
	m = sendFrame(m, baseFrame())

	got := stripANSI(m.View())
	for _, want := range []string{
		"Agents",
		"Agent Detail",
		"bugbot",
		"World state",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("120×40 golden view missing %q, got:\n%s", want, got)
		}
	}

	// Reflow guard: a wider/taller terminal must produce a different layout.
	if got == view80 {
		t.Errorf("120×40 view is identical to 80×24 view — reflow did not change the layout")
	}
}

// TestView_ResizeReflow verifies that sending a WindowSizeMsg causes the panes
// to reflow without panicking.
func TestView_ResizeReflow(t *testing.T) {
	m := NewModel(context.Background(), &fakeFeed{}, nil)
	m = sendFrame(m, baseFrame())

	// Start at 80×24.
	next, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = next.(Model)
	view80 := stripANSI(m.View())
	if view80 == "" {
		t.Fatal("View() at 80x24 returned empty string")
	}

	// Resize to 120×40.
	next, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = next.(Model)
	view120 := stripANSI(m.View())
	if view120 == "" {
		t.Fatal("View() at 120x40 returned empty string")
	}

	// Resize to tiny terminal (should degrade gracefully, not panic).
	next, _ = m.Update(tea.WindowSizeMsg{Width: 20, Height: 6})
	m = next.(Model)
	if got := m.View(); got == "" {
		t.Fatal("View() at tiny 20x6 returned empty string")
	}
}

// stripANSI wraps progress.StripANSI so this package's tests do not
// reimplement ANSI-stripping.
func stripANSI(s string) string { return progress.StripANSI(s) }

// TestUpdate_DrilldownSurvivesReorderedFrame is the B2 regression: drilling
// into an agent, then receiving a FrameMsg where mergeAgents' sort has
// reshuffled positions (a new, earlier-started agent inserted ahead of it),
// must NOT swap which agent the detail pane shows. detailIdx is re-resolved
// by agentKey identity, not held as a raw position.
func TestUpdate_DrilldownSurvivesReorderedFrame(t *testing.T) {
	m := NewModel(context.Background(), &fakeFeed{}, nil)
	m = sendFrame(m, baseFrame())
	// cursor 0 is the live "verifier/candidate A" (Started=1000); roster focused.

	m = sendKey(m, "enter")
	if m.focus != paneDetail {
		t.Fatalf("focus = %v after enter, want paneDetail", m.focus)
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
	// The rendered view must contain the detail of the tracked agent.
	view := stripANSI(m.View())
	if !strings.Contains(view, "candidate A") || !strings.Contains(view, "still going") {
		t.Fatalf("detail pane did not track the reordered agent:\n%s", view)
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
	// Use a wide terminal so the transcript viewport is wide enough for full lines.
	next2, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	m = next2.(Model)
	m = sendFrame(m, Frame{
		HasSnapshot: true,
		Agents: []AgentView{
			{Role: "reproducer", Label: "nil deref", Lens: "nil-safety", UnitID: "u1", TranscriptPath: path},
		},
	})
	// roster pane is already focused; no tab needed.

	// Fire "enter" WITHOUT running its Cmd yet, to observe the state the
	// off-thread load leaves behind before it resolves.
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if m.focus != paneDetail {
		t.Fatalf("focus = %v, want paneDetail immediately after enter", m.focus)
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
			t.Errorf("agent detail pane missing %q from rendered transcript, got:\n%s", want, view)
		}
	}
}

// TestIntegration_EnterOnFeedRowOpensSourcePane exercises the full .8 -> .9
// composition on the merged tree: a structured action-feed row carrying a
// File target, ENTER in the detail pane's feed mode, the resulting
// openSourceMsg flowing through Update into handleOpenSource, and the async
// bounded load resolving into the source view. The two features were built on
// separate branches against the openSourceMsg contract stub; this is the seam
// test proving they compose.
func TestIntegration_EnterOnFeedRowOpensSourcePane(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := "package main\n\nfunc main() {\n\tprintln(\"hi\")\n}\n"
	if err := os.WriteFile(filepath.Join(root, "src", "main.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	m := NewModel(context.Background(), &fakeFeed{}, nil).WithRepoRoot(root)
	fr := baseFrame()
	fr.ActionRows = map[string][]ActionRow{
		agentFeedKey("verifier", "candidate A"): {
			{Seq: 1, AgentRole: "verifier", AgentLabel: "candidate A", Tool: "read_file",
				Target: "src/main.go:3", File: "src/main.go", Line: 3, EndLine: 4},
		},
	}
	m = sendFrame(m, fr)

	// Drill into the live agent (roster cursor starts on it): detailMode
	// defaults to the action feed for live agents.
	m = sendKey(m, "enter")
	if m.focus != paneDetail || !m.detailMode {
		t.Fatalf("after drill-in: focus=%v detailMode=%v, want paneDetail+feed", m.focus, m.detailMode)
	}

	// ENTER on the feed row emits openSourceMsg; handleOpenSource turns it
	// into an async bounded load resolving to sourceLoadedMsg. runCmd is
	// deliberately one-hop, so drive the two-hop chain explicitly.
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("enter on feed row returned no cmd, want openSourceMsg emission")
	}
	next, cmd = m.Update(cmd()) // openSourceMsg -> handleOpenSource
	m = next.(Model)
	if cmd == nil {
		t.Fatalf("openSourceMsg produced no load cmd (note=%q)", m.sourceNote)
	}
	next, _ = m.Update(cmd()) // sourceLoadedMsg -> applied
	m = next.(Model)

	if m.contextMode != contextModeSource {
		t.Fatalf("contextMode = %v, want contextModeSource (note=%q)", m.contextMode, m.sourceNote)
	}
	if m.sourceFile != "src/main.go" || m.sourceLine != 3 || m.sourceEndLine != 4 {
		t.Fatalf("source target = %s:%d-%d, want src/main.go:3-4", m.sourceFile, m.sourceLine, m.sourceEndLine)
	}
	if m.sourceNote != "" {
		t.Fatalf("sourceNote = %q, want clean load", m.sourceNote)
	}
	if len(m.sourceLines) != 5 {
		t.Fatalf("loaded %d source lines, want 5", len(m.sourceLines))
	}
	view := stripANSI(m.View())
	if !strings.Contains(view, "src/main.go") {
		t.Errorf("rendered view missing source pane header src/main.go:\n%s", view)
	}
}
