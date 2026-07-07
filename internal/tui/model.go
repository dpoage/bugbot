package tui

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/dpoage/bugbot/internal/agent"
)

// pane identifies one of the three simultaneous panes in the compositor.
//
// paneRoster  — left  — filterable merged agent list
// paneDetail  — center — selected agent's status + transcript
// paneContext — right  — cockpit summary / world-state with optional sub-modes
type pane int

const (
	paneRoster  pane = iota // agent list with '/' filter
	paneDetail              // selected agent detail + transcript
	paneContext             // cockpit summary / world-state
	paneCount   = 3
)

// contextMode is the sub-mode of paneContext; cycles via a key within the pane.
type contextMode int

const (
	contextModeSummary  contextMode = iota // cockpit at-a-glance summary
	contextModeFindings                    // findings tallies
	contextModeLeads                       // pending-leads blackboard
)

// minWidthForHorizontal is the column threshold below which panes stack
// vertically (focused-pane-first) instead of side by side.
const minWidthForHorizontal = 80

// repaintInterval drives a UI-only re-render (elapsed timers, spend ticker)
// independent of the Feed's cadence, so the Cockpit advances even between
// Frame updates.
const repaintInterval = 250 * time.Millisecond

// repaintMsg is sent on repaintInterval purely to trigger a re-render.
type repaintMsg time.Time

func repaintTick() tea.Cmd {
	return tea.Tick(repaintInterval, func(t time.Time) tea.Msg { return repaintMsg(t) })
}

// Model is the bubbletea reducer driving the three-pane cockpit. It knows
// nothing about where Frame values come from — that is entirely Feed's concern
// — so a LiveFeed (Owner mode) or SnapshotFeed (Observer mode) plugs in by
// construction with zero changes here.
type Model struct {
	feed Feed

	frame     Frame
	haveFrame bool

	// focus is the pane currently receiving keyboard input.
	focus pane

	// contextMode is the sub-mode of paneContext.
	contextMode contextMode

	// cursor is the row index within the focused pane's navigable list
	// (roster list for paneRoster, leads list for contextModeLeads).
	cursor    int
	filter    string
	filtering bool

	// detailIdx/detailKey track the agent shown in paneDetail.
	// detailKey is the stable identity; detailIdx is re-resolved each frame.
	detailIdx int
	detailKey string

	transcript     *agent.Transcript
	transcriptNote string
	// transcriptPath is the TranscriptPath the current transcript/note was
	// loaded for; a FrameMsg that resolves detailKey to a different path
	// triggers a reload.
	transcriptPath   string
	transcriptView   viewport.Model
	transcriptLoaded bool

	// rosterView and contextView are independently scrollable viewport models
	// for the roster and context panes respectively.
	rosterView  viewport.Model
	contextView viewport.Model

	width, height int
	quitting      bool

	// disp is the dispatch palette's handle into the in-process engine; nil
	// means dispatch is disabled (Observer mode, or a never-run repo — see
	// selectFeed). Model never calls it directly outside confirmPaletteRow.
	disp dispatcher
	// ctx is the program's own context (see run.go's runProgram), retained
	// solely so a dispatched verb's tea.Cmd can derive a cancelable child
	// context via context.WithCancel(m.ctx).
	ctx context.Context

	palette paletteState

	// running/runVerb/runStarted/runCancel/runOut describe the ONE active
	// dispatch, if any — the palette refuses to start a second one while
	// running is true.
	running    bool
	runVerb    string
	runStarted time.Time
	runCancel  context.CancelFunc
	runOut     *capBuffer

	// lastVerb/lastErr/lastResult describe the most recently COMPLETED dispatch.
	lastVerb   string
	lastErr    error
	lastResult string
	lastOut    string
}

