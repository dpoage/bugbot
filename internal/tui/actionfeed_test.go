package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/dpoage/bugbot/internal/progress"
)

// ── helpers ──────────────────────────────────────────────────────────────────

func toolCallStart(role, label, tool, file string, line, endLine int, symbol, pattern string) progress.Event {
	return progress.Event{
		Kind:    progress.KindToolCall,
		Phase:   "start",
		Role:    role,
		Label:   label,
		Tool:    tool,
		File:    file,
		Line:    line,
		EndLine: endLine,
		Symbol:  symbol,
		Pattern: pattern,
		Time:    time.Now(),
	}
}

func toolCallDone(role, label, tool string, count int, err string) progress.Event {
	return progress.Event{
		Kind:  progress.KindToolCall,
		Phase: "done",
		Role:  role,
		Label: label,
		Tool:  tool,
		Count: count,
		Err:   err,
		Time:  time.Now(),
	}
}

// ── actionRing tests ──────────────────────────────────────────────────────────

func TestActionRing_StartDonePairing(t *testing.T) {
	r := newActionRing()
	ev := toolCallStart("finder", "lens1", "grep", "", 0, 0, "", "TODO")
	r.ApplyStart(ev)

	rows := r.Rows()
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if !rows[0].InFlight {
		t.Fatal("expected row to be in-flight after start")
	}

	r.ApplyDone(toolCallDone("finder", "lens1", "grep", 5, ""))
	rows = r.Rows()
	if len(rows) != 1 {
		t.Fatalf("expected 1 row after done, got %d", len(rows))
	}
	if rows[0].InFlight {
		t.Fatal("expected row to be resolved after done")
	}
	if rows[0].Count != 5 {
		t.Fatalf("expected Count=5, got %d", rows[0].Count)
	}
}

func TestActionRing_ErrorRow(t *testing.T) {
	r := newActionRing()
	r.ApplyStart(toolCallStart("finder", "l", "sandbox_exec", "", 0, 0, "", ""))
	r.ApplyDone(toolCallDone("finder", "l", "sandbox_exec", 0, "exit 1"))

	rows := r.Rows()
	if len(rows) != 1 {
		t.Fatalf("got %d rows", len(rows))
	}
	if rows[0].Err != "exit 1" {
		t.Fatalf("expected Err='exit 1', got %q", rows[0].Err)
	}
	rendered := renderActionRow(rows[0], "⠋", 80)
	if !strings.Contains(rendered, "err:") {
		t.Fatalf("rendered row should contain 'err:'; got: %s", rendered)
	}
}

func TestActionRing_OrphanDone(t *testing.T) {
	r := newActionRing()
	// done arrives without a prior start (attach mid-run)
	r.ApplyDone(toolCallDone("finder", "l", "read_file", 30, ""))

	rows := r.Rows()
	if len(rows) != 1 {
		t.Fatalf("expected 1 standalone row, got %d", len(rows))
	}
	if rows[0].InFlight {
		t.Fatal("orphan done should not be in-flight")
	}
	if rows[0].Count != 30 {
		t.Fatalf("expected Count=30, got %d", rows[0].Count)
	}
}

func TestActionRing_BoundedAtCap(t *testing.T) {
	r := newActionRing()
	// push more than cap
	for i := 0; i < actionRingCap+10; i++ {
		r.ApplyStart(toolCallStart("r", "l", "grep", "", 0, 0, "", fmt.Sprintf("pat%d", i)))
	}
	rows := r.Rows()
	if len(rows) != actionRingCap {
		t.Fatalf("expected %d rows (cap), got %d", actionRingCap, len(rows))
	}
	// newest retained: the last pushed pattern (raw field, not quoted form)
	lastPat := rows[len(rows)-1].Pattern
	expectedLast := fmt.Sprintf("pat%d", actionRingCap+9)
	if lastPat != expectedLast {
		t.Fatalf("last row Pattern = %q, want %q", lastPat, expectedLast)
	}
}

func TestActionRing_MultipleStartDonePairsInOrder(t *testing.T) {
	r := newActionRing()
	r.ApplyStart(toolCallStart("r", "l", "grep", "", 0, 0, "", "a"))
	r.ApplyStart(toolCallStart("r", "l", "grep", "", 0, 0, "", "b"))

	// resolve first one
	r.ApplyDone(toolCallDone("r", "l", "grep", 3, ""))
	rows := r.Rows()
	// first row resolved, second still in-flight
	if rows[0].InFlight {
		t.Fatal("first row should be resolved")
	}
	if !rows[1].InFlight {
		t.Fatal("second row should still be in-flight")
	}
	if rows[0].Count != 3 {
		t.Fatalf("first row Count=%d, want 3", rows[0].Count)
	}
}

