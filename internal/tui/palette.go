package tui

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/dpoage/bugbot/internal/engine"
)

// dispatcher is the subset of *engine.Dispatcher the dispatch palette needs
// to run the five in-process verbs (Scan, Verify, Repro, Sweep, ReviewPR).
// Defined locally — rather than depending on *engine.Dispatcher directly —
// so tests can inject a fake that records calls and blocks on demand,
// without spinning up a real funnel/LLM/sandbox stack. A nil dispatcher on
// Model means dispatch is disabled: Observer mode, or a never-run repo (see
// selectFeed).
type dispatcher interface {
	Scan(context.Context, engine.ScanOpts) (*engine.ScanResult, error)
	Verify(context.Context, engine.VerifyOpts) (*engine.VerifyResult, error)
	Repro(context.Context, engine.ReproOpts) (*engine.ReproResult, error)
	Sweep(context.Context, engine.SweepOpts) (*engine.SweepResult, error)
	ReviewPR(context.Context, engine.ReviewPROpts) (*engine.ReviewPRResult, error)
	Mode() engine.Mode
}

// Compile-time assertion that *engine.Dispatcher satisfies dispatcher.
var _ dispatcher = (*engine.Dispatcher)(nil)

// paletteRow is one selectable row in the dispatch palette. Scan's two scan
// scopes (a whole-snapshot Sweep vs a Since-scoped Targeted diff) are
// modeled as separate rows rather than a sub-menu: navigation stays a flat
// up/down list, the same way every other palette row works.
type paletteRow int

const (
	rowScanSweep paletteRow = iota
	rowScanTargeted
	rowVerify
	rowRepro
	rowSweep
	rowReview

	paletteRowCount
)

// editableRow reports whether row has a text field the palette focuses on
// Enter (Scan Targeted's --since ref, Repro's --max N) rather than
// dispatching immediately.
func (r paletteRow) editableField() bool {
	return r == rowScanTargeted || r == rowRepro || r == rowReview
}

// paletteState holds the dispatch palette's own UI state, separate from
// Model's focus/cursor fields (the palette is a modal overlay rendered over
// the pane compositor).
type paletteState struct {
	open bool
	// cursor indexes the currently highlighted row (0..paletteRowCount-1).
	cursor paletteRow
	// editing is true while keystrokes are being captured into since/maxN
	// instead of driving row navigation (entered via Enter on an
	// editableField row; left via Enter, which submits, or Esc, which just
	// stops capturing and returns to navigation).
	editing bool

	since    textinput.Model // Scan --since ref
	maxN     textinput.Model // Repro --max N
	prNumber textinput.Model // Review --pr N

	suspected bool // Verify --suspected toggle
}

// newPaletteState builds the palette's initial (closed) state.
func newPaletteState() paletteState {
	since := textinput.New()
	since.Placeholder = "ref, e.g. HEAD~5"
	since.CharLimit = 128
	since.Width = 30

	maxN := textinput.New()
	maxN.Placeholder = "default"
	maxN.CharLimit = 6
	maxN.Width = 10

	prNumber := textinput.New()
	prNumber.Placeholder = "PR number"
	prNumber.CharLimit = 8
	prNumber.Width = 10

	return paletteState{since: since, maxN: maxN, prNumber: prNumber}
}

// paletteRowLabel renders row's static label (verb + current sub-choice
// value), independent of enablement/running state.
func (m Model) paletteRowLabel(row paletteRow) string {
	switch row {
	case rowScanSweep:
		return "Scan  — whole-snapshot sweep"
	case rowScanTargeted:
		return fmt.Sprintf("Scan  — targeted --since %s", m.palette.since.View())
	case rowVerify:
		mark := " "
		if m.palette.suspected {
			mark = "x"
		}
		return fmt.Sprintf("Verify — [%s] --suspected (press 's' to toggle)", mark)
	case rowRepro:
		return fmt.Sprintf("Repro — --max %s", m.palette.maxN.View())
	case rowSweep:
		return "Sweep — impact-sweep drain"
	case rowReview:
		return fmt.Sprintf("Review — --pr %s", m.palette.prNumber.View())
	default:
		return "?"
	}
}

// capBuffer is a bounded io.Writer: it retains only the last capBufferMax
// bytes written, so routing a dispatch verb's Out/ErrOut through it can
// never grow Model's memory unboundedly across a long scan. Its mutex is
// defensive rather than load-bearing today (dispatchCmd's goroutine only
// ever finishes writing before its dispatchDoneMsg is delivered to Update,
// which is the only place Tail() is read — see Model's lastOut), but keeps
// capBuffer itself safe to read from at any point without relying on that.
type capBuffer struct {
	mu  sync.Mutex
	buf []byte
}