// NewModel builds the initial Model for feed. disp is the dispatch
// palette's handle into the in-process engine (nil disables dispatch — see
// selectFeed); ctx is the program's own context, retained only so a
// dispatched verb can derive a cancelable child context. The transcript
// viewport starts sized for the default 80x24 terminal so it renders content
// even before the first real tea.WindowSizeMsg arrives (matters for tests
// that never send one; a real tea.Program always sends one immediately
// after Init).
func NewModel(ctx context.Context, feed Feed, disp dispatcher) Model {
	m := Model{
		feed:      feed,
		disp:      disp,
		ctx:       ctx,
		palette:   newPaletteState(),
		width:     80,
		height:    24,
		detailIdx: -1,
	}
	vw, vh := m.paneDetailSize()
	m.transcriptView = viewport.New(vw, vh)
	rw, rh := m.paneRosterSize()
	m.rosterView = viewport.New(rw, rh)
	cw, ch := m.paneContextSize()
	m.contextView = viewport.New(cw, ch)
	return m
}

// Init implements tea.Model.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.feed.Next(), repaintTick())
}

// Update implements tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.resizePanes()
		return m, nil

	case FrameMsg:
		m.frame = Frame(msg)
		m.haveFrame = true
		if m.detailKey != "" {
			if idx, ok := findAgentByKey(m.frame.Agents, m.detailKey); ok {
				m.detailIdx = idx
				if path := m.frame.Agents[idx].TranscriptPath; path != m.transcriptPath {
					m.transcriptPath = path
					m.transcript = nil
					m.transcriptLoaded = false
					m.transcriptNote = "loading transcript..."
					m.transcriptView.SetContent("")
					m.clampCursor()
					return m, tea.Batch(m.feed.Next(), loadTranscriptCmd(m.detailKey, path))
				}
			} else {
				m.detailIdx = -1
			}
		}
		m.clampCursor()
		return m, m.feed.Next()

	case transcriptLoadedMsg:
		// Discard results for an agent the user has since navigated away
		// from (or a superseded reload for the same agent).
		if msg.key != m.detailKey {
			return m, nil
		}
		m.transcriptLoaded = true
		m.transcript = msg.transcript
		m.transcriptNote = msg.note
		if msg.transcript != nil {
			m.transcriptView.SetContent(renderTranscript(msg.transcript))
		} else {
			m.transcriptView.SetContent("")
		}
		return m, nil

	case repaintMsg:
		return m, repaintTick()

	case dispatchDoneMsg:
		if m.runCancel != nil {
			m.runCancel()
		}
		m.running = false
		m.runCancel = nil
		m.runVerb = ""
		if m.runOut != nil {
			m.lastOut = m.runOut.Tail()
		}
		m.runOut = nil
		m.lastVerb = msg.verb
		m.lastErr = msg.err
		m.lastResult = msg.summary
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

// handleKey handles keyboard input for the multi-pane compositor.
func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Cancel key stops the active dispatch at highest priority.
	if m.running {
		switch msg.String() {
		case "ctrl+x", "esc":
			return m.cancelRun(), nil
		}
	}

	if m.palette.open {
		return m.handlePaletteKey(msg)
	}

	if m.filtering {
		switch msg.Type {
		case tea.KeyCtrlC:
			if m.running {
				m = m.cancelRun()
			}
			m.quitting = true
			return m, tea.Quit
		case tea.KeyEsc:
			m.filtering = false
			m.filter = ""
			m.cursor = 0
		case tea.KeyEnter:
			m.filtering = false
		case tea.KeyBackspace:
			if m.filter != "" {
				r := []rune(m.filter)
				m.filter = string(r[:len(r)-1])
			}
		default:
			if msg.Type == tea.KeyRunes {
				m.filter += string(msg.Runes)
			}
		}
		m.clampCursor()
		return m, nil
	}

	switch msg.String() {
	case "ctrl+c", "q":
		if m.running {
			m = m.cancelRun()
		}
		m.quitting = true
		return m, tea.Quit

	case "tab":
		m.focus = (m.focus + 1) % paneCount
		m.cursor = 0
		return m, nil

	case "1":
		m.focus = paneRoster
		m.cursor = 0
		return m, nil

	case "2":
		m.focus = paneDetail
		return m, nil

	case "3":
		m.focus = paneContext
		m.cursor = 0
		return m, nil

	case "d":
		// Opens from any pane. Opens even when m.disp == nil: the palette
		// renders every verb disabled with the reason instead of hiding
		// itself, so Observer-mode operators can see WHY dispatch is
		// unavailable.
		m.palette.open = true
		m.palette.editing = false
		return m, nil

	case "j", "down":
		m.scrollDown()
		return m, nil

	case "k", "up":
		m.scrollUp()
		return m, nil

	case "f", "pgdown":
		m.pageDown()
		return m, nil

	case "b", "pgup":
		m.pageUp()
		return m, nil

	case "/":
		// Filter only available when roster pane is focused.
		if m.focus == paneRoster {
			m.filtering = true
		}
		return m, nil

	case "enter":
		// Drill into an agent from the roster pane: sets detailKey and
		// switches focus to the detail pane to show it.
		if m.focus == paneRoster {
			idx := m.visibleAgentIndices()
			if m.cursor >= 0 && m.cursor < len(idx) {
				a := m.frame.Agents[idx[m.cursor]]
				m.detailIdx = idx[m.cursor]
				m.detailKey = agentKey(a)
				m.transcript = nil
				m.transcriptNote = "loading transcript..."
				m.transcriptPath = a.TranscriptPath
				m.transcriptLoaded = false
				m.transcriptView.SetContent("")
				m.focus = paneDetail
				return m, loadTranscriptCmd(m.detailKey, a.TranscriptPath)
			}
		}
		return m, nil

	case "m":
		// Cycle the context pane's sub-mode (summary → findings → leads → summary).
		m.contextMode = (m.contextMode + 1) % 3
		m.cursor = 0
		return m, nil

	case "esc":
		if m.filter != "" {
			m.filter = ""
			m.cursor = 0
		}
		return m, nil
	}
	return m, nil
}

