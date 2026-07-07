package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/store"
	"github.com/dpoage/bugbot/internal/util"
)

// ── Styles ────────────────────────────────────────────────────────────────────

var (
	headerStyle    = lipgloss.NewStyle().Bold(true)
	dimStyle       = lipgloss.NewStyle().Faint(true)
	staleStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("3"))
	selectedStyle  = lipgloss.NewStyle().Bold(true).Reverse(true)
	sectionStyle   = lipgloss.NewStyle().Bold(true).Underline(true)
	footerStyle    = lipgloss.NewStyle().Faint(true)
	errorNoteStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))

	// paneBorderFocused is the lipgloss border applied to the active pane.
	paneBorderFocused = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("12"))

	// paneBorderNormal is the border for every non-focused pane.
	paneBorderNormal = lipgloss.NewStyle().
				Border(lipgloss.NormalBorder()).
				BorderForeground(lipgloss.Color("8"))
)

// ── Top-level View ────────────────────────────────────────────────────────────

// View implements tea.Model. It dispatches to the palette overlay or the
// three-pane compositor depending on state.
func (m Model) View() string {
	if m.quitting {
		return ""
	}

	if m.palette.open {
		return lipgloss.JoinVertical(lipgloss.Left, m.viewPalette(), "", m.viewFooter())
	}

	if m.cmdBar.open {
		return lipgloss.JoinVertical(lipgloss.Left, m.viewCmdBar(), "", m.viewFooter())
	}

	return lipgloss.JoinVertical(lipgloss.Left, m.viewPanes(), m.viewFooter())
}

// viewPanes composes the three simultaneous panes according to the current
// layout mode (horizontal ≥80 cols, else vertical stack). The focused pane
// receives a rounded border in the accent colour; others get a dim normal
// border.
func (m Model) viewPanes() string {
	horizontal, paneW, paneH := m.layoutDimensions()
	// inner content dimensions = pane outer minus 2-char border on each axis
	innerW := paneW - 2
	if innerW < 1 {
		innerW = 1
	}
	innerH := paneH - 2
	if innerH < 1 {
		innerH = 1
	}

	roster := m.renderRosterPane(innerW, innerH)
	detail := m.renderDetailPane(innerW, innerH)
	ctx := m.renderContextPane(innerW, innerH)

	rPane := m.applyPaneBorder(paneRoster, roster, paneW, paneH)
	dPane := m.applyPaneBorder(paneDetail, detail, paneW, paneH)
	cPane := m.applyPaneBorder(paneContext, ctx, paneW, paneH)

	if horizontal {
		return lipgloss.JoinHorizontal(lipgloss.Top, rPane, dPane, cPane)
	}

	// Narrow terminal: show focused pane first, then the others.
	ordered := [paneCount]string{rPane, dPane, cPane}
	focused := ordered[m.focus]
	var rest []string
	for i, p := range ordered {
		if pane(i) != m.focus {
			rest = append(rest, p)
		}
	}
	all := append([]string{focused}, rest...)
	return lipgloss.JoinVertical(lipgloss.Left, all...)
}

// applyPaneBorder wraps content with the appropriate lipgloss border, sized to
// paneW×paneH (outer dimensions including border characters).
func (m Model) applyPaneBorder(p pane, content string, paneW, paneH int) string {
	style := paneBorderNormal
	if m.focus == p {
		style = paneBorderFocused
	}
	w := paneW - 2
	if w < 1 {
		w = 1
	}
	h := paneH - 2
	if h < 1 {
		h = 1
	}
	return style.Width(w).Height(h).Render(content)
}

// ── Roster pane ───────────────────────────────────────────────────────────────