// capBufferMax bounds capBuffer's retained tail.
const capBufferMax = 4096

func (b *capBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > capBufferMax {
		b.buf = b.buf[len(b.buf)-capBufferMax:]
	}
	return len(p), nil
}

// Tail returns the buffer's current contents as a string.
func (b *capBuffer) Tail() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// dispatchDoneMsg is what dispatchCmd's tea.Cmd resolves to once the
// dispatched verb returns (successfully, with an error, or cancelled).
type dispatchDoneMsg struct {
	verb    string // human-readable label, e.g. "scan --since HEAD~5"
	err     error
	summary string // human-readable result, ignored when err != nil
}

// dispatchCmd runs row against disp off the Update thread — exactly like
// loadTranscriptCmd runs its filesystem read off-thread — because every
// verb can take anywhere from seconds to minutes. ctx is a child of the
// program's own context (see Model.ctx) so the palette's cancel key can stop
// it without quitting the TUI. Out/ErrOut are routed to out (a capBuffer),
// NEVER os.Stdout/Stderr, which would corrupt the bubbletea alt-screen;
// because the Owner Dispatcher was opened with the cockpit's own LiveFeed as
// its progress sink, live agent/stage/spend updates already flow through
// there with no extra wiring — out only backstops the small amount of
// incidental text these verbs also print.
func dispatchCmd(ctx context.Context, cancel context.CancelFunc, disp dispatcher, row paletteRow, since string, maxNText string, prNumberText string, suspected bool, out *capBuffer) tea.Cmd {
	return func() tea.Msg {
		// Always release runCtx's resources once the verb returns, on every
		// path (success, error, or an already-cancelled ctx) — cancelRun
		// only calls this on ctrl+x/esc, so without this defer a dispatch
		// that runs to completion on its own would leak its cancelCtx.
		defer cancel()
		switch row {
		case rowScanSweep:
			res, err := disp.Scan(ctx, engine.ScanOpts{Out: out, ErrOut: out})
			return dispatchDoneMsg{verb: "scan (sweep)", err: err, summary: scanSummary(res)}

		case rowScanTargeted:
			res, err := disp.Scan(ctx, engine.ScanOpts{Since: since, Out: out, ErrOut: out})
			return dispatchDoneMsg{verb: fmt.Sprintf("scan --since %s", since), err: err, summary: scanSummary(res)}

		case rowVerify:
			res, err := disp.Verify(ctx, engine.VerifyOpts{Suspected: suspected, Out: out})
			verb := "verify"
			if suspected {
				verb = "verify --suspected"
			}
			return dispatchDoneMsg{verb: verb, err: err, summary: verifySummary(res)}

		case rowRepro:
			maxN, _ := strconv.Atoi(maxNText) // empty/invalid -> 0 -> engine default
			res, err := disp.Repro(ctx, engine.ReproOpts{MaxN: maxN, Out: out})
			verb := "repro"
			if maxN > 0 {
				verb = fmt.Sprintf("repro --max %d", maxN)
			}
			return dispatchDoneMsg{verb: verb, err: err, summary: reproSummary(res)}

		case rowSweep:
			res, err := disp.Sweep(ctx, engine.SweepOpts{Out: out, ErrOut: out})
			return dispatchDoneMsg{verb: "sweep", err: err, summary: sweepSummary(res)}

		case rowReview:
			prNumber, convErr := strconv.Atoi(strings.TrimSpace(prNumberText))
			if convErr != nil || prNumber <= 0 {
				return dispatchDoneMsg{verb: "review", err: fmt.Errorf("review requires a positive PR number, got %q", prNumberText)}
			}
			res, err := disp.ReviewPR(ctx, engine.ReviewPROpts{PRNumber: prNumber, Suspected: "summary", GH: engine.RealGH, Out: out, ErrOut: out})
			return dispatchDoneMsg{verb: fmt.Sprintf("review --pr %d", prNumber), err: err, summary: reviewSummary(res)}

		default:
			return dispatchDoneMsg{verb: "dispatch", err: fmt.Errorf("tui: unrecognized dispatch row %d", row)}
		}
	}
}

func scanSummary(res *engine.ScanResult) string {
	if res == nil || res.Result == nil {
		return "scan complete"
	}
	return fmt.Sprintf("scan complete: %d finding(s)", len(res.Result.Findings))
}

func verifySummary(res *engine.VerifyResult) string {
	if res == nil || res.Drain == nil {
		return "verify: no pending candidates"
	}
	return fmt.Sprintf("verify complete: %d finding(s)", len(res.Drain.Findings))
}

func reproSummary(res *engine.ReproResult) string {
	if res == nil {
		return "repro complete"
	}
	if res.Skipped != "" {
		return "repro skipped: " + res.Skipped
	}
	if res.Summary == nil {
		return "repro: empty backlog"
	}
	return fmt.Sprintf("repro complete: %d attempted", res.Summary.Attempted)
}