// ── ActionFeedState tests ─────────────────────────────────────────────────────

func TestActionFeedState_PerAgentVsAggregate(t *testing.T) {
	s := newActionFeedState()

	s.ApplyToolCallEvent(toolCallStart("finder", "lens1", "grep", "", 0, 0, "", "TODO"))
	s.ApplyToolCallEvent(toolCallStart("verifier", "cand1", "read_file", "foo.go", 1, 10, "", ""))

	// per-agent: only finder rows
	k1 := agentFeedKey("finder", "lens1")
	rows := s.VisibleRows(k1)
	if len(rows) != 1 {
		t.Fatalf("per-agent finder: expected 1 row, got %d", len(rows))
	}

	// aggregate toggle
	s.showAggregate = true
	agg := s.VisibleRows(k1)
	if len(agg) != 2 {
		t.Fatalf("aggregate: expected 2 rows, got %d", len(agg))
	}
}

func TestActionFeedState_AggregateBounded(t *testing.T) {
	s := newActionFeedState()
	for i := 0; i < actionRingCap+5; i++ {
		s.ApplyToolCallEvent(toolCallStart("r", "l", "grep", "", 0, 0, "", fmt.Sprintf("p%d", i)))
	}
	s.showAggregate = true
	rows := s.VisibleRows("")
	if len(rows) != actionRingCap {
		t.Fatalf("aggregate bounded: expected %d rows, got %d", actionRingCap, len(rows))
	}
}

// ── Tool glyph/color coverage ─────────────────────────────────────────────────

func TestToolGlyphs_AllTools(t *testing.T) {
	tools := []string{"grep", "read_file", "read_symbol", "find_references", "list_dir",
		"run_tests", "sandbox_exec", "status_note", "unknown_tool"}
	for _, tool := range tools {
		g := toolGlyph(tool)
		if g == "" {
			t.Errorf("toolGlyph(%q) returned empty string", tool)
		}
		// render should not panic
		row := ActionRow{Tool: tool, Target: "test", InFlight: true}
		rendered := renderActionRow(row, "⠋", 80)
		if rendered == "" {
			t.Errorf("renderActionRow for tool %q returned empty string", tool)
		}
	}
}

// ── openSourceMsg emission ────────────────────────────────────────────────────

func TestEnterOnFeedRow_EmitsOpenSourceMsg(t *testing.T) {
	row := ActionRow{
		Tool:    "read_file",
		File:    "pkg/foo.go",
		Line:    10,
		EndLine: 40,
		Pattern: "",
	}
	cmd := enterOnFeedRow(row)
	if cmd == nil {
		t.Fatal("expected non-nil cmd for row with File")
	}
	msg := cmd()
	osm, ok := msg.(openSourceMsg)
	if !ok {
		t.Fatalf("expected openSourceMsg, got %T", msg)
	}
	if osm.File != "pkg/foo.go" {
		t.Errorf("File=%q, want pkg/foo.go", osm.File)
	}
	if osm.Line != 10 || osm.EndLine != 40 {
		t.Errorf("Line=%d EndLine=%d, want 10,40", osm.Line, osm.EndLine)
	}
}

func TestEnterOnFeedRow_PatternOnly(t *testing.T) {
	row := ActionRow{Tool: "grep", Pattern: "TODO"}
	cmd := enterOnFeedRow(row)
	if cmd == nil {
		t.Fatal("expected non-nil cmd for row with Pattern")
	}
	msg := cmd()
	osm, ok := msg.(openSourceMsg)
	if !ok {
		t.Fatalf("expected openSourceMsg, got %T", msg)
	}
	if osm.Pattern != "TODO" {
		t.Errorf("Pattern=%q, want TODO", osm.Pattern)
	}
}

func TestEnterOnFeedRow_NoFileNoPattern_Nil(t *testing.T) {
	row := ActionRow{Tool: "status_note", Target: "checking"}
	cmd := enterOnFeedRow(row)
	if cmd != nil {
		t.Fatal("expected nil cmd for row with no File or Pattern")
	}
}

func TestEnterOnFeedRow_ObserverRow_Nil(t *testing.T) {
	row := ActionRow{IsObserver: true, ObserverText: "read_file foo.go"}
	cmd := enterOnFeedRow(row)
	if cmd != nil {
		t.Fatal("expected nil cmd for observer row")
	}
}

// ── Observer path ─────────────────────────────────────────────────────────────