// scrollDown advances the focused pane's cursor or viewport down by one row.
func (m *Model) scrollDown() {
	switch m.focus {
	case paneRoster:
		m.moveCursor(1)
	case paneDetail:
		m.transcriptView.LineDown(1)
	case paneContext:
		if m.contextMode == contextModeLeads {
			m.moveCursor(1)
		} else {
			m.contextView.LineDown(1)
		}
	}
}

// scrollUp moves the focused pane's cursor or viewport up by one row.
func (m *Model) scrollUp() {
	switch m.focus {
	case paneRoster:
		m.moveCursor(-1)
	case paneDetail:
		m.transcriptView.LineUp(1)
	case paneContext:
		if m.contextMode == contextModeLeads {
			m.moveCursor(-1)
		} else {
			m.contextView.LineUp(1)
		}
	}
}

// pageDown pages the focused pane down.
func (m *Model) pageDown() {
	switch m.focus {
	case paneDetail:
		m.transcriptView.ViewDown()
	case paneContext:
		m.contextView.ViewDown()
	}
}

// pageUp pages the focused pane up.
func (m *Model) pageUp() {
	switch m.focus {
	case paneDetail:
		m.transcriptView.ViewUp()
	case paneContext:
		m.contextView.ViewUp()
	}
}

// visibleAgentIndices returns indices into m.frame.Agents matching the active
// filter (case-insensitive substring over role/label/lens/activity/detail),
// or every index when no filter is set.
func (m Model) visibleAgentIndices() []int {
	agents := m.frame.Agents
	if m.filter == "" {
		idx := make([]int, len(agents))
		for i := range agents {
			idx[i] = i
		}
		return idx
	}
	needle := strings.ToLower(m.filter)
	var idx []int
	for i, a := range agents {
		hay := strings.ToLower(a.Role + " " + a.Label + " " + a.Lens + " " + a.Activity + " " + a.Detail)
		if strings.Contains(hay, needle) {
			idx = append(idx, i)
		}
	}
	return idx
}

func (m *Model) moveCursor(delta int) {
	n := m.listLen()
	if n == 0 {
		m.cursor = 0
		return
	}
	m.cursor += delta
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor > n-1 {
		m.cursor = n - 1
	}
}

