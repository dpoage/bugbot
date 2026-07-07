package tui

// actionfeed.go: live scrolling action feed for KindToolCall events.
//
// The feed renders as a view mode in the detail pane, toggled with 'a'.
// 'g' toggles between per-agent and aggregate-all-agents view.
// j/k scroll; ENTER on a row with File or Pattern emits openSourceMsg.

import (
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/dpoage/bugbot/internal/progress"
)

// sortActionRowsBySeq sorts rows in place by ascending Seq (chronological).
func sortActionRowsBySeq(rows []ActionRow) {
	sort.Slice(rows, func(i, j int) bool { return rows[i].Seq < rows[j].Seq })
}

// actionRingCap is the bounded ring size per-agent and for the aggregate.
const actionRingCap = 128

// spinnerFrames cycles through for in-flight rows.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// globalSeq is a process-wide monotonic counter for ActionRow.Seq.
var globalSeq atomic.Uint64

func nextSeq() uint64 { return globalSeq.Add(1) }

// toolGlyph returns the single-character glyph for a tool name.
func toolGlyph(tool string) string {
	switch {
	case tool == "grep":
		return "⌕"
	case tool == "read_file":
		return "📄"
	case tool == "read_symbol":
		return "🔍"
	case strings.HasPrefix(tool, "find_"):
		return "🔎"
	case tool == "list_dir":
		return "📁"
	case tool == "run_tests":
		return "🧪"
	case tool == "sandbox_exec":
		return "⚙"
	case tool == "status_note":
		return "📝"
	default:
		return "▶"
	}
}

// toolColor returns a lipgloss color for a tool name.
func toolColor(tool string) lipgloss.Color {
	switch {
	case tool == "grep":
		return lipgloss.Color("6") // cyan
	case tool == "read_file":
		return lipgloss.Color("4") // blue
	case tool == "read_symbol":
		return lipgloss.Color("5") // magenta
	case strings.HasPrefix(tool, "find_"):
		return lipgloss.Color("3") // yellow
	case tool == "list_dir":
		return lipgloss.Color("2") // green
	case tool == "run_tests":
		return lipgloss.Color("11") // bright yellow
	case tool == "sandbox_exec":
		return lipgloss.Color("1") // red
	case tool == "status_note":
		return lipgloss.Color("8") // dim
	default:
		return lipgloss.Color("7") // white
	}
}

// ActionRow is one structured row in the action feed.
type ActionRow struct {
	// Seq is a monotonic sequence number assigned at fold time (Handle).
	Seq uint64

	// AgentRole/AgentLabel identify which agent owns this row.
	AgentRole  string
	AgentLabel string

	// Tool is the tool name (e.g. "grep", "read_file").
	Tool string

	// Target is the normalized display target (file:line, file:line-end, pattern, symbol).
	Target string

	// File/Line/EndLine/Symbol/Pattern are the raw event fields, kept for
	// openSourceMsg emission on ENTER.
	File    string
	Line    int
	EndLine int
	Symbol  string
	Pattern string

	// InFlight is true while the Phase=done has not yet been received.
	InFlight bool

	// Count is the result count from Phase=done (hits/refs/lines).
	Count int

	// Err is a tool error from Phase=done (non-empty means error).
	Err string

	// At is the timestamp of the start event.
	At time.Time

	// IsObserver marks a row that came from RecentActions (plain Describe string, no raw fields).
	IsObserver bool
	// ObserverText is the raw Describe string for IsObserver rows.
	ObserverText string
}

// actionKey is the pairing key for start->done matching.
type actionKey struct {
	Role  string
	Label string
	Tool  string
}

// actionRing is a per-agent bounded ring of ActionRows plus a pairing index.
type actionRing struct {
	rows    [actionRingCap]ActionRow
	head    int                    // next write position (circular)
	count   int                    // how many valid rows (up to cap)
	pending map[uint64]int         // seq -> ring index for in-flight rows
	byKey   map[actionKey][]uint64 // key -> ordered list of in-flight seqs
}

func newActionRing() *actionRing {
	return &actionRing{
		pending: make(map[uint64]int),
		byKey:   make(map[actionKey][]uint64),
	}
}

// push adds a row and returns its ring index.
func (r *actionRing) push(row ActionRow) int {
	idx := r.head
	r.rows[idx] = row
	r.head = (r.head + 1) % actionRingCap
	if r.count < actionRingCap {
		r.count++
	}
	return idx
}

// Rows returns the rows in chronological order (oldest first).
func (r *actionRing) Rows() []ActionRow {
	if r.count == 0 {
		return nil
	}
	out := make([]ActionRow, r.count)
	start := (r.head - r.count + actionRingCap) % actionRingCap
	for i := 0; i < r.count; i++ {
		out[i] = r.rows[(start+i)%actionRingCap]
	}
	return out
}

