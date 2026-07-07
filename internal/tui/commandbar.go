package tui

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/dpoage/bugbot/internal/store"
)

// ── Fuzzy matching ────────────────────────────────────────────────────────────

// matchScore is the priority tier returned by fuzzyScore.
// Lower value = better match (so sorting ascending puts best matches first).
type matchScore int

const (
	matchExactSubstring matchScore = iota // target contains query as a substring
	matchWordPrefix                       // query is a prefix of some word in target
	matchSubsequence                      // query chars appear as a scattered subsequence
	matchNone                             // no match
)

// fuzzyScore scores query against target (both case-folded internally).
// It returns (matchNone, false) when query does not match at all.
func fuzzyScore(target, query string) (matchScore, bool) {
	if query == "" {
		return matchSubsequence, true // empty query matches everything
	}
	tl := strings.ToLower(target)
	ql := strings.ToLower(query)

	// Tier 1: exact substring.
	if strings.Contains(tl, ql) {
		return matchExactSubstring, true
	}

	// Tier 2: prefix of any whitespace/punctuation-delimited word.
	if hasWordPrefix(tl, ql) {
		return matchWordPrefix, true
	}

	// Tier 3: scattered subsequence.
	if isSubsequence(tl, ql) {
		return matchSubsequence, true
	}

	return matchNone, false
}