func (m *Model) clampCursor() {
	n := m.listLen()
	if n == 0 {
		m.cursor = 0
		return
	}
	if m.cursor > n-1 {
		m.cursor = n - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

// listLen returns the number of navigable rows in the focused pane's list.
func (m Model) listLen() int {
	switch m.focus {
	case paneRoster:
		return len(m.visibleAgentIndices())
	case paneContext:
		if m.contextMode == contextModeLeads {
			return len(m.frame.World.PendingLeads)
		}
		return 0
	default:
		return 0
	}
}

// resizePanes recalculates all viewport dimensions when the terminal size changes.
func (m *Model) resizePanes() {
	rw, rh := m.paneRosterSize()
	m.rosterView.Width = rw
	m.rosterView.Height = rh

	dw, dh := m.paneDetailSize()
	m.transcriptView.Width = dw
	m.transcriptView.Height = dh

	cw, ch := m.paneContextSize()
	m.contextView.Width = cw
	m.contextView.Height = ch
}

// paneRosterSize returns the (width, height) for the roster viewport.
func (m Model) paneRosterSize() (int, int) {
	_, w, h := m.layoutDimensions()
	// leave 2 lines for header + filter row inside the pane
	if h < 3 {
		h = 3
	}
	if w < 10 {
		w = 10
	}
	return w - 2, h - 4
}

// paneDetailSize returns the (width, height) for the transcript viewport.
func (m Model) paneDetailSize() (int, int) {
	_, w, h := m.layoutDimensions()
	// leave lines for header + detail fields + section heading
	if h < 3 {
		h = 3
	}
	if w < 10 {
		w = 10
	}
	return w - 2, h - 14
}

// paneContextSize returns the (width, height) for the context viewport.
func (m Model) paneContextSize() (int, int) {
	_, w, h := m.layoutDimensions()
	if h < 3 {
		h = 3
	}
	if w < 10 {
		w = 10
	}
	return w - 2, h - 4
}

// layoutDimensions returns (horizontal bool, paneWidth int, paneHeight int).
// When horizontal is true the three panes sit side-by-side; otherwise they
// stack vertically (narrow terminal degradation).
// paneWidth and paneHeight describe the inner content area of each pane.
func (m Model) layoutDimensions() (bool, int, int) {
	// subtract 2 from height for the footer line + blank separator
	availH := m.height - 2
	if availH < 3 {
		availH = 3
	}

	if m.width >= minWidthForHorizontal {
		// horizontal: three columns split roughly 25/45/30
		paneW := m.width / 3
		if paneW < 20 {
			paneW = 20
		}
		return true, paneW, availH
	}

	// vertical stack: each pane gets a third of the height
	paneH := availH / 3
	if paneH < 3 {
		paneH = 3
	}
	return false, m.width, paneH
}

// transcriptLoadedMsg is the tea.Msg loadTranscriptCmd resolves to. key ties
// the result back to the detailKey that requested it, so a stale/superseded
// load is discarded on arrival rather than clobbering the displayed agent's
// transcript.
type transcriptLoadedMsg struct {
	key        string
	transcript *agent.Transcript
	note       string // set when there is no transcript to show, or a load error
}

// loadTranscriptCmd reads path (when non-empty) off the bubbletea Update
// thread, mirroring SnapshotFeed.buildFrame's own off-thread pattern: this is
// the ONLY other place internal/tui touches the filesystem, and
// agent.LoadJSONL reads the whole file into memory, which would otherwise
// freeze keyboard input on drill-in for a multi-MB transcript. It only ever
// reads a local file — no network, no LLM.
func loadTranscriptCmd(key, path string) tea.Cmd {
	return func() tea.Msg {
		if path == "" {
			return transcriptLoadedMsg{key: key, note: "no transcript"}
		}
		f, err := os.Open(path)
		if err != nil {
			return transcriptLoadedMsg{key: key, note: fmt.Sprintf("transcript unavailable: %v", err)}
		}
		defer func() { _ = f.Close() }()

		tr, err := agent.LoadJSONL(f)
		if err != nil {
			return transcriptLoadedMsg{key: key, note: fmt.Sprintf("transcript unavailable: %v", err)}
		}
		return transcriptLoadedMsg{key: key, transcript: tr}
	}
}

// sortedIssueStates returns the keys of a published-issue map in a stable
// order, for deterministic rendering.
func sortedIssueStates[K ~string, V any](m map[K]V) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}