// ApplyStart folds a Phase=start event into the ring.
func (r *actionRing) ApplyStart(ev progress.Event) {
	row := ActionRow{
		Seq:        nextSeq(),
		AgentRole:  ev.Role,
		AgentLabel: ev.Label,
		Tool:       ev.Tool,
		Target:     buildTarget(ev),
		File:       ev.File,
		Line:       ev.Line,
		EndLine:    ev.EndLine,
		Symbol:     ev.Symbol,
		Pattern:    ev.Pattern,
		InFlight:   true,
		At:         ev.Time,
	}
	idx := r.push(row)
	r.pending[row.Seq] = idx
	k := actionKey{Role: ev.Role, Label: ev.Label, Tool: ev.Tool}
	r.byKey[k] = append(r.byKey[k], row.Seq)
}

// ApplyDone folds a Phase=done event: matches the earliest unresolved start by
// (Role,Label,Tool) and updates it in place. If no match (orphan done), pushes
// a standalone resolved row.
func (r *actionRing) ApplyDone(ev progress.Event) {
	k := actionKey{Role: ev.Role, Label: ev.Label, Tool: ev.Tool}
	seqs := r.byKey[k]
	if len(seqs) > 0 {
		// match earliest unresolved
		matchSeq := seqs[0]
		r.byKey[k] = seqs[1:]
		if len(r.byKey[k]) == 0 {
			delete(r.byKey, k)
		}
		if ringIdx, ok := r.pending[matchSeq]; ok {
			// Patch in-place if the row is still in the ring (hasn't been evicted).
			// The ring may have wrapped around; we detect eviction by checking if
			// the slot still holds the expected seq.
			if r.rows[ringIdx].Seq == matchSeq {
				r.rows[ringIdx].InFlight = false
				r.rows[ringIdx].Count = ev.Count
				r.rows[ringIdx].Err = ev.Err
			}
			delete(r.pending, matchSeq)
		}
		return
	}
	// Orphan done: push a standalone resolved row.
	row := ActionRow{
		Seq:        nextSeq(),
		AgentRole:  ev.Role,
		AgentLabel: ev.Label,
		Tool:       ev.Tool,
		Target:     buildTarget(ev),
		File:       ev.File,
		Line:       ev.Line,
		EndLine:    ev.EndLine,
		Symbol:     ev.Symbol,
		Pattern:    ev.Pattern,
		InFlight:   false,
		Count:      ev.Count,
		Err:        ev.Err,
		At:         ev.Time,
	}
	r.push(row)
}

// buildTarget constructs the normalized display target from an event.
func buildTarget(ev progress.Event) string {
	switch {
	case ev.Tool == "grep":
		if ev.Pattern != "" {
			return fmt.Sprintf("%q", ev.Pattern)
		}
		return ""
	case ev.Tool == "status_note":
		return ev.Message
	case ev.Symbol != "" && ev.File != "":
		return fmt.Sprintf("%s in %s", ev.Symbol, ev.File)
	case ev.Symbol != "":
		return ev.Symbol
	case ev.File != "" && ev.Line > 0 && ev.EndLine > ev.Line:
		return fmt.Sprintf("%s:%d-%d", ev.File, ev.Line, ev.EndLine)
	case ev.File != "" && ev.Line > 0:
		return fmt.Sprintf("%s:%d", ev.File, ev.Line)
	case ev.File != "":
		return ev.File
	case ev.Pattern != "":
		return fmt.Sprintf("%q", ev.Pattern)
	default:
		return ""
	}
}

// ActionFeedState holds all rendering state for the action feed view mode.
// It lives on Model and is updated by handleToolCallEvent.
type ActionFeedState struct {
	// perAgent maps agentKey (role+":"+label) to its ring.
	perAgent map[string]*actionRing
	// aggregate is the interleaved ring of all agents.
	aggregate *actionRing

	// showAggregate: when true, show aggregate; when false, show per selected-agent.
	showAggregate bool

	// cursor is the row cursor within the visible feed list.
	cursor int

	// spinFrame is the current spinner animation frame index.
	spinFrame int
}

func newActionFeedState() ActionFeedState {
	return ActionFeedState{
		perAgent:  make(map[string]*actionRing),
		aggregate: newActionRing(),
	}
}

func agentFeedKey(role, label string) string { return role + ":" + label }

// ApplyToolCallEvent folds a KindToolCall event into both the per-agent ring
// and the aggregate ring.
func (s *ActionFeedState) ApplyToolCallEvent(ev progress.Event) {
	k := agentFeedKey(ev.Role, ev.Label)
	ring, ok := s.perAgent[k]
	if !ok {
		ring = newActionRing()
		s.perAgent[k] = ring
	}

	if ev.Phase == "start" {
		ring.ApplyStart(ev)
		s.aggregate.ApplyStart(ev)
	} else if ev.Phase == "done" {
		ring.ApplyDone(ev)
		s.aggregate.ApplyDone(ev)
	}
}

// RebuildAggregate rebuilds the aggregate ring from the union of all per-agent
// rings, sorted by Seq (monotonic insertion order). Call this after any
// structural change to perAgent (frame sync, prune). O(total rows) each call
// but bounded by actionRingCap per agent so stays fast.
func (s *ActionFeedState) RebuildAggregate() {
	// Collect all rows.
	var all []ActionRow
	for _, ring := range s.perAgent {
		all = append(all, ring.Rows()...)
	}
	// Sort by Seq for chronological interleave.
	sortActionRowsBySeq(all)
	// Rebuild aggregate ring (bounded to cap).
	s.aggregate = newActionRing()
	start := 0
	if len(all) > actionRingCap {
		start = len(all) - actionRingCap
	}
	for _, row := range all[start:] {
		s.aggregate.push(row)
	}
}