// renderRosterPane renders the filterable merged agent list.
func (m Model) renderRosterPane(innerW, innerH int) string {
	var b strings.Builder

	title := "Agents"
	if m.focus == paneRoster && (m.filtering || m.filter != "") {
		title = fmt.Sprintf("Agents  filter: %s", m.filter)
		if m.filtering {
			title += "█"
		}
	}
	b.WriteString(headerStyle.Render(title) + "\n")

	idx := m.visibleAgentIndices()
	if len(idx) == 0 {
		b.WriteString(dimStyle.Render("(no agents)") + "\n")
	} else {
		for row, i := range idx {
			a := m.frame.Agents[i]
			line := agentLine(a)
			if row == m.cursor && m.focus == paneRoster {
				line = selectedStyle.Render(line)
			} else if row == m.cursor {
				// dim selection indicator when roster is not the focused pane
				line = dimStyle.Render(line)
			}
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}

// ── Detail pane ───────────────────────────────────────────────────────────────

// renderDetailPane renders the selected agent's detail and transcript/action-feed viewport.
func (m Model) renderDetailPane(innerW, innerH int) string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("Agent Detail") + "\n")

	if m.detailKey == "" {
		b.WriteString(dimStyle.Render("select an agent (enter) to drill in") + "\n")
		return b.String()
	}
	if m.detailIdx < 0 || m.detailIdx >= len(m.frame.Agents) {
		b.WriteString(dimStyle.Render("(agent no longer available)") + "\n")
		return b.String()
	}

	a := m.frame.Agents[m.detailIdx]

	fmt.Fprintf(&b, "role:      %s\n", a.Role)
	if a.Lens != "" {
		fmt.Fprintf(&b, "lens:      %s\n", a.Lens)
	}
	if a.Strategy != "" {
		fmt.Fprintf(&b, "strategy:  %s\n", a.Strategy)
	}
	fmt.Fprintf(&b, "label:     %s\n", a.Label)
	if a.Live {
		b.WriteString("state:     live\n")
		if a.Activity != "" {
			fmt.Fprintf(&b, "activity:  %s\n", a.Activity)
		}
	} else {
		fmt.Fprintf(&b, "state:     %s\n", a.Status)
	}
	fmt.Fprintf(&b, "started:   %s\n", fmtTime(a.Started))
	if !a.FinishedAt.IsZero() {
		fmt.Fprintf(&b, "finished:  %s\n", fmtTime(a.FinishedAt))
	}

	if m.detailMode {
		// Action feed view: render the action feed for this agent.
		feed := renderActionFeed(m.actionFeed, innerW, innerH-8, agentFeedKey(a.Role, a.Label), recentActionsForView(a, m.frame), m.focus == paneDetail)
		b.WriteString(feed)
	} else {
		b.WriteString("\n" + sectionStyle.Render("Transcript") + "\n")
		if m.transcript == nil {
			b.WriteString(dimStyle.Render(m.transcriptNote) + "\n")
		} else {
			b.WriteString(m.transcriptView.View() + "\n")
		}
	}
	return b.String()
}

// recentActionsForView returns the RecentActions for a live Observer-mode agent
// when the frame has no Owner-mode ActionRows; otherwise nil.
func recentActionsForView(a AgentView, fr Frame) []string {
	if fr.ActionRows != nil {
		return nil
	}
	return a.RecentActions
}

// ── Context pane ─────────────────────────────────────────────────────────────

// renderContextPane renders the cockpit summary, findings, or leads view
// depending on m.contextMode.
func (m Model) renderContextPane(innerW, innerH int) string {
	switch m.contextMode {
	case contextModeFindings:
		return m.renderFindings()
	case contextModeLeads:
		return m.renderLeads()
	case contextModeSource:
		return m.renderSourcePane(innerW, innerH)
	case contextModeGrep:
		return m.renderGrepPane(innerW, innerH)
	default:
		return m.renderCockpitSummary()
	}
}