func sweepSummary(res *engine.SweepResult) string {
	if res == nil || res.Result == nil {
		return "sweep: no unswept findings"
	}
	return fmt.Sprintf("sweep complete: %d finding(s)", len(res.Result.Findings))
}

func reviewSummary(res *engine.ReviewPRResult) string {
	if res == nil {
		return "review complete"
	}
	count := 0
	if res.Result != nil {
		count = len(res.Result.Findings)
	}
	return fmt.Sprintf("review complete: PR #%d, %d finding(s), %d new verified", res.PRNumber, count, res.NewVerifiedCount)
}

// handlePaletteKey handles a keypress while the palette overlay is open. It
// is called from handleKey before the normal top-level key switch, and
// returns handled=false for a key it does not consume (letting a real
// tea.Program's default handling — there is none beyond handleKey — apply;
// currently every key while the palette is open is either consumed here or
// ignored).
func (m Model) handlePaletteKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	// ctrl+x is intercepted globally in handleKey. Inside the palette,
	// 'x' also cancels an active run — giving a clearly-labeled affordance
	// for the most destructive action. esc closes the palette without any
	// side-effects: run cancellation is never implicit.
	if m.palette.editing {
		field := m.paletteEditingField()
		switch msg.Type {
		case tea.KeyEsc:
			m.palette.editing = false
			return m, nil
		case tea.KeyEnter:
			m.palette.editing = false
			return m.confirmPaletteRow()
		default:
			var cmd tea.Cmd
			*field, cmd = field.Update(msg)
			return m, cmd
		}
	}

	switch msg.String() {
	case "esc", "d":
		// Both esc and 'd' close the palette without side-effects.
		// 'd' closes because it is the toggle key (closed→open handled in
		// model.go handleKey; open→close handled here).
		m.palette.open = false
		return m, nil

	case "x":
		// Explicit in-palette cancel for the active run. Only meaningful
		// while running; a no-op otherwise so the key is safe to press.
		if m.running {
			m = m.cancelRun()
		}
		return m, nil

	case "j", "down":
		m.palette.cursor = (m.palette.cursor + 1) % paletteRowCount
		return m, nil

	case "k", "up":
		m.palette.cursor = (m.palette.cursor - 1 + paletteRowCount) % paletteRowCount
		return m, nil

	case "s":
		if m.palette.cursor == rowVerify {
			m.palette.suspected = !m.palette.suspected
		}
		return m, nil

	case "enter":
		if m.palette.cursor.editableField() {
			m.palette.editing = true
			m.paletteEditingField().Focus()
			// No textinput.Blink cmd: the cursor blink animation is a
			// cosmetic nicety a real terminal renders fine without an
			// explicit tick (Focus() alone enables the cursor), and
			// scheduling one here would tie every "enter to edit" keypress
			// to the cursor package's blink interval in tests that run
			// tea.Cmds synchronously (see sendKey/runCmd).
			return m, nil
		}
		return m.confirmPaletteRow()
	}
	return m, nil
}

// paletteEditingField returns the textinput.Model backing the row currently
// under the cursor, for rows that have one.
func (m *Model) paletteEditingField() *textinput.Model {
	switch m.palette.cursor {
	case rowRepro:
		return &m.palette.maxN
	case rowReview:
		return &m.palette.prNumber
	default:
		return &m.palette.since
	}
}

// confirmPaletteRow starts a dispatch for the row under the cursor, unless
// dispatch is disabled (m.disp == nil) or a run is already active — both are
// silent no-ops, matching the palette's own rendered gating text.
func (m Model) confirmPaletteRow() (Model, tea.Cmd) {
	if m.disp == nil || m.running {
		return m, nil
	}

	row := m.palette.cursor
	since := m.palette.since.Value()
	maxNText := m.palette.maxN.Value()
	prNumberText := m.palette.prNumber.Value()
	suspected := m.palette.suspected

	runCtx, cancel := context.WithCancel(m.ctx)
	m.running = true
	m.runVerb = m.paletteRowLabel(row)
	m.runStarted = time.Now()
	m.runCancel = cancel
	m.runOut = &capBuffer{}
	m.palette.open = false

	return m, dispatchCmd(runCtx, cancel, m.disp, row, since, maxNText, prNumberText, suspected, m.runOut)
}

// cancelRun stops the active dispatch's context, if one is running. The
// dispatch's own tea.Cmd goroutine is still in flight until it observes
// ctx.Done and returns dispatchDoneMsg — cancelRun itself does not clear
// m.running; Update's dispatchDoneMsg case does, once the verb actually
// unwinds.
func (m Model) cancelRun() Model {
	if m.runCancel != nil {
		m.runCancel()
	}
	return m
}

