package tui

import (
	"context"
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

// TestActionFeedState_ConcurrentAgentsSameLabel_CollisionRegression is the
// bugbot-r7ub regression: two event streams sharing (Role, Label) but with
// distinct AgentID must fold into DISTINCT rings, and a KindAgentFinished for
// one agent must prune only that agent's ring. Before feedKeyForEvent (which
// falls back to agentFeedKey only when AgentID is empty), both streams
// collided into a single ring keyed by role+label, so this test's ring-count
// assertions fail on pre-fix code.
func TestActionFeedState_ConcurrentAgentsSameLabel_CollisionRegression(t *testing.T) {
	s := newActionFeedState()

	const role, label = "reproducer", "duplicate open finding"
	evA := toolCallStart(role, label, "read_file", "a.go", 1, 0, "", "")
	evA.AgentID = "agent-A"
	evB := toolCallStart(role, label, "read_file", "b.go", 1, 0, "", "")
	evB.AgentID = "agent-B"

	s.ApplyToolCallEvent(evA)
	s.ApplyToolCallEvent(evB)

	if len(s.perAgent) != 2 {
		t.Fatalf("perAgent = %d rings, want 2 distinct rings for colliding role+label", len(s.perAgent))
	}
	keyA, keyB := feedKeyForEvent(evA), feedKeyForEvent(evB)
	if keyA == keyB {
		t.Fatalf("feedKeyForEvent produced identical keys for distinct AgentIDs: %q", keyA)
	}
	rowsA := s.VisibleRows(keyA)
	if len(rowsA) != 1 || rowsA[0].File != "a.go" {
		t.Fatalf("agent-A ring = %+v, want single row for a.go", rowsA)
	}
	rowsB := s.VisibleRows(keyB)
	if len(rowsB) != 1 || rowsB[0].File != "b.go" {
		t.Fatalf("agent-B ring = %+v, want single row for b.go", rowsB)
	}

	// KindAgentFinished for agent-A must prune ONLY agent-A's ring.
	finishedA := progress.Event{Kind: progress.KindAgentFinished, Role: role, Label: label, AgentID: "agent-A"}
	s.PruneAgent(feedKeyForEvent(finishedA))
	if _, ok := s.perAgent[keyA]; ok {
		t.Error("agent-A ring should be pruned")
	}
	if _, ok := s.perAgent[keyB]; !ok {
		t.Error("agent-B ring must survive agent-A's finish — same-label sibling wiped out")
	}
}

// TestActionFeedState_EmptyAgentID_FoldsUnderRoleLabel verifies the
// backward-compatibility fallback: events from a pre-identity emitter (no
// AgentID set, e.g. an old-daemon Attach connection) still fold into ONE
// ring keyed by role+label, exactly like before this change.
func TestActionFeedState_EmptyAgentID_FoldsUnderRoleLabel(t *testing.T) {
	s := newActionFeedState()

	ev1 := toolCallStart("finder", "lensX", "grep", "", 0, 0, "", "foo")
	ev2 := toolCallStart("finder", "lensX", "read_file", "x.go", 1, 0, "", "")
	// Both events deliberately carry no AgentID.

	s.ApplyToolCallEvent(ev1)
	s.ApplyToolCallEvent(ev2)

	if len(s.perAgent) != 1 {
		t.Fatalf("perAgent = %d rings, want 1 (pre-identity events fold under role+label)", len(s.perAgent))
	}
	k := agentFeedKey("finder", "lensX")
	rows := s.VisibleRows(k)
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows folded under role+label fallback key, got %d", len(rows))
	}
}

// ── Tool glyph/color coverage ─────────────────────────────────────────────────

