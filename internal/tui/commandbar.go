package tui

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/store"
)

// ── Fuzzy matching ────────────────────────────────────────────────────────────

// matchScore is the priority tier returned by fuzzyScore.
// Lower value = better match (so sorting ascending puts best matches first).
type matchScore int

const (
	matchExactSubstring matchScore = iota // target contains query as a substring
	matchWordPrefix                       // query is a prefix of some word in target (but not a substring of the whole)
	matchSubsequence                      // query chars appear as a scattered subsequence
	matchNone                             // no match
)

// fuzzyScore scores query against target (both case-folded internally).
// Tier ordering: word-prefix is checked BEFORE Contains so that a query
// that is both a word-prefix AND an inner substring of the full string ranks
// as word-prefix (the more semantically precise match) rather than exact
// substring. This makes the word-prefix tier reachable and ensures prefix
// outranks mid-string matches.
// It returns (matchNone, false) when query does not match at all.
func fuzzyScore(target, query string) (matchScore, bool) {
	if query == "" {
		return matchSubsequence, true // empty query matches everything
	}
	tl := strings.ToLower(target)
	ql := strings.ToLower(query)

	// Tier 1 (best): query is a prefix of some whitespace/punctuation word.
	// Checked before Contains so that "nil" matching "nil-safety" (word-prefix)
	// is returned as matchWordPrefix, not matchExactSubstring.
	if hasWordPrefix(tl, ql) {
		return matchWordPrefix, true
	}

	// Tier 2: exact substring — query appears verbatim somewhere in target
	// (but NOT as a clean word prefix, handled above).
	if strings.Contains(tl, ql) {
		return matchExactSubstring, true
	}

	// Tier 3: scattered subsequence.
	if isSubsequence(tl, ql) {
		return matchSubsequence, true
	}

	return matchNone, false
}

