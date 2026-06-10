package progress

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// ANSI control sequences used by the pane. Kept as named constants so the render
// path is legible and tests that strip ANSI have a single vocabulary to reason
// about.
const (
	ansiCursorUp     = "\x1b[%dA" // move cursor up n lines
	ansiClearLine    = "\x1b[2K"  // erase the entire current line
	ansiCarriageHome = "\r"       // column 0
	ansiHideCursor   = "\x1b[?25l"
	ansiShowCursor   = "\x1b[?25h"
)

// repaintInterval is the ticker cadence: the pane repaints at least this often
// so the elapsed clock and per-agent running times advance even between events.
const repaintInterval = 250 * time.Millisecond

// eventRepaintMin rate-limits event-driven repaints: an event triggers an
// immediate repaint unless one happened within this window, in which case the
// ticker will pick it up. This bounds repaint frequency under a burst of agent
// events without losing responsiveness.
const eventRepaintMin = 50 * time.Millisecond

// activeAgent tracks one in-flight agent for the live pane.
type activeAgent struct {
	role    string
	label   string
	started time.Time
}

// PaneRenderer maintains an in-place, multi-line ANSI status pane on a TTY. It
// shows a header (scan kind, commit, elapsed), one line per active agent, stage
// counters, cumulative spend, and the last event — repainting on a ticker and
// (rate-limited) on event arrival.
//
// Lifecycle: NewPaneRenderer starts the repaint ticker; feed it events via
// Handle (it satisfies Sink); call Stop once to do a final repaint, restore the
// cursor, and leave the terminal on a clean newline. Stop is idempotent.
//
// Concurrency: Handle is called from parallel agent goroutines and the ticker
// runs in its own goroutine; all shared state is guarded by mu. Handle never
// blocks beyond the short mutex (the non-blocking Sink contract).
type PaneRenderer struct {
	out   io.Writer
	width int

	mu           sync.Mutex
	started      time.Time
	scanKind     string
	commit       string
	counts       Counts
	inTokens     int64
	outTokens    int64
	cachedTokens int64
	agents       map[string]*activeAgent // keyed by role+label
	lastEvent    string
	prevLines    int // lines painted last frame, to know how far to move up
	lastPaint    time.Time
	stopped      bool
	degraded     bool
	budgetNote   string
	now          func() time.Time // injectable for tests
	ticking      bool             // false for the test constructor (no ticker goroutine)

	done chan struct{}
}

// NewPaneRenderer constructs a pane writing to out and starts its repaint
// ticker. width is the column budget for truncation; pass 0 to auto-detect via
// $COLUMNS (fallback 80).
func NewPaneRenderer(out io.Writer, width int) *PaneRenderer {
	if width <= 0 {
		width = terminalWidth()
	}
	p := &PaneRenderer{
		out:     out,
		width:   width,
		agents:  make(map[string]*activeAgent),
		now:     time.Now,
		ticking: true,
		done:    make(chan struct{}),
	}
	p.started = p.now()
	_, _ = io.WriteString(p.out, ansiHideCursor)
	go p.loop()
	return p
}

// newTestPane builds a pane with no ticker goroutine and an injectable clock, so
// tests can drive repaints synchronously and assert on deterministic frames. The
// hide-cursor escape is still written so the output matches production framing.
func newTestPane(out io.Writer, width int, now func() time.Time) *PaneRenderer {
	p := &PaneRenderer{
		out:    out,
		width:  width,
		agents: make(map[string]*activeAgent),
		now:    now,
		done:   make(chan struct{}),
	}
	p.started = p.now()
	_, _ = io.WriteString(p.out, ansiHideCursor)
	return p
}

// loop repaints on the ticker until Stop closes done.
func (p *PaneRenderer) loop() {
	t := time.NewTicker(repaintInterval)
	defer t.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-t.C:
			p.repaint(false)
		}
	}
}

// Handle implements Sink: it updates pane state from the event and triggers a
// rate-limited repaint. It never blocks beyond the mutex.
func (p *PaneRenderer) Handle(ev Event) {
	p.mu.Lock()
	p.apply(ev)
	p.mu.Unlock()
	p.repaint(false)
}

// apply folds an event into pane state. Caller holds mu.
func (p *PaneRenderer) apply(ev Event) {
	switch ev.Kind {
	case KindScanStarted, KindCycleStarted:
		p.scanKind = ev.ScanKind
		p.commit = ev.Commit
		if p.started.IsZero() {
			p.started = p.now()
		}
		p.lastEvent = "started " + ev.ScanKind
	case KindStageStarted:
		p.lastEvent = "stage: " + ev.Stage
	case KindStageFinished:
		if ev.Counts != nil {
			p.mergeCounts(*ev.Counts)
		}
		p.lastEvent = "stage done: " + ev.Stage
	case KindAgentStarted:
		p.agents[agentKey(ev.Role, ev.Label)] = &activeAgent{
			role: ev.Role, label: ev.Label, started: p.now(),
		}
	case KindAgentFinished:
		delete(p.agents, agentKey(ev.Role, ev.Label))
		p.lastEvent = fmt.Sprintf("%s done: %s", ev.Role, ev.Label)
	case KindSpendTick:
		p.inTokens = ev.InputTokens
		p.outTokens = ev.OutputTokens
		p.cachedTokens = ev.CacheReadTokens
	case KindFindingVerified:
		p.counts.Verified++
		p.lastEvent = "verified: " + ev.Title
	case KindBudgetDegraded:
		p.degraded = true
		p.budgetNote = ev.Message
		p.lastEvent = "budget degraded"
	case KindBudgetStopped:
		p.degraded = true
		p.budgetNote = ev.Message
		p.lastEvent = "budget stopped"
	case KindScanFinished, KindCycleFinished:
		if ev.Counts != nil {
			p.mergeCounts(*ev.Counts)
		}
		if ev.InputTokens > 0 || ev.OutputTokens > 0 {
			p.inTokens = ev.InputTokens
			p.outTokens = ev.OutputTokens
			p.cachedTokens = ev.CacheReadTokens
		}
		p.lastEvent = "finished"
	}
}