// renderCockpitSummary is the at-a-glance cockpit summary for the context pane.
func (m Model) renderCockpitSummary() string {
	var b strings.Builder
	fr := m.frame

	if !fr.HasSnapshot || fr.Stale {
		b.WriteString(headerStyle.Render("bugbot — idle") + "\n")
		if fr.HasSnapshot && fr.Stale {
			b.WriteString(staleStyle.Render("last-known state looks stale or crashed") + "\n")
		} else {
			b.WriteString(dimStyle.Render("no scan or daemon running against this config") + "\n")
		}
	} else {
		st := fr.Snapshot
		b.WriteString(headerStyle.Render("bugbot — active") + "\n")
		if st.ScanKind != "" {
			fmt.Fprintf(&b, "scan: kind=%s commit=%s elapsed=%s\n",
				st.ScanKind, util.ShortSHA(st.Commit), elapsedSince(st.StartedAt))
		}
		if st.Stage != "" {
			b.WriteString("stage: " + st.Stage + "\n")
		}
		fmt.Fprintf(&b, "run spend: in=%d out=%d total=%d tokens\n",
			st.SpendInput, st.SpendOutput, st.SpendInput+st.SpendOutput)
		b.WriteString("\n" + sectionStyle.Render("Agents") + "\n")
		if len(st.ActiveAgents) == 0 {
			b.WriteString(dimStyle.Render("(none active)") + "\n")
		}
		for _, a := range st.ActiveAgents {
			line := fmt.Sprintf("  %-12s %-24s running %s", a.Role, a.Label, elapsedSince(a.Started))
			if a.Activity != "" {
				line += "  [" + a.Activity + "]"
			}
			b.WriteString(line + "\n")
		}
	}

	b.WriteString("\n" + renderWorldState(fr.World))
	if line, ok := m.dispatchStatusLine(); ok {
		b.WriteString("\n" + line + "\n")
	}
	b.WriteString(dimStyle.Render("m: cycle views (findings/leads)") + "\n")
	return b.String()
}

// renderFindings renders the tallies breakdown and open-finding rows in the
// context pane. The open-finding list (ws.Findings) is cursor-navigable when
// contextModeFindings is active, mirroring renderLeads.
func (m Model) renderFindings() string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("Findings") + "\n")
	ws := m.frame.World
	if !ws.HasTallies {
		b.WriteString(dimStyle.Render("(no findings data)") + "\n")
	} else {
		t := ws.Tallies
		for tier := 0; tier <= 3; tier++ {
			if n, ok := t.OpenByTier[tier]; ok && n > 0 {
				fmt.Fprintf(&b, "T%d open:    %d\n", tier, n)
			}
		}
		fmt.Fprintf(&b, "fixed:      %d\n", t.Fixed)
		fmt.Fprintf(&b, "dismissed:  %d\n", t.Dismissed)
		if t.NeedsHuman > 0 {
			fmt.Fprintf(&b, "needs human: %d\n", t.NeedsHuman)
		}
		if len(ws.Published) > 0 {
			b.WriteString("\n" + sectionStyle.Render("Published") + "\n")
			for _, k := range sortedIssueStates(ws.Published) {
				fmt.Fprintf(&b, "%s: %d\n", k, ws.Published[k])
			}
		}
	}
	if len(ws.Findings) > 0 {
		b.WriteString("\n" + sectionStyle.Render("Open findings") + "\n")
		if ws.FindingsTotal > len(ws.Findings) {
			fmt.Fprintf(&b, "(%d total, showing %d)\n", ws.FindingsTotal, len(ws.Findings))
		}
		for row, f := range ws.Findings {
			line := fmt.Sprintf("%-40s  %s", f.Title, f.File)
			if row == m.cursor && m.focus == paneContext {
				line = selectedStyle.Render(line)
			}
			b.WriteString(line + "\n")
		}
	}
	b.WriteString(dimStyle.Render("m: cycle views") + "\n")
	return b.String()
}

// renderLeads renders the pending-leads blackboard in the context pane.
func (m Model) renderLeads() string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("Leads") + "\n")
	ws := m.frame.World
	if ws.PendingLeadsTotal == 0 {
		b.WriteString(dimStyle.Render("(blackboard empty)") + "\n")
	} else {
		fmt.Fprintf(&b, "%d pending lead(s)\n\n", ws.PendingLeadsTotal)
		for row, l := range ws.PendingLeads {
			line := formatLead(l)
			if row == m.cursor && m.focus == paneContext {
				line = selectedStyle.Render(line)
			}
			b.WriteString(line + "\n")
		}
	}
	b.WriteString(dimStyle.Render("m: cycle views") + "\n")
	return b.String()
}