// hasWordPrefix reports whether ql is a prefix of any word in tl.
// Word boundaries are positions immediately after a non-letter/non-digit rune,
// or position 0.
func hasWordPrefix(tl, ql string) bool {
	runes := []rune(tl)
	qrunes := []rune(ql)
	if len(qrunes) == 0 {
		return true
	}
	for i := range runes {
		// A word starts at position 0 or immediately after a non-alnum rune.
		wordStart := i == 0 || (!unicode.IsLetter(runes[i-1]) && !unicode.IsDigit(runes[i-1]))
		if !wordStart {
			continue
		}
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
	cmdKindAgent   cmdKind = iota
	cmdKindFinding         // open finding: enter -> paneContext+contextModeFindings+cursor
	cmdKindLead            // pending lead: enter -> paneContext+contextModeLeads+cursor
	cmdKindFile            // file from AgentView.Files: enter -> owning agent's detail pane
)

// cmdCandidate is one selectable item in the command bar.
type cmdCandidate struct {
	kind    cmdKind
	label   string // display text shown in the bar
	display string // searchable text used for fuzzy matching

	// For cmdKindAgent and cmdKindFile: stable key for re-resolution.
	agentIdx int
	agentKey string

	// For cmdKindFinding: stable identity for re-resolution (domain.Finding.ID).
	findingID  string
	findingIdx int // fallback position

	// For cmdKindLead: stable identity for re-resolution (store.Lead.ID).
	leadID  string
	leadIdx int // fallback position
}

// buildCandidates constructs the full candidate universe from frame.
// Called on every keystroke; allocation is proportional to frame size.
// Candidate order: agents, findings, leads, files (within each group,
// frame order is preserved).
func buildCandidates(frame Frame) []cmdCandidate {
	caps := len(frame.Agents) + len(frame.World.Findings) + len(frame.World.PendingLeads)
	// Count files across all agents.
	for _, a := range frame.Agents {
		caps += len(a.Files)
	}
	candidates := make([]cmdCandidate, 0, caps)

	for i, a := range frame.Agents {
		label := fmt.Sprintf("agent   %-10s  %s", a.Role, a.Label)
		search := a.Role + " " + a.Label
		candidates = append(candidates, cmdCandidate{
			kind:     cmdKindAgent,
			label:    label,
			display:  search,
			agentIdx: i,
			agentKey: agentKey(a),
		})
	}

	for i, f := range frame.World.Findings {
		label := fmt.Sprintf("finding %-40s  %s", f.Title, f.File)
		search := f.Title + " " + f.File
		candidates = append(candidates, cmdCandidate{
			kind:       cmdKindFinding,
			label:      label,
			display:    search,
			findingID:  f.ID,
			findingIdx: i,
		})
	}

	for i, l := range frame.World.PendingLeads {
		label := fmt.Sprintf("lead    %-10s  %s", l.TargetLens, l.File)
		search := l.TargetLens + " " + l.File + " " + l.Note
		candidates = append(candidates, cmdCandidate{
			kind:    cmdKindLead,
			label:   label,
			display: search,
			leadID:  l.ID,
			leadIdx: i,
		})
	}

	// File candidates: one entry per file per agent (N1).
	for i, a := range frame.Agents {
		for _, file := range a.Files {
			label := fmt.Sprintf("file    %-30s  [%s %s]", file, a.Role, a.Label)
			search := file + " " + a.Role + " " + a.Label
			candidates = append(candidates, cmdCandidate{
				kind:     cmdKindFile,
				label:    label,
				display:  search,
				agentIdx: i,
				agentKey: agentKey(a),
			})
		}
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
	}
	rankedList := make([]ranked, 0, len(all))
	for _, c := range all {
		tier, ok := fuzzyScore(c.display, query)
		if !ok {
			continue
		}
		rankedList = append(rankedList, ranked{c: c, tier: tier})
	}
	// Stable sort by tier ascending (lower = better).
	// Insertion sort is fine — candidate lists are small in practice.
	for i := 1; i < len(rankedList); i++ {
		for j := i; j > 0 && rankedList[j].tier < rankedList[j-1].tier; j-- {
			rankedList[j], rankedList[j-1] = rankedList[j-1], rankedList[j]
		}
	}
	result := make([]cmdCandidate, len(rankedList))
	for i, r := range rankedList {
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
	ti.Placeholder = "jump to agent, finding, lead, or file…"
	ti.CharLimit = 128
	ti.Width = 60
	return cmdBarState{input: ti}
}

// openCmdBar opens the command bar and rebuilds the candidate universe from frame.
// If the bar is already open, this is a no-op (preserves the current query
// and selection — ctrl+p while open doesn't wipe the user's work).
func (s *cmdBarState) openCmdBar(frame Frame) {
	if s.open {
		return // N3: ctrl+p while already open is a no-op
	}
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

// cmdBarNavigateWithCmd applies the navigation effect of a confirmed candidate
// selection. Returns (Model, tea.Cmd): the cmd may be a loadTranscriptCmd
// for agent/file drill-in.
//
// Re-resolution at nav time (N6): leads and findings are re-resolved by stable
// ID against the current frame (which may have been refreshed while the bar
// was open) rather than the snapshot index captured at open time.
func (m Model) cmdBarNavigateWithCmd(c cmdCandidate) (Model, tea.Cmd) {
	switch c.kind {
	case cmdKindAgent, cmdKindFile:
		// Both agent and file candidates drill into the owning agent's detail pane.
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

	case cmdKindFinding:
		// Re-resolve by stable ID (N6).
		target := resolveFindingIdx(m.frame.World.Findings, c.findingID, c.findingIdx)
		m.focus = paneContext
		m.contextMode = contextModeFindings
		m.cursor = target
		return m, nil

	case cmdKindLead:
		// Re-resolve by stable ID (N6).
		target := resolveLeadIdx(m.frame.World.PendingLeads, c.leadID, c.leadIdx)
		m.focus = paneContext
		m.contextMode = contextModeLeads
		m.cursor = target
		return m, nil
	}
	return m, nil
}

// resolveFindingIdx returns the current index of the finding with id in findings,
// falling back to fallback when not found (frame may have reordered).
func resolveFindingIdx(findings []domain.Finding, id string, fallback int) int {
	for i, f := range findings {
		if f.ID == id {
			return i
		}
	}
	if fallback >= len(findings) {
		if len(findings) == 0 {
			return 0
		}
		return len(findings) - 1
	}
	if fallback < 0 {
		return 0
	}
	return fallback
}

// resolveLeadIdx returns the current index of the lead with id in leads,
// falling back to fallback when not found.
func resolveLeadIdx(leads []store.Lead, id string, fallback int) int {
	for i, l := range leads {
		if l.ID == id {
			return i
		}
	}
	if fallback >= len(leads) {
		if len(leads) == 0 {
			return 0
		}
		return len(leads) - 1
	}
	if fallback < 0 {
		return 0
	}
	return fallback
}

// ── Follow-active-agent ───────────────────────────────────────────────────────

// applyFollowActive applies follow-active-agent logic to the model when a new
// frame arrives and m.followActive is true. It selects the most-recently-
// active live agent (max ActivityAt among live agents) in the roster and
// drills into the detail pane. Returns (Model, tea.Cmd): always returns a
// loadTranscriptCmd when the selected agent changes (even for agents with an
// empty TranscriptPath — loadTranscriptCmd maps "" to "no transcript" gracefully,
// preventing the detail pane from being stuck at "loading transcript...").
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
	m.transcriptPath = a.TranscriptPath

	// Advance roster cursor to the agent row.
	visible := m.visibleAgentIndices()
	for row, vi := range visible {
		if vi == idx {
			m.cursor = row
			break
		}
	}

	// Always fire loadTranscriptCmd on agent change (B2 fix): even when
	// TranscriptPath is empty, loadTranscriptCmd resolves cleanly to
	// "no transcript" rather than leaving the pane stuck at "loading…".
	return m, loadTranscriptCmd(key, a.TranscriptPath)
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