// PruneAgent removes the ring for the given agentFeedKey, then rebuilds the
// aggregate. Called on KindAgentFinished to prevent unbounded growth.
func (s *ActionFeedState) PruneAgent(feedKey string) {
	delete(s.perAgent, feedKey)
	s.RebuildAggregate()
}

// VisibleRows returns the rows for the current view mode, given the selected agent key.
func (s *ActionFeedState) VisibleRows(agentKey string) []ActionRow {
	if s.showAggregate {
		return s.aggregate.Rows()
	}
	if ring, ok := s.perAgent[agentKey]; ok {
		return ring.Rows()
	}
	return nil
}

// renderActionFeed renders the action feed into the detail pane.
// innerW/innerH are the content dimensions (inside border, inside header).
// selectedAgentKey is the key of the agent currently shown in paneDetail.
// recentActions is the Observer-mode ring (nil/empty in Owner mode).
// focused=true means the detail pane has keyboard focus.
func renderActionFeed(
	state ActionFeedState,
	innerW, innerH int,
	selectedAgentKey string,
	recentActions []string,
	focused bool,
) string {
	var b strings.Builder

	// Header line
	mode := "agent"
	if state.showAggregate {
		mode = "all"
	}
	b.WriteString(headerStyle.Render(fmt.Sprintf("Action Feed [%s]", mode)) + "\n")

	var rows []ActionRow
	if len(recentActions) > 0 {
		// Observer mode: render RecentActions as plain rows (newest-first -> reverse for display oldest-first).
		for i := len(recentActions) - 1; i >= 0; i-- {
			rows = append(rows, ActionRow{IsObserver: true, ObserverText: recentActions[i]})
		}
	} else {
		rows = state.VisibleRows(selectedAgentKey)
	}

	if len(rows) == 0 {
		b.WriteString(dimStyle.Render("(no tool calls yet)") + "\n")
		return b.String()
	}

	// Clamp cursor
	cursor := state.cursor
	if cursor >= len(rows) {
		cursor = len(rows) - 1
	}
	if cursor < 0 {
		cursor = 0
	}

	// Spinner frame
	spin := spinnerFrames[state.spinFrame%len(spinnerFrames)]

	// Determine visible window (scroll to cursor).
	maxLines := innerH - 2 // -1 for header, -1 for hints
	if maxLines < 1 {
		maxLines = 1
	}
	start := 0
	if cursor >= maxLines {
		start = cursor - maxLines + 1
	}
	end := start + maxLines
	if end > len(rows) {
		end = len(rows)
	}

	for i := start; i < end; i++ {
		row := rows[i]
		line := renderActionRow(row, spin, innerW)
		if focused && i == cursor {
			line = selectedStyle.Render(line)
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

var (
	feedErrStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	feedInFlightStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	feedOkStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
)

// renderActionRow formats one ActionRow into a single terminal line.
func renderActionRow(row ActionRow, spin string, width int) string {
	if row.IsObserver {
		return dimStyle.Render("  " + row.ObserverText)
	}

	glyph := toolGlyph(row.Tool)
	color := toolColor(row.Tool)
	glyphStr := lipgloss.NewStyle().Foreground(color).Render(glyph)

	// Target column (truncate if needed)
	target := row.Target
	maxTarget := width - 20
	if maxTarget < 8 {
		maxTarget = 8
	}
	if len(target) > maxTarget {
		target = target[:maxTarget-1] + "…"
	}

	// Outcome column
	var outcome string
	if row.InFlight {
		outcome = feedInFlightStyle.Render(spin)
	} else if row.Err != "" {
		outcome = feedErrStyle.Render("err: " + row.Err)
	} else if row.Count > 0 {
		outcome = feedOkStyle.Render(fmt.Sprintf("%d hits", row.Count))
	} else {
		outcome = feedOkStyle.Render("ok")
	}

	return fmt.Sprintf("%s %-10s %-*s %s", glyphStr, row.Tool, maxTarget, target, outcome)
}

// enterOnFeedRow returns a tea.Cmd when pressing ENTER on a feed row that has
// File or Pattern set. Returns nil if the row has no source location.
func enterOnFeedRow(row ActionRow) tea.Cmd {
	if row.IsObserver {
		return nil
	}
	if row.File == "" && row.Pattern == "" {
		return nil
	}
	msg := openSourceMsg{
		File:    row.File,
		Line:    row.Line,
		EndLine: row.EndLine,
		Pattern: row.Pattern,
	}
	return func() tea.Msg { return msg }
}

// advanceSpinner increments the spinner frame on each repaint.
func (s *ActionFeedState) advanceSpinner() {
	s.spinFrame = (s.spinFrame + 1) % len(spinnerFrames)
}
