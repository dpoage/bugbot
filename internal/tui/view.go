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

var (
	headerStyle    = lipgloss.NewStyle().Bold(true)
	dimStyle       = lipgloss.NewStyle().Faint(true)
	staleStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("3"))
	selectedStyle  = lipgloss.NewStyle().Bold(true).Reverse(true)
	sectionStyle   = lipgloss.NewStyle().Bold(true).Underline(true)
	footerStyle    = lipgloss.NewStyle().Faint(true)
	errorNoteStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
)

// View implements tea.Model.
func (m Model) View() string {
	if m.quitting {
		return ""
	}

	if m.palette.open {
		return lipgloss.JoinVertical(lipgloss.Left, m.viewPalette(), "", m.viewFooter())
	}

	var body string
	switch m.screen {
	case screenAgents:
		body = m.viewAgents()
	case screenAgentDetail:
		body = m.viewAgentDetail()
	case screenFindings:
		body = m.viewFindings()
	case screenLeads:
		body = m.viewLeads()
	default:
		body = m.viewCockpit()
	}

	return lipgloss.JoinVertical(lipgloss.Left, body, "", m.viewFooter())
}

// viewCockpit renders the primary at-a-glance screen: header, live agents,
// and the accumulated world-state block. With no live snapshot (or a stale
// one) it renders the static idle view — world-state only.
func (m Model) viewCockpit() string {
	var b strings.Builder
	fr := m.frame

	if !fr.HasSnapshot || fr.Stale {
		b.WriteString(headerStyle.Render("bugbot — idle"))
		b.WriteString("\n")
		if fr.HasSnapshot && fr.Stale {
			b.WriteString(staleStyle.Render("last-known state looks stale or crashed") + "\n")
		} else {
			b.WriteString(dimStyle.Render("no scan or daemon running against this config") + "\n")
		}
	} else {
		st := fr.Snapshot
		b.WriteString(headerStyle.Render("bugbot — active") + "\n")
		if st.ScanKind != "" {
			b.WriteString(fmt.Sprintf("scan: kind=%s commit=%s elapsed=%s\n",
				st.ScanKind, util.ShortSHA(st.Commit), elapsedSince(st.StartedAt)))
		}
		if st.Stage != "" {
			b.WriteString("stage: " + st.Stage + "\n")
		}
		b.WriteString(fmt.Sprintf("run spend: in=%d out=%d total=%d tokens\n",
			st.SpendInput, st.SpendOutput, st.SpendInput+st.SpendOutput))
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
	return b.String()
}

// viewAgents renders the filterable merged agent list.
func (m Model) viewAgents() string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("Agents") + "\n")
	if m.filtering || m.filter != "" {
		b.WriteString(fmt.Sprintf("filter: %s\n", m.filter))
	}

	idx := m.visibleAgentIndices()
	if len(idx) == 0 {
		b.WriteString(dimStyle.Render("(no agents)") + "\n")
		return b.String()
	}
	for row, i := range idx {
		a := m.frame.Agents[i]
		line := agentLine(a)
		if row == m.cursor {
			line = selectedStyle.Render(line)
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

// agentLine renders one row for the Agents list.
func agentLine(a AgentView) string {
	state := a.Status
	if a.Live {
		state = "running " + elapsedSince(a.Started)
	}
	line := fmt.Sprintf("%-10s %-24s %-16s", a.Role, a.Label, state)
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

// viewAgentDetail renders the drill-down for the selected agent: its
// activity/timeline plus agent_units detail fields, and the transcript
// viewer (or a "no transcript" note).
func (m Model) viewAgentDetail() string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("Agent detail") + "\n")

	if m.detailIdx < 0 || m.detailIdx >= len(m.frame.Agents) {
		b.WriteString(dimStyle.Render("(agent no longer available)") + "\n")
		return b.String()
	}
	a := m.frame.Agents[m.detailIdx]

	b.WriteString(fmt.Sprintf("role:      %s\n", a.Role))
	if a.Lens != "" {
		b.WriteString(fmt.Sprintf("lens:      %s\n", a.Lens))
	}
	if a.Strategy != "" {
		b.WriteString(fmt.Sprintf("strategy:  %s\n", a.Strategy))
	}
	b.WriteString(fmt.Sprintf("label:     %s\n", a.Label))
	if a.Live {
		b.WriteString("state:     live\n")
		if a.Activity != "" {
			b.WriteString(fmt.Sprintf("activity:  %s (at %s)\n", a.Activity, fmtTime(a.ActivityAt)))
		}
	} else {
		b.WriteString(fmt.Sprintf("state:     %s\n", a.Status))
	}
	b.WriteString(fmt.Sprintf("started:   %s\n", fmtTime(a.Started)))
	if !a.FinishedAt.IsZero() {
		b.WriteString(fmt.Sprintf("finished:  %s (took %s)\n", fmtTime(a.FinishedAt), a.FinishedAt.Sub(a.Started).Round(time.Second)))
	}
	b.WriteString(fmt.Sprintf("tokens:    in=%d out=%d cached=%d\n", a.InputTokens, a.OutputTokens, a.CacheReadTokens))
	if a.Candidates != 0 || a.LeadsPosted != 0 {
		b.WriteString(fmt.Sprintf("candidates: %d  leads posted: %d\n", a.Candidates, a.LeadsPosted))
	}
	if len(a.Files) > 0 {
		b.WriteString("files:     " + strings.Join(a.Files, ", ") + "\n")
	}
	if a.Detail != "" {
		b.WriteString("detail:    " + a.Detail + "\n")
	}

	b.WriteString("\n" + sectionStyle.Render("Transcript") + "\n")
	if m.transcript == nil {
		b.WriteString(dimStyle.Render(m.transcriptNote) + "\n")
	} else {
		b.WriteString(m.transcriptView.View() + "\n")
	}
	return b.String()
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
			b.WriteString(fmt.Sprintf("[step %d] assistant: %s\n", ev.Step, util.TruncateRunes(text, 200)))
			for _, tc := range ev.ToolCalls {
				b.WriteString(fmt.Sprintf("           tool_call: %s\n", tc.Name))
			}
		case agent.EventToolResult:
			marker := "ok"
			if ev.IsError {
				marker = "error"
			}
			result := util.CollapseWhitespace(ev.Result)
			b.WriteString(fmt.Sprintf("[step %d] tool_result(%s, %s): %s\n", ev.Step, ev.ToolName, marker, util.TruncateRunes(result, 200)))
		}
	}
	return b.String()
}