// hasWordPrefix reports whether ql is a prefix of any word in tl.
// Word boundaries are positions immediately after whitespace or punctuation.
func hasWordPrefix(tl, ql string) bool {
	runes := []rune(tl)
	qrunes := []rune(ql)
	for i, r := range runes {
		// A word starts at position 0 or after a non-letter/non-digit.
		wordStart := i == 0 || !unicode.IsLetter(runes[i-1]) && !unicode.IsDigit(runes[i-1])
		if !wordStart {
			continue
		}
		_ = r
		// Check if the query is a prefix starting at i.
		if i+len(qrunes) > len(runes) {
			continue
		}
		match := true
		for j, q := range qrunes {
			if runes[i+j] != q {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// isSubsequence reports whether every rune of sub appears in s in order.
func isSubsequence(s, sub string) bool {
	si := 0
	sr := []rune(s)
	for _, r := range sub {
		found := false
		for si < len(sr) {
			if sr[si] == r {
				si++
				found = true
				break
			}
			si++
		}
		if !found {
			return false
		}
	}
	return true
}

// ── Command bar candidates ────────────────────────────────────────────────────

// cmdKind identifies the type of item a cmdCandidate represents.
type cmdKind int

const (
	cmdKindAgent cmdKind = iota
	cmdKindLead
)

// cmdCandidate is one selectable item in the command bar.
type cmdCandidate struct {
	kind    cmdKind
	label   string // display text shown in the bar
	display string // searchable text used for fuzzy matching

	// For cmdKindAgent: index into frame.Agents.
	agentIdx int
	agentKey string // stable key for re-resolution

	// For cmdKindLead: index into frame.World.PendingLeads.
	leadIdx int
}

// buildCandidates constructs the full candidate universe from frame.
// Called on every keystroke; allocation is proportional to frame size
// (small — typically <100 agents + leads in practice).
func buildCandidates(frame Frame) []cmdCandidate {
	caps := len(frame.Agents) + len(frame.World.PendingLeads)
	candidates := make([]cmdCandidate, 0, caps)

	for i, a := range frame.Agents {
		label := fmt.Sprintf("agent  %-10s  %s", a.Role, a.Label)
		search := a.Role + " " + a.Label
		candidates = append(candidates, cmdCandidate{
			kind:     cmdKindAgent,
			label:    label,
			display:  search,
			agentIdx: i,
			agentKey: agentKey(a),
		})
	}

	for i, l := range frame.World.PendingLeads {
		label := fmt.Sprintf("lead   %-10s  %s", l.TargetLens, l.File)
		search := l.TargetLens + " " + l.File + " " + l.Note
		candidates = append(candidates, cmdCandidate{
			kind:    cmdKindLead,
			label:   label,
			display: search,
			leadIdx: i,
		})
	}

	return candidates
}

// filterCandidates returns only those candidates matching query, ordered by
// fuzzy rank (best first). Stable within the same rank tier (input order
// preserved from buildCandidates, which is frame order).
func filterCandidates(all []cmdCandidate, query string) []cmdCandidate {
	type ranked struct {
		c    cmdCandidate
		tier matchScore
		orig int
	}
	ranked_list := make([]ranked, 0, len(all))
	for i, c := range all {
		tier, ok := fuzzyScore(c.display, query)
		if !ok {
			continue
		}
		ranked_list = append(ranked_list, ranked{c: c, tier: tier, orig: i})
	}
	// Stable sort by tier ascending (lower = better).
	// Use insertion sort (list is tiny).
	for i := 1; i < len(ranked_list); i++ {
		for j := i; j > 0 && ranked_list[j].tier < ranked_list[j-1].tier; j-- {
			ranked_list[j], ranked_list[j-1] = ranked_list[j-1], ranked_list[j]
		}
	}
	result := make([]cmdCandidate, len(ranked_list))
	for i, r := range ranked_list {
		result[i] = r.c
	}
	return result
}

// ── Command bar state ─────────────────────────────────────────────────────────

// cmdBarState holds the command bar overlay's own UI state, separate from
// Model's focus/cursor fields. Mirrors paletteState's pattern: an open bool,
// a textinput for the query, a list of filtered candidates, and a selection
// cursor.
type cmdBarState struct {
	open      bool
	input     textinput.Model
	results   []cmdCandidate
	cursor    int            // index into results
	allFramed []cmdCandidate // full universe from the last frame (rebuilt each keystroke)
}

// newCmdBarState builds the initial (closed) command bar state.
func newCmdBarState() cmdBarState {
	ti := textinput.New()
	ti.Placeholder = "jump to agent or lead…"
	ti.CharLimit = 128
	ti.Width = 60
	return cmdBarState{input: ti}
}

// openCmdBar opens the command bar and rebuilds the candidate universe from frame.
func (s *cmdBarState) openCmdBar(frame Frame) {
	s.open = true
	s.allFramed = buildCandidates(frame)
	s.results = filterCandidates(s.allFramed, "")
	s.cursor = 0
	s.input.SetValue("")
	s.input.Focus()
}

// closeCmdBar resets the command bar to closed state.
func (s *cmdBarState) closeCmdBar() {
	s.open = false
	s.input.SetValue("")
	s.input.Blur()
	s.results = nil
	s.allFramed = nil
	s.cursor = 0
}

// refilter updates results from the current input value.
func (s *cmdBarState) refilter() {
	s.results = filterCandidates(s.allFramed, s.input.Value())
	if s.cursor >= len(s.results) {
		s.cursor = 0
	}
}

// selectedCandidate returns the currently highlighted candidate, or (zero, false).
func (s *cmdBarState) selectedCandidate() (cmdCandidate, bool) {
	if len(s.results) == 0 || s.cursor < 0 || s.cursor >= len(s.results) {
		return cmdCandidate{}, false
	}
	return s.results[s.cursor], true
}

// ── Key handling ──────────────────────────────────────────────────────────────

// handleCmdBarKey handles a keypress while the command bar overlay is open.
// Returns (Model, tea.Cmd, consumed bool). When consumed is true the caller
// must not forward the key to the normal switch.
func (m Model) handleCmdBarKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyCtrlC:
		m.quitting = true
		return m, tea.Quit

	case tea.KeyEsc:
		m.cmdBar.closeCmdBar()
		return m, nil

	case tea.KeyEnter:
		var navCmd tea.Cmd
		if c, ok := m.cmdBar.selectedCandidate(); ok {
			m, navCmd = m.cmdBarNavigateWithCmd(c)
		}
		m.cmdBar.closeCmdBar()
		return m, navCmd

	case tea.KeyUp, tea.KeyCtrlK:
		if m.cmdBar.cursor > 0 {
			m.cmdBar.cursor--
		}
		return m, nil

	case tea.KeyDown, tea.KeyCtrlJ:
		if m.cmdBar.cursor < len(m.cmdBar.results)-1 {
			m.cmdBar.cursor++
		}
		return m, nil
	}

	switch msg.String() {
	case "j":
		if m.cmdBar.cursor < len(m.cmdBar.results)-1 {
			m.cmdBar.cursor++
		}
		return m, nil

	case "k":
		if m.cmdBar.cursor > 0 {
			m.cmdBar.cursor--
		}
		return m, nil
	}

	// All other keys: route into the textinput; refilter on change.
	before := m.cmdBar.input.Value()
	var cmd tea.Cmd
	m.cmdBar.input, cmd = m.cmdBar.input.Update(msg)
	if m.cmdBar.input.Value() != before {
		m.cmdBar.refilter()
	}
	_ = cmd // see palette.go: skip blink cmd to keep tests synchronous
	return m, nil
}

// cmdBarNavigateWithCmd returns (Model, tea.Cmd) so that agent drill-in can
// return a loadTranscriptCmd exactly as handleKey's enter case does.
func (m Model) cmdBarNavigateWithCmd(c cmdCandidate) (Model, tea.Cmd) {
	switch c.kind {
	case cmdKindAgent:
		idx, ok := findAgentByKey(m.frame.Agents, c.agentKey)
		if !ok {
			idx = c.agentIdx
		}
		if idx < 0 || idx >= len(m.frame.Agents) {
			return m, nil
		}
		a := m.frame.Agents[idx]
		m.detailIdx = idx
		m.detailKey = agentKey(a)
		m.transcript = nil
		m.transcriptNote = "loading transcript..."
		m.transcriptPath = a.TranscriptPath
		m.transcriptLoaded = false
		m.transcriptView.SetContent("")
		m.focus = paneDetail
		// Turn off follow — user made an explicit choice.
		m.followActive = false
		visible := m.visibleAgentIndices()
		for row, vi := range visible {
			if vi == idx {
				m.cursor = row
				break
			}
		}
		return m, loadTranscriptCmd(m.detailKey, a.TranscriptPath)

	case cmdKindLead:
		m.focus = paneContext
		m.contextMode = contextModeLeads
		target := c.leadIdx
		if target >= len(m.frame.World.PendingLeads) {
			target = len(m.frame.World.PendingLeads) - 1
		}
		if target < 0 {
			target = 0
		}
		m.cursor = target
		return m, nil
	}
	return m, nil
}

// ── Follow-active-agent ───────────────────────────────────────────────────────

// applyFollowActive applies follow-active-agent logic to the model when a new
// frame arrives and m.followActive is true. It selects the most-recently-
// active live agent (max ActivityAt among live agents) in the roster and
// drills into the detail pane. Returns (Model, tea.Cmd): the cmd may be a
// loadTranscriptCmd when the selected agent changed.
func (m Model) applyFollowActive(frame Frame) (Model, tea.Cmd) {
	if !m.followActive {
		return m, nil
	}
	idx := mostRecentlyActiveAgent(frame.Agents)
	if idx < 0 {
		return m, nil // no live agents; hold current selection
	}
	a := frame.Agents[idx]
	key := agentKey(a)

	// Already showing this agent — nothing to do.
	if key == m.detailKey {
		return m, nil
	}

	m.detailIdx = idx
	m.detailKey = key
	m.transcript = nil
	m.transcriptNote = "loading transcript..."
	m.transcriptLoaded = false
	m.transcriptView.SetContent("")
	m.focus = paneDetail

	// Advance roster cursor to the agent row.
	visible := m.visibleAgentIndices()
	for row, vi := range visible {
		if vi == idx {
			m.cursor = row
			break
		}
	}

	if a.TranscriptPath != m.transcriptPath {
		m.transcriptPath = a.TranscriptPath
		return m, loadTranscriptCmd(key, a.TranscriptPath)
	}
	return m, nil
}

// mostRecentlyActiveAgent returns the index of the live agent with the latest
// ActivityAt, or -1 when no live agents exist in views.
func mostRecentlyActiveAgent(views []AgentView) int {
	best := -1
	for i, a := range views {
		if !a.Live {
			continue
		}
		if best < 0 || a.ActivityAt.After(views[best].ActivityAt) {
			best = i
		}
	}
	return best
}

// ── Render ────────────────────────────────────────────────────────────────────

// viewCmdBar renders the command bar overlay as a string, mirroring
// viewPalette's pattern: header, textinput, filtered result list, footer hint.
func (m Model) viewCmdBar() string {
	var b strings.Builder

	// Header.
	header := "Jump to…"
	if m.followActive {
		header += "  [follow: ON]"
	}
	b.WriteString(headerStyle.Render(header) + "\n")
	b.WriteString("\n")

	// Textinput.
	b.WriteString(m.cmdBar.input.View() + "\n")
	b.WriteString("\n")

	// Results list.
	if len(m.cmdBar.results) == 0 {
		b.WriteString(dimStyle.Render("(no matches)") + "\n")
	} else {
		for i, c := range m.cmdBar.results {
			line := c.label
			if i == m.cmdBar.cursor {
				line = selectedStyle.Render("> " + line)
			} else {
				line = "  " + line
			}
			b.WriteString(line + "\n")
		}
	}

	b.WriteString("\n")
	b.WriteString(footerStyle.Render("type to filter · j/k or ↑/↓ move · enter jump · esc cancel"))
	return b.String()
}

// viewCmdBarLeadLabel is exported for tests — renders a lead's display label
// in the same format buildCandidates uses, for assertion.
func cmdBarLeadLabel(l store.Lead) string {
	return fmt.Sprintf("lead   %-10s  %s", l.TargetLens, l.File)
}