// ── Shared renderers ─────────────────────────────────────────────────────────

// agentLine renders one row for the roster list.
func agentLine(a AgentView) string {
	state := a.Status
	if a.Live {
		state = "running " + elapsedSince(a.Started)
	}
	line := fmt.Sprintf("%-10s %-20s %-14s", a.Role, a.Label, state)
	if a.Live && a.Activity != "" {
		line += "  [" + a.Activity + "]"
	}
	return line
}

// elapsedSince renders how long ago t was, rounded to the second, or "-" for
// a zero time. Called from View (not stored on Model), so the repaint tick
// (repaintInterval) actually advances what's on screen between Frame updates
// rather than re-rendering an identical frame.
func elapsedSince(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return time.Since(t).Round(time.Second).String()
}

// renderTranscript formats a loaded transcript compactly for the viewport: one
// line per assistant turn (text + tool calls) and one per tool result.
// EventRequest entries are skipped — the full conversation state is
// reconstructible from the assistant/tool-result pairs and is too verbose for
// this pane.
func renderTranscript(t *agent.Transcript) string {
	if t == nil || len(t.Events) == 0 {
		return "(empty transcript)"
	}
	var b strings.Builder
	for _, ev := range t.Events {
		switch ev.Kind {
		case agent.EventAssistant:
			text := util.CollapseWhitespace(ev.Text)
			fmt.Fprintf(&b, "[step %d] assistant: %s\n", ev.Step, util.TruncateRunes(text, 200))
			for _, tc := range ev.ToolCalls {
				fmt.Fprintf(&b, "           tool_call: %s\n", tc.Name)
			}
		case agent.EventToolResult:
			marker := "ok"
			if ev.IsError {
				marker = "error"
			}
			result := util.CollapseWhitespace(ev.Result)
			fmt.Fprintf(&b, "[step %d] tool_result(%s, %s): %s\n", ev.Step, ev.ToolName, marker, util.TruncateRunes(result, 200))
		}
	}
	return b.String()
}

// renderWorldState renders the accumulated world-state block for the cockpit pane.
func renderWorldState(ws WorldState) string {
	var b strings.Builder
	b.WriteString(sectionStyle.Render("World state") + "\n")

	if ws.HasTallies {
		fmt.Fprintf(&b, "findings: %s\n", findingsLine(ws.Tallies))
		if ws.Tallies.NeedsHuman > 0 {
			fmt.Fprintf(&b, "needs human: %d finding(s) flagged for human review\n", ws.Tallies.NeedsHuman)
		}
	}
	if ws.PendingLeadsTotal == 0 {
		b.WriteString("blackboard: empty\n")
	} else {
		fmt.Fprintf(&b, "blackboard: %d pending lead(s)\n", ws.PendingLeadsTotal)
	}
	if ws.HasDaySpend {
		raw := ws.DaySpend.InputTokens + ws.DaySpend.OutputTokens
		line := fmt.Sprintf("in=%d out=%d total=%d tokens", ws.DaySpend.InputTokens, ws.DaySpend.OutputTokens, raw)
		if ws.DayBudgetLimit > 0 {
			chargeable := ws.DaySpend.Chargeable(ws.CacheReadWeight)
			pct := float64(chargeable) * 100 / float64(ws.DayBudgetLimit)
			line += fmt.Sprintf(" (%.1f%% of day budget)", pct)
		}
		b.WriteString("spend today: " + line + "\n")
	}
	if ws.HasLastRun {
		when := "running"
		if !ws.LastRun.FinishedAt.IsZero() {
			when = "finished"
		}
		fmt.Fprintf(&b, "last run: %s commit=%s %s\n", ws.LastRun.Kind, util.ShortSHA(ws.LastRun.CommitSHA), when)
	}
	return b.String()
}