func TestRenderActionFeed_ObserverRecentActions(t *testing.T) {
	state := newActionFeedState()
	recentActions := []string{"grep \"TODO\" [done, 3 hits]", "read_file foo.go:1-10"}
	out := renderActionFeed(state, 80, 20, "", recentActions, false)
	if !strings.Contains(out, "grep") {
		t.Errorf("observer feed should contain 'grep'; got:\n%s", out)
	}
	if !strings.Contains(out, "read_file") {
		t.Errorf("observer feed should contain 'read_file'; got:\n%s", out)
	}
}

// ── Model-level integration tests ─────────────────────────────────────────────

func newTestModelWithAgent() Model {
	m := NewModel(nil, &fakeFeed{mode: Owner}, nil)
	fr := baseFrame()
	m = sendFrame(m, fr)
	// drill into first agent
	m = sendKey(m, "enter")
	return m
}

func TestModel_ActionFeedToggle(t *testing.T) {
	m := newTestModelWithAgent()
	// detailMode starts true (action feed)
	if !m.detailMode {
		t.Fatal("expected detailMode=true initially")
	}
	// focus must be paneDetail after enter
	if m.focus != paneDetail {
		t.Fatalf("expected paneDetail focus, got %v", m.focus)
	}
	// 'a' toggles to transcript
	m = sendKey(m, "a")
	if m.detailMode {
		t.Fatal("expected detailMode=false after 'a'")
	}
	// 'a' again back to feed
	m = sendKey(m, "a")
	if !m.detailMode {
		t.Fatal("expected detailMode=true after second 'a'")
	}
}

func TestModel_ActionFeedAggregatToggle(t *testing.T) {
	m := newTestModelWithAgent()
	if m.actionFeed.showAggregate {
		t.Fatal("expected per-agent mode initially")
	}
	m = sendKey(m, "g")
	if !m.actionFeed.showAggregate {
		t.Fatal("expected aggregate mode after 'g'")
	}
	m = sendKey(m, "g")
	if m.actionFeed.showAggregate {
		t.Fatal("expected per-agent mode after second 'g'")
	}
}

func TestModel_EnterOnFeedRowEmitsCmd(t *testing.T) {
	m := newTestModelWithAgent()
	// inject a tool-call event directly into actionFeed
	ev := toolCallStart("verifier", "candidate A", "read_file", "pkg/foo.go", 5, 20, "", "")
	m.actionFeed.ApplyToolCallEvent(ev)
	m.actionFeed.cursor = 0

	// frame detailKey must be set
	if m.detailKey == "" {
		t.Skip("no detailKey set")
	}

	// ENTER should emit openSourceMsg
	var cmd tea.Cmd
	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected a tea.Cmd on enter with file-bearing row")
	}
	msg := cmd()
	osm, ok := msg.(openSourceMsg)
	if !ok {
		t.Fatalf("expected openSourceMsg, got %T", msg)
	}
	if osm.File != "pkg/foo.go" {
		t.Errorf("File=%q, want pkg/foo.go", osm.File)
	}
}

func TestModel_ActionFeedScrolling(t *testing.T) {
	m := newTestModelWithAgent()
	// add several rows
	for i := 0; i < 5; i++ {
		ev := toolCallStart("verifier", "candidate A", "read_file", fmt.Sprintf("f%d.go", i), i+1, 0, "", "")
		m.actionFeed.ApplyToolCallEvent(ev)
	}
	// cursor at 0; scrollDown should move it
	m = sendKey(m, "j")
	if m.actionFeed.cursor != 1 {
		t.Fatalf("cursor after j: got %d, want 1", m.actionFeed.cursor)
	}
	m = sendKey(m, "k")
	if m.actionFeed.cursor != 0 {
		t.Fatalf("cursor after k: got %d, want 0", m.actionFeed.cursor)
	}
}

func TestModel_FrameSyncActionRows(t *testing.T) {
	m := NewModel(nil, &fakeFeed{mode: Owner}, nil)
	// Build a frame with ActionRows
	fr := baseFrame()
	k := agentFeedKey("verifier", "candidate A")
	fr.ActionRows = map[string][]ActionRow{
		k: {
			{Seq: 1, Tool: "grep", Pattern: "TODO", InFlight: false, Count: 3},
		},
	}
	m = sendFrame(m, fr)
	ring, ok := m.actionFeed.perAgent[k]
	if !ok {
		t.Fatal("expected ring for agent key after frame sync")
	}
	rows := ring.Rows()
	if len(rows) != 1 {
		t.Fatalf("expected 1 row in synced ring, got %d", len(rows))
	}
	if rows[0].Count != 3 {
		t.Errorf("Count=%d, want 3", rows[0].Count)
	}
}