// isCancelled reports whether err is (or wraps) context.Canceled, the
// dispatchDoneMsg case renders as "cancelled" rather than an error.
func isCancelled(err error) bool {
	return errors.Is(err, context.Canceled)
}

// viewPalette renders the dispatch palette overlay.
//
// While a run is active the running verb and elapsed time are shown
// prominently at the top, every dispatch row is rendered disabled with a
// "run in progress" reason, and an 'x' cancel affordance appears in the
// footer. This makes the one-at-a-time policy visible rather than silent.
// esc closes the palette without affecting the run; 'x' (or the global
// ctrl+x) cancels it.
func (m Model) viewPalette() string {
	var b []byte
	b = append(b, headerStyle.Render("dispatch palette")...)
	b = append(b, '\n')

	if m.disp == nil {
		b = append(b, dimStyle.Render("read-only: another process holds the writer lock")...)
		b = append(b, '\n')
	} else if m.running {
		b = append(b, sectionStyle.Render(fmt.Sprintf("running: %s (%s)", m.runVerb, elapsedSince(m.runStarted)))...)
		b = append(b, '\n')
	}
	b = append(b, '\n')

	for row := paletteRow(0); row < paletteRowCount; row++ {
		line := m.paletteRowLabel(row)
		if row == m.palette.cursor {
			line = selectedStyle.Render("> " + line)
		} else {
			line = "  " + line
		}
		if m.disp == nil || m.running {
			// Render the row dim; append a reason so the operator knows
			// why it cannot be activated rather than seeing a silent no-op.
			var reason string
			if m.disp == nil {
				reason = " (read-only)"
			} else {
				reason = " (run in progress)"
			}
			line = dimStyle.Render(line + reason)
		}
		b = append(b, line...)
		b = append(b, '\n')
	}

	b = append(b, '\n')
	if m.palette.editing {
		b = append(b, footerStyle.Render("type value · enter confirm · esc stop editing")...)
	} else if m.running {
		b = append(b, footerStyle.Render(fmt.Sprintf("x cancel running %s (%s) · ctrl+x cancel · esc close", m.runVerb, elapsedSince(m.runStarted)))...)
	} else {
		b = append(b, footerStyle.Render("j/k move · enter select/edit · s toggle suspected · d/esc close")...)
	}

	if m.lastVerb != "" {
		b = append(b, "\n\n"...)
		b = append(b, sectionStyle.Render("last dispatch")...)
		b = append(b, '\n')
		b = append(b, dispatchResultLine(m.lastVerb, m.lastErr, m.lastResult)...)
		if tail := lastOutTail(m.lastOut); tail != "" {
			b = append(b, '\n')
			b = append(b, dimStyle.Render(tail)...)
		}
	}

	return string(b)
}

// dispatchResultLine renders the compact result of the most recent dispatch
// (success, error, or cancelled), shared by the palette overlay and the
// Cockpit's status line.
func dispatchResultLine(verb string, err error, result string) string {
	switch {
	case err == nil:
		return fmt.Sprintf("%s: %s", verb, result)
	case isCancelled(err):
		return fmt.Sprintf("%s: cancelled", verb)
	default:
		return errorNoteStyle.Render(fmt.Sprintf("%s: error: %v", verb, err))
	}
}

// dispatchStatusLine renders the Cockpit's compact one-line dispatch status:
// the active run's verb+elapsed while running, otherwise the last completed
// run's result — or ok=false when neither applies (no dispatch has run yet).
func (m Model) dispatchStatusLine() (string, bool) {
	switch {
	case m.running:
		return sectionStyle.Render("dispatch") + " " +
			fmt.Sprintf("running: %s (%s)", m.runVerb, elapsedSince(m.runStarted)), true
	case m.lastVerb != "":
		return sectionStyle.Render("dispatch") + " " + dispatchResultLine(m.lastVerb, m.lastErr, m.lastResult), true
	default:
		return "", false
	}
}

// lastOutTailLines bounds how much of a completed dispatch's own Out/ErrOut
// text the palette shows underneath its typed *Result summary — a raw dump
// of capBuffer's full retained tail would overwhelm the overlay.
const lastOutTailLines = 5

// lastOutTail returns the last lastOutTailLines non-empty lines of out,
// trimmed, joined back with newlines — "" when out is blank.
func lastOutTail(out string) string {
	out = strings.TrimSpace(out)
	if out == "" {
		return ""
	}
	lines := strings.Split(out, "\n")
	if len(lines) > lastOutTailLines {
		lines = lines[len(lines)-lastOutTailLines:]
	}
	return strings.Join(lines, "\n")
}