// findingsLine renders "open: T0=1 T1=2 T2=3 | fixed=4 dismissed=1".
func findingsLine(t domain.FindingTallies) string {
	var parts []string
	for tier := 0; tier <= 3; tier++ {
		if n := t.OpenByTier[tier]; n > 0 {
			parts = append(parts, fmt.Sprintf("T%d=%d", tier, n))
		}
	}
	open := "none"
	if len(parts) > 0 {
		open = strings.Join(parts, " ")
	}
	return fmt.Sprintf("open: %s | fixed=%d dismissed=%d", open, t.Fixed, t.Dismissed)
}

func formatLead(l store.Lead) string {
	return fmt.Sprintf("-> %s: %s:%d — %s", l.TargetLens, l.File, l.Line, util.TruncateRunes(util.CollapseWhitespace(l.Note), 70))
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format(time.RFC3339)
}

// viewFooter renders the keymap hint line at the bottom of the screen.
func (m Model) viewFooter() string {
	follow := ""
	if m.followActive {
		follow = " · [F:follow]"
	}
	if m.filtering {
		return footerStyle.Render("filter: type to narrow · enter accept · esc clear · ctrl+c quit")
	}
	if m.running {
		return footerStyle.Render("ctrl+x/esc cancel run · d dispatch · tab/1/2/3 focus · q quit")
	}
	switch m.focus {
	case paneRoster:
		return footerStyle.Render("j/k move · h/l pane · gg/G top/bot · ctrl+d/u half · enter drill in · / filter · ctrl+p jump · F follow · d dispatch · tab/1/2/3 · q quit" + follow)
	case paneDetail:
		if m.detailMode {
			return footerStyle.Render("j/k scroll feed · enter open · a transcript · A toggle-all · gg/G top/bot · ctrl+d/u half · h/l pane · ctrl+p jump · F follow · d dispatch · tab/1/2/3 · q quit" + follow)
		}
		return footerStyle.Render("j/k scroll transcript · a action-feed · gg/G top/bot · ctrl+d/u half · h/l pane · ctrl+p jump · F follow · d dispatch · tab/1/2/3 · q quit" + follow)
	case paneContext:
		return footerStyle.Render("m cycle modes · j/k scroll/move · gg/G top/bot · ctrl+d/u half · h/l pane · ctrl+p jump · F follow · d dispatch · tab/1/2/3 · q quit" + follow)
	default:
		return footerStyle.Render("h/l pane · tab/1/2/3 focus · ctrl+p jump · F follow · d dispatch · q quit" + follow)
	}
}

// renderSourcePane renders the syntax-highlighted source view in the context pane.
func (m Model) renderSourcePane(innerW, innerH int) string {
	if m.sourceNote != "" && len(m.sourceLines) == 0 {
		var b strings.Builder
		b.WriteString(headerStyle.Render("Source") + "\n")
		b.WriteString(dimStyle.Render(m.sourceNote) + "\n")
		b.WriteString(dimStyle.Render("esc: back") + "\n")
		return b.String()
	}
	content := renderSourceView(m.sourceLines, m.sourceFile, m.sourceLine, m.sourceEndLine, m.sourceOffset, innerW, innerH)
	return content + "\n" + dimStyle.Render("j/k scroll · esc back")
}

// renderGrepPane renders the grep hit list in the context pane.
func (m Model) renderGrepPane(innerW, innerH int) string {
	if m.grepNote != "" && len(m.grepHits) == 0 {
		var b strings.Builder
		b.WriteString(headerStyle.Render("Grep: "+m.grepPattern) + "\n")
		b.WriteString(dimStyle.Render(m.grepNote) + "\n")
		b.WriteString(dimStyle.Render("esc: back") + "\n")
		return b.String()
	}
	content := renderGrepView(m.grepHits, m.grepCursor, m.grepOffset, innerW, innerH)
	return content + "\n" + dimStyle.Render("j/k move · enter open · esc back")
}
