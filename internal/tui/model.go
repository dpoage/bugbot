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

// screen identifies one full-screen view. Cockpit/Agents/Findings/Leads are
// the top-level screens tab cycles through; AgentDetail is only reachable by
// drilling into an agent from Agents and returns to whichever screen pushed
// it.
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

	// detailIdx is the CURRENT position of the drilled-in agent in
	// frame.Agents, re-resolved from detailKey on every FrameMsg (mergeAgents
	// rebuilds the slice from scratch each frame, so a raw index would go
	// stale). -1 means detailKey's agent is not present in the current frame.
	detailIdx int
	// detailKey is the stable identity (see agentKey) of the agent behind
	// screenAgentDetail; empty when no agent has been drilled into yet.
	detailKey string

	transcript     *agent.Transcript
	transcriptNote string // set when there is no transcript, it is loading, or a load error
	// transcriptPath is the TranscriptPath the current transcript/note was
	// loaded for; a FrameMsg that resolves detailKey to a different
	// TranscriptPath triggers a reload.
	transcriptPath   string
	transcriptView   viewport.Model
	transcriptLoaded bool // whether load has settled (success, empty, or error) for transcriptPath

	width, height int
	quitting      bool

	// disp is the dispatch palette's handle into the in-process engine; nil
	// means dispatch is disabled (Observer mode, or a never-run repo — see
	// selectFeed). Model never calls it directly outside confirmPaletteRow.
	disp dispatcher
	// ctx is the program's own context (see run.go's runProgram), retained
	// solely so a dispatched verb's tea.Cmd can derive a cancelable child
	// context via context.WithCancel(m.ctx) — cancelling one run must not
	// quit the TUI, so it cannot simply reuse tea.WithContext's lifecycle.
	// Nothing else on Model reads it.
	ctx context.Context

	palette paletteState

	// running/runVerb/runStarted/runCancel/runOut describe the ONE active
	// dispatch, if any — the palette refuses to start a second one while
	// running is true. runCancel is non-nil only while running is true.
	running    bool
	runVerb    string
	runStarted time.Time
	runCancel  context.CancelFunc
	runOut     *capBuffer

	// lastVerb/lastErr/lastResult describe the most recently COMPLETED
	// dispatch (success, error, or cancelled), rendered on the Cockpit
	// status line and in the palette overlay until superseded by the next
	// run. lastOut is the tail of that run's own Out/ErrOut text (see
	// capBuffer) — surfaced in the palette's "last dispatch" detail
	// alongside lastResult's typed *Result summary, since a verb can also
	// print incidental status text a typed summary does not capture (e.g.
	// Verify's "no pending candidates" line, a sandbox-degraded warning).
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
		feed:    feed,
		disp:    disp,
		ctx:     ctx,
		palette: newPaletteState(),
		width:   80,
		height:  24,
	}
	vw, vh := m.transcriptSize()
	m.transcriptView = viewport.New(vw, vh)
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
		vw, vh := m.transcriptSize()
		m.transcriptView.Width = vw
		m.transcriptView.Height = vh
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
		// from (or a superseded reload for the same agent) — msg.key ties
		// the result back to the request that produced it.
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
		// runCancel is idempotent (context.CancelFunc): calling it here even
		// though dispatchCmd's own defer already released runCtx on every
		// path keeps the invariant simple ("Model never forgets to call a
		// cancel it created") regardless of which path got here first.
		if m.runCancel != nil {
			m.runCancel()
		}
		m.running = false
		m.runCancel = nil
		m.runVerb = ""
		// Capture the verb's own captured Out/ErrOut text before dropping
		// the buffer — this is the only place runOut's contents are ever
		// read (dispatchCmd only writes it), so a run whose funnel prints
		// something beyond its typed *Result (e.g. Verify's "no pending
		// candidates" line, or a sandbox-degraded warning) is not silently
		// discarded.
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

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// A cancel key stops the active dispatch without quitting the TUI or
	// closing whatever screen/palette is open; it takes priority over every
	// other key while a run is in flight, matching the palette's one-at-a-
	// time gating — while running is true there is always exactly one run a
	// cancel key could mean.
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

	// screenAgentDetail forwards navigation to the transcript viewport
	// instead of the list-cursor keys (there is no list on this screen).
	if m.screen == screenAgentDetail {
		switch msg.String() {
		case "ctrl+c", "q":
			if m.running {
				m = m.cancelRun()
			}
			m.quitting = true
			return m, tea.Quit
		case "tab":
			m.screen = nextTopScreen(m.screen)
			m.cursor = 0
			return m, nil
		case "esc":
			m.screen = m.prevScreen
			return m, nil
		default:
			var cmd tea.Cmd
			m.transcriptView, cmd = m.transcriptView.Update(msg)
			return m, cmd
		}
	}

	switch msg.String() {
	case "ctrl+c", "q":
		if m.running {
			m = m.cancelRun()
		}
		m.quitting = true
		return m, tea.Quit

	case "tab":
		m.screen = nextTopScreen(m.screen)
		m.cursor = 0
		return m, nil
	case "d":
		// Opens from any top-level screen (this switch is unreachable from
		// screenAgentDetail — see the branch above — and from filtering,
		// handled earlier). Opens even when m.disp == nil: the palette
		// renders every verb disabled with the reason instead of hiding
		// itself, so Observer-mode operators can see WHY dispatch is
		// unavailable rather than wondering if the key did nothing.
		m.palette.open = true
		m.palette.editing = false
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
				a := m.frame.Agents[idx[m.cursor]]
				m.detailIdx = idx[m.cursor]
				m.detailKey = agentKey(a)
				m.prevScreen = m.screen
				m.screen = screenAgentDetail
				m.transcript = nil
				m.transcriptNote = "loading transcript..."
				m.transcriptPath = a.TranscriptPath
				m.transcriptLoaded = false
				m.transcriptView.SetContent("")
				return m, loadTranscriptCmd(m.detailKey, a.TranscriptPath)
			}
		}
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

// transcriptLoadedMsg is the tea.Msg loadTranscriptCmd resolves to. key ties
// the result back to the detailKey that requested it, so a stale/superseded
// load (user drilled into a different agent, or the frame's TranscriptPath
// changed again, before this one finished) is discarded on arrival rather
// than clobbering the currently-displayed agent's transcript.
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
		defer f.Close()

		tr, err := agent.LoadJSONL(f)
		if err != nil {
			return transcriptLoadedMsg{key: key, note: fmt.Sprintf("transcript unavailable: %v", err)}
		}
		return transcriptLoadedMsg{key: key, transcript: tr}
	}
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
