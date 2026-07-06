package tui

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/dpoage/bugbot/internal/agent"
)

// screen identifies one full-screen view. Cockpit/Agents/Findings/Leads are
// the top-level screens tab cycles through; AgentDetail is only reachable by
// drilling into an agent from Agents (or Cockpit) and returns to whichever
// screen pushed it.
type screen int

const (
	screenCockpit screen = iota
	screenAgents
	screenAgentDetail
	screenFindings
	screenLeads
)

// topScreens is the tab-cycle order.
var topScreens = []screen{screenCockpit, screenAgents, screenFindings, screenLeads}

// repaintInterval drives a UI-only re-render (elapsed timers, spend ticker)
// independent of the Feed's cadence, so the Cockpit advances even between
// Frame updates.
const repaintInterval = 250 * time.Millisecond

// repaintMsg is sent on repaintInterval purely to trigger a re-render.
type repaintMsg time.Time

func repaintTick() tea.Cmd {
	return tea.Tick(repaintInterval, func(t time.Time) tea.Msg { return repaintMsg(t) })
}

// Model is the bubbletea reducer driving every screen. It knows nothing about
// where Frame values come from — that is entirely Feed's concern — so a
// future Owner-mode LiveFeed plugs in by construction with zero changes here.
type Model struct {
	feed Feed

	frame     Frame
	haveFrame bool

	screen     screen
	prevScreen screen // screen to return to from screenAgentDetail on esc

	cursor    int // index into the CURRENT screen's visible list
	filter    string
	filtering bool

	detailIdx        int // absolute index into frame.Agents for the drilled-in agent
	transcript       *agent.Transcript
	transcriptNote   string // set when there is no transcript to show, or a load error
	transcriptView   viewport.Model
	transcriptLoaded bool // whether the above two fields reflect detailIdx

	width, height int
	quitting      bool
}

// NewModel builds the initial Model for feed. now is the model's notion of
// "current time" for elapsed/staleness rendering; pass a fixed function in
// tests for deterministic output.
func NewModel(feed Feed) Model {
	return Model{
		feed:           feed,
		transcriptView: viewport.New(0, 0),
		width:          80,
		height:         24,
	}
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
		vw, vh := m.transcriptSize()
		m.transcriptView.Width = vw
		m.transcriptView.Height = vh
		return m, nil

	case FrameMsg:
		m.frame = Frame(msg)
		m.haveFrame = true
		m.clampCursor()
		return m, m.feed.Next()

	case repaintMsg:
		return m, repaintTick()

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.filtering {
		switch msg.Type {
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
		m.quitting = true
		return m, tea.Quit

	case "tab":
		m.screen = nextTopScreen(m.screen)
		m.cursor = 0
		return m, nil

	case "j", "down":
		m.moveCursor(1)
		return m, nil

	case "k", "up":
		m.moveCursor(-1)
		return m, nil

	case "/":
		if m.screen == screenAgents {
			m.filtering = true
		}
		return m, nil

	case "enter":
		if m.screen == screenAgents {
			idx := m.visibleAgentIndices()
			if m.cursor >= 0 && m.cursor < len(idx) {
				m.detailIdx = idx[m.cursor]
				m.prevScreen = m.screen
				m.screen = screenAgentDetail
				m.loadTranscript()
			}
		}
		return m, nil

	case "esc":
		if m.screen == screenAgentDetail {
			m.screen = m.prevScreen
		} else if m.filter != "" {
			m.filter = ""
			m.cursor = 0
		}
		return m, nil
	}
	return m, nil
}

// nextTopScreen cycles through topScreens; a detail screen falls back to
// Cockpit before advancing so tab always lands on a top-level screen.
func nextTopScreen(cur screen) screen {
	for i, s := range topScreens {
		if s == cur {
			return topScreens[(i+1)%len(topScreens)]
		}
	}
	return topScreens[0]
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

// listLen returns the number of navigable rows on the current screen.
func (m Model) listLen() int {
	switch m.screen {
	case screenAgents:
		return len(m.visibleAgentIndices())
	case screenLeads:
		return len(m.frame.World.PendingLeads)
	default:
		return 0
	}
}

// loadTranscript populates m.transcript/transcriptNote/transcriptView for the
// agent at m.detailIdx. It only ever reads a local file — no network, no LLM.
func (m *Model) loadTranscript() {
	m.transcriptLoaded = true
	m.transcript = nil
	m.transcriptNote = ""
	m.transcriptView.SetContent("")

	if m.detailIdx < 0 || m.detailIdx >= len(m.frame.Agents) {
		m.transcriptNote = "no transcript"
		return
	}
	path := m.frame.Agents[m.detailIdx].TranscriptPath
	if path == "" {
		m.transcriptNote = "no transcript"
		return
	}
	f, err := os.Open(path)
	if err != nil {
		m.transcriptNote = fmt.Sprintf("transcript unavailable: %v", err)
		return
	}
	defer f.Close()

	tr, err := agent.LoadJSONL(f)
	if err != nil {
		m.transcriptNote = fmt.Sprintf("transcript unavailable: %v", err)
		return
	}
	m.transcript = tr
	m.transcriptView.SetContent(renderTranscript(tr))
}

// transcriptSize returns the viewport dimensions for the AgentDetail screen's
// transcript pane, leaving room for the detail header above it.
func (m Model) transcriptSize() (int, int) {
	w := m.width - 4
	if w < 10 {
		w = 10
	}
	h := m.height - 12
	if h < 3 {
		h = 3
	}
	return w, h
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