func TestToolGlyphs_AllTools(t *testing.T) {
	tools := []string{"grep", "read_file", "read_symbol", "find_references", "list_dir",
		"run_tests", "sandbox_exec", "status_note", "summarize_package", "unknown_tool"}
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

// TestToolGlyph_SummarizePackage verifies that summarize_package renders with
// its dedicated glyph (🗺) and a non-default color, and that the rendered row
// is non-empty for both in-flight and resolved states.
func TestToolGlyph_SummarizePackage(t *testing.T) {
	const tool = "summarize_package"

	g := toolGlyph(tool)
	if g != "🗺" {
		t.Errorf("toolGlyph(%q) = %q, want 🗺", tool, g)
	}

	color := toolColor(tool)
	if color == toolColor("unknown_tool") {
		t.Errorf("toolColor(%q) returned the default/fallback color; want a dedicated color", tool)
	}

	// Render in-flight row.
	inFlight := ActionRow{Tool: tool, Target: "internal/funnel [3 files]", InFlight: true}
	rendered := renderActionRow(inFlight, "⠋", 80)
	if rendered == "" {
		t.Errorf("renderActionRow(%q, in-flight) returned empty string", tool)
	}

	// Render resolved row.
	resolved := ActionRow{Tool: tool, Target: "internal/funnel [3 files]", InFlight: false, Count: 3}
	rendered = renderActionRow(resolved, "⠋", 80)
	if rendered == "" {
		t.Errorf("renderActionRow(%q, resolved) returned empty string", tool)
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
	m := NewModel(context.Background(), &fakeFeed{mode: Owner}, nil)
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
	m = sendKey(m, "A")
	if !m.actionFeed.showAggregate {
		t.Fatal("expected aggregate mode after 'A'")
	}
	m = sendKey(m, "A")
	if m.actionFeed.showAggregate {
		t.Fatal("expected per-agent mode after second 'A'")
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
	m := NewModel(context.Background(), &fakeFeed{mode: Owner}, nil)
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

// ── View()-level regression tests (B1: per-agent render, B2: aggregate render) ──

// TestView_ActionFeedRowAppearsInDetailPane drills into a live agent whose
// ActionRows frame carries a read_file row and asserts the target string is
// visible in View() output. This is the exact regression that let B1 slip past
// the 22 green unit tests: per-agent ring was keyed by agentFeedKey but
// renderActionFeed was called with m.detailKey (wrong format), producing an
// empty feed pane in the real UI.
func TestView_ActionFeedRowAppearsInDetailPane(t *testing.T) {
	m := NewModel(context.Background(), &fakeFeed{mode: Owner}, nil)
	m.width = 120
	m.height = 40

	fr := baseFrame() // verifier "candidate A" is live
	k := agentFeedKey("verifier", "candidate A")
	fr.ActionRows = map[string][]ActionRow{
		k: {
			{
				Seq:      1,
				Tool:     "read_file",
				Target:   "internal/foo.go:10-40",
				File:     "internal/foo.go",
				Line:     10,
				EndLine:  40,
				InFlight: false,
				Count:    30,
			},
		},
	}
	m = sendFrame(m, fr)
	// drill in — sets detailMode=true (live agent), detailIdx=0
	m = sendKey(m, "enter")

	if !m.detailMode {
		t.Fatal("detailMode should be true after drilling into live agent")
	}

	view := stripANSI(m.View())
	if !strings.Contains(view, "internal/foo.go") {
		t.Errorf("View() should contain row target 'internal/foo.go' in action feed; got:\n%s", view)
	}
}

// TestView_AggregateActionFeedAppearsAfterAToggle feeds two agents' ActionRows,
// toggles 'A', and asserts both agents' rows appear in View(). This is the
// regression for B2: aggregate was never populated on the Model side, so 'A'
// always showed an empty pane.
func TestView_AggregateActionFeedAppearsAfterAToggle(t *testing.T) {
	m := NewModel(context.Background(), &fakeFeed{mode: Owner}, nil)
	m.width = 120
	m.height = 40

	fr := Frame{
		HasSnapshot: true,
		Snapshot: progress.Status{
			ScanKind: "sweep",
			Stage:    "verify",
		},
		// Agents must be set directly; tests bypass Feed.buildFrame.
		Agents: []AgentView{
			{Role: "verifier", Label: "candidate A", Live: true, Started: time.Unix(999, 0)},
			{Role: "finder", Label: "lens1", Live: true, Started: time.Unix(998, 0)},
		},
	}
	k1 := agentFeedKey("verifier", "candidate A")
	k2 := agentFeedKey("finder", "lens1")
	fr.ActionRows = map[string][]ActionRow{
		k1: {{Seq: 1, Tool: "read_file", Target: "alpha.go:1", File: "alpha.go", Line: 1, InFlight: false, Count: 5}},
		k2: {{Seq: 2, Tool: "grep", Target: `"betaPattern"`, Pattern: "betaPattern", InFlight: false, Count: 2}},
	}
	m = sendFrame(m, fr)
	// drill into first agent
	m = sendKey(m, "enter")
	// toggle aggregate with 'A'
	m = sendKey(m, "A")

	if !m.actionFeed.showAggregate {
		t.Fatal("showAggregate should be true after 'A'")
	}

	view := stripANSI(m.View())
	if !strings.Contains(view, "alpha.go") {
		t.Errorf("aggregate feed should contain 'alpha.go'; got:\n%s", view)
	}
	if !strings.Contains(view, "betaPattern") {
		t.Errorf("aggregate feed should contain 'betaPattern'; got:\n%s", view)
	}
}

// TestModel_PruneOnAgentFinished asserts that ring count drops when an agent's
// key is pruned (B3 regression: rings were never removed, leaking 128 rows
// per finished agent indefinitely).
func TestModel_PruneOnAgentFinished(t *testing.T) {
	m := NewModel(context.Background(), &fakeFeed{mode: Owner}, nil)
	fr := baseFrame()
	k := agentFeedKey("verifier", "candidate A")
	fr.ActionRows = map[string][]ActionRow{
		k: {{Seq: 1, Tool: "grep", Pattern: "TODO", InFlight: false, Count: 3}},
	}
	m = sendFrame(m, fr)
	if _, ok := m.actionFeed.perAgent[k]; !ok {
		t.Fatal("ring should exist after first frame")
	}

	// Next frame: ActionRows no longer contains the agent (it finished).
	fr2 := baseFrame()
	fr2.ActionRows = map[string][]ActionRow{} // empty: agent gone
	m = sendFrame(m, fr2)

	if _, ok := m.actionFeed.perAgent[k]; ok {
		t.Error("ring should have been pruned when agent disappeared from ActionRows")
	}
}

// TestActionFeedState_PruneAgent asserts PruneAgent removes the ring and
// RebuildAggregate reflects the removal.
func TestActionFeedState_PruneAgent(t *testing.T) {
	s := newActionFeedState()
	s.ApplyToolCallEvent(toolCallStart("r", "l1", "grep", "", 0, 0, "", "a"))
	s.ApplyToolCallEvent(toolCallStart("r", "l2", "read_file", "foo.go", 1, 0, "", ""))
	s.RebuildAggregate()

	if len(s.aggregate.Rows()) != 2 {
		t.Fatalf("before prune: aggregate has %d rows, want 2", len(s.aggregate.Rows()))
	}

	s.PruneAgent(agentFeedKey("r", "l1"))
	if _, ok := s.perAgent[agentFeedKey("r", "l1")]; ok {
		t.Error("l1 ring should be gone after prune")
	}
	agg := s.aggregate.Rows()
	if len(agg) != 1 {
		t.Errorf("after prune: aggregate has %d rows, want 1", len(agg))
	}
	if agg[0].Tool != "read_file" {
		t.Errorf("remaining aggregate row should be read_file, got %q", agg[0].Tool)
	}
}