// mergeCounts updates the running counters, taking the max so a later stage's
// (smaller) verified count never clobbers a finding-driven increment, while
// earlier stages still populate hypothesized/triaged.
func (p *PaneRenderer) mergeCounts(c Counts) {
	if c.Hypothesized > p.counts.Hypothesized {
		p.counts.Hypothesized = c.Hypothesized
	}
	if c.Triaged > p.counts.Triaged {
		p.counts.Triaged = c.Triaged
	}
	if c.Verified > p.counts.Verified {
		p.counts.Verified = c.Verified
	}
	if c.Killed > p.counts.Killed {
		p.counts.Killed = c.Killed
	}
}

// repaint redraws the pane in place. When force is false it is rate-limited
// against the last paint so a burst of events does not thrash the terminal; the
// ticker (which calls with force=false too but on a fixed cadence) guarantees
// the pane still advances. final=true is the Stop path: it paints once more and
// leaves a trailing newline.
func (p *PaneRenderer) repaint(final bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped && !final {
		return
	}
	if !final {
		now := p.now()
		if !p.lastPaint.IsZero() && now.Sub(p.lastPaint) < eventRepaintMin {
			return
		}
		p.lastPaint = now
	}

	lines := p.frame()

	var b strings.Builder
	// On the final repaint, restore the cursor first (the escape does not move
	// it), so the bytes written last are the frame's trailing newline — the
	// terminal is left on a clean new line, not mid-escape.
	if final {
		b.WriteString(ansiShowCursor)
	}
	// Move up over the previous frame and clear each line as we rewrite it.
	if p.prevLines > 0 {
		fmt.Fprintf(&b, ansiCursorUp, p.prevLines)
	}
	for _, ln := range lines {
		b.WriteString(ansiCarriageHome)
		b.WriteString(ansiClearLine)
		b.WriteString(truncate(ln, p.width))
		b.WriteByte('\n')
	}
	// If this frame is shorter than the last, clear the leftover lines so stale
	// content from a taller previous frame does not linger.
	for i := len(lines); i < p.prevLines; i++ {
		b.WriteString(ansiCarriageHome)
		b.WriteString(ansiClearLine)
		b.WriteByte('\n')
	}
	if extra := p.prevLines - len(lines); extra > 0 {
		// Move the cursor back up to the bottom of the current frame so the next
		// repaint's cursor-up count (prevLines) is correct.
		fmt.Fprintf(&b, ansiCursorUp, extra)
	}

	p.prevLines = len(lines)

	_, _ = io.WriteString(p.out, b.String())
}

// paintNow forces a repaint regardless of the rate limit, without the
// final-frame semantics (no newline / cursor restore). Used by tests to render
// deterministically after applying events.
func (p *PaneRenderer) paintNow() {
	p.mu.Lock()
	p.lastPaint = time.Time{}
	p.mu.Unlock()
	p.repaint(false)
}

// frame builds the plain-text lines of the current pane (no ANSI; the repaint
// path wraps each line in clear-line escapes and truncates to width).
func (p *PaneRenderer) frame() []string {
	elapsed := p.now().Sub(p.started).Round(time.Second)
	var lines []string

	header := fmt.Sprintf("bugbot %s  commit %s  elapsed %s",
		dash(p.scanKind), shortSHA(dash(p.commit)), elapsed)
	lines = append(lines, header)

	// Active agents, sorted for stable output.
	keys := make([]string, 0, len(p.agents))
	for k := range p.agents {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		lines = append(lines, "  (no active agents)")
	}
	for _, k := range keys {
		a := p.agents[k]
		run := p.now().Sub(a.started).Round(time.Second)
		lines = append(lines, fmt.Sprintf("  %-8s %-40s %s", a.role, a.label, run))
	}

	lines = append(lines, fmt.Sprintf("  stages: hypothesized=%d triaged=%d verified=%d killed=%d",
		p.counts.Hypothesized, p.counts.Triaged, p.counts.Verified, p.counts.Killed))
	spendLine := fmt.Sprintf("  spend:  in=%d out=%d total=%d tokens",
		p.inTokens, p.outTokens, p.inTokens+p.outTokens)
	if p.cachedTokens > 0 {
		spendLine += fmt.Sprintf(" (cached %d)", p.cachedTokens)
	}
	lines = append(lines, spendLine)
	if p.degraded && p.budgetNote != "" {
		lines = append(lines, "  budget: "+p.budgetNote)
	}
	if p.lastEvent != "" {
		lines = append(lines, "  last:   "+p.lastEvent)
	}
	return lines
}

// Stop ends the ticker, paints a final frame, restores the cursor, and writes a
// trailing newline so the terminal is left clean (not mid-escape). It is
// idempotent and safe to call from defer.
func (p *PaneRenderer) Stop() {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	p.stopped = true
	ticking := p.ticking
	p.mu.Unlock()

	if ticking {
		close(p.done)
	}
	p.repaint(true)
}

// agentKey identifies an active agent by role and label.
func agentKey(role, label string) string { return role + "\x00" + label }

// dash returns "-" for an empty string so header/agent lines never collapse.
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