// viewFindings renders the tallies breakdown.
func (m Model) viewFindings() string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("Findings") + "\n")
	ws := m.frame.World
	if !ws.HasTallies {
		b.WriteString(dimStyle.Render("(no findings data)") + "\n")
		return b.String()
	}
	t := ws.Tallies
	for tier := 0; tier <= 3; tier++ {
		if n, ok := t.OpenByTier[tier]; ok && n > 0 {
			b.WriteString(fmt.Sprintf("T%d open:   %d\n", tier, n))
		}
	}
	b.WriteString(fmt.Sprintf("fixed:      %d\n", t.Fixed))
	b.WriteString(fmt.Sprintf("dismissed:  %d\n", t.Dismissed))
	if t.NeedsHuman > 0 {
		b.WriteString(fmt.Sprintf("needs human: %d\n", t.NeedsHuman))
	}
	if len(ws.Published) > 0 {
		b.WriteString("\n" + sectionStyle.Render("Published") + "\n")
		for _, k := range sortedIssueStates(ws.Published) {
			b.WriteString(fmt.Sprintf("%s: %d\n", k, ws.Published[k]))
		}
	}
	return b.String()
}

// viewLeads renders the pending-leads blackboard.
func (m Model) viewLeads() string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("Leads") + "\n")
	ws := m.frame.World
	if ws.PendingLeadsTotal == 0 {
		b.WriteString(dimStyle.Render("(blackboard empty)") + "\n")
		return b.String()
	}
	b.WriteString(fmt.Sprintf("%d pending lead(s)\n\n", ws.PendingLeadsTotal))
	for row, l := range ws.PendingLeads {
		line := formatLead(l)
		if row == m.cursor {
			line = selectedStyle.Render(line)
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

func formatLead(l store.Lead) string {
	return fmt.Sprintf("-> %s: %s:%d — %s", l.TargetLens, l.File, l.Line, util.TruncateRunes(util.CollapseWhitespace(l.Note), 70))
}

// renderWorldState renders the accumulated world-state block shared by the
// Cockpit screen.
func renderWorldState(ws WorldState) string {
	var b strings.Builder
	b.WriteString(sectionStyle.Render("World state") + "\n")

	if ws.HasTallies {
		b.WriteString(fmt.Sprintf("findings: %s\n", findingsLine(ws.Tallies)))
		if ws.Tallies.NeedsHuman > 0 {
			b.WriteString(fmt.Sprintf("needs human: %d finding(s) flagged for human review\n", ws.Tallies.NeedsHuman))
		}
	}
	if ws.PendingLeadsTotal == 0 {
		b.WriteString("blackboard: empty\n")
	} else {
		b.WriteString(fmt.Sprintf("blackboard: %d pending lead(s)\n", ws.PendingLeadsTotal))
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
		b.WriteString(fmt.Sprintf("last run: %s commit=%s %s\n", ws.LastRun.Kind, util.ShortSHA(ws.LastRun.CommitSHA), when))
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

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format(time.RFC3339)
}

// viewFooter renders the keymap hint line.
func (m Model) viewFooter() string {
	if m.filtering {
		return footerStyle.Render("filter: type to narrow · enter accept · esc clear")
	}
	if m.running {
		return footerStyle.Render("ctrl+x/esc cancel run · d dispatch · tab cycle · q quit")
	}
	hint := "j/k move · d dispatch · tab cycle · q quit"
	switch m.screen {
	case screenAgents:
		hint = "j/k move · enter drill in · / filter · d dispatch · tab cycle · q quit"
	case screenAgentDetail:
		hint = "esc back · tab cycle · q quit"
	}
	return footerStyle.Render(hint)
}
