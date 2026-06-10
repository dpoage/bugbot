package progress

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// fixedClock returns a clock function pinned to t, so pane frames are
// deterministic (elapsed/running times are stable).
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func TestPane_RendersHeaderAgentsCountsSpend(t *testing.T) {
	var buf bytes.Buffer
	start := time.Unix(1_000_000, 0)
	p := newTestPane(&buf, 200, fixedClock(start))

	p.Handle(Event{Kind: KindScanStarted, ScanKind: "sweep", Commit: "abcdef1234567890"})
	p.Handle(Event{Kind: KindStageStarted, Stage: StageHypothesize})
	p.Handle(Event{Kind: KindAgentStarted, Role: RoleFinder, Label: "nil-safety/error-handling"})
	p.Handle(Event{Kind: KindAgentStarted, Role: RoleVerifier, Label: "some bug title"})
	p.Handle(Event{Kind: KindSpendTick, InputTokens: 1200, OutputTokens: 340})
	p.Handle(Event{Kind: KindStageFinished, Stage: StageHypothesize, Counts: &Counts{Hypothesized: 5}})
	p.paintNow()

	got := StripANSI(buf.String())

	for _, want := range []string{
		"bugbot sweep",
		"commit abcdef123456", // shortSHA truncates to 12
		"elapsed",
		"finder",
		"nil-safety/error-handling",
		"verifier",
		"some bug title",
		"hypothesized=5",
		"in=1200 out=340 total=1540",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("pane output missing %q\n---\n%s", want, got)
		}
	}
}

func TestPane_AgentFinishedRemovesAgent(t *testing.T) {
	var buf bytes.Buffer
	p := newTestPane(&buf, 200, fixedClock(time.Unix(1, 0)))

	p.Handle(Event{Kind: KindAgentStarted, Role: RoleFinder, Label: "lensA"})
	p.Handle(Event{Kind: KindAgentFinished, Role: RoleFinder, Label: "lensA"})
	// Reset so we assert only on the final frame, not the earlier (rate-limited)
	// repaints that still held the agent.
	buf.Reset()
	p.paintNow()

	got := StripANSI(buf.String())
	// The agent must no longer appear as an active-agent line. (The "last event"
	// line legitimately still names it, e.g. "finder done: lensA", so we check
	// the placeholder rather than mere substring presence.)
	if !strings.Contains(got, "(no active agents)") {
		t.Errorf("expected no-active-agents placeholder:\n%s", got)
	}
}

func TestPane_NarrowTerminalTruncates(t *testing.T) {
	var buf bytes.Buffer
	const width = 24
	p := newTestPane(&buf, width, fixedClock(time.Unix(1, 0)))

	p.Handle(Event{Kind: KindAgentStarted, Role: RoleFinder, Label: strings.Repeat("x", 200)})
	p.paintNow()

	// Every rendered line's visible content must fit the width; truncate appends
	// an ellipsis rune so visible length stays <= width. Strip ANSI escapes and
	// the leading carriage-return (column-0 control, not printable width).
	for _, line := range strings.Split(buf.String(), "\n") {
		visible := []rune(strings.TrimPrefix(StripANSI(line), "\r"))
		if len(visible) > width {
			t.Errorf("line exceeds width %d: %d runes %q", width, len(visible), string(visible))
		}
	}
}

func TestPane_StopLeavesCleanNewlineAndShowsCursor(t *testing.T) {
	var buf bytes.Buffer
	p := newTestPane(&buf, 80, fixedClock(time.Unix(1, 0)))
	p.Handle(Event{Kind: KindScanStarted, ScanKind: "oneshot"})
	p.paintNow()
	p.Stop()

	out := buf.String()
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("Stop did not leave a trailing newline; terminal left mid-line")
	}
	if !strings.Contains(out, ansiShowCursor) {
		t.Errorf("Stop did not restore the cursor (missing show-cursor escape)")
	}
}

func TestPane_StopIdempotent(t *testing.T) {
	var buf bytes.Buffer
	p := newTestPane(&buf, 80, fixedClock(time.Unix(1, 0)))
	p.Stop()
	p.Stop() // must not panic or double-close
}

func TestPane_RepaintRedrawsInPlace(t *testing.T) {
	var buf bytes.Buffer
	p := newTestPane(&buf, 200, fixedClock(time.Unix(1, 0)))

	p.Handle(Event{Kind: KindScanStarted, ScanKind: "sweep"})
	p.paintNow()
	first := buf.Len()
	buf.Reset()

	// A second paint must move the cursor up over the previous frame before
	// rewriting, otherwise the pane scrolls instead of repainting in place.
	p.paintNow()
	second := buf.String()
	if !strings.Contains(second, "\x1b[") || !strings.Contains(second, "A") {
		t.Errorf("second paint lacks cursor-up escape; not repainting in place:\n%q", second)
	}
	if first == 0 {
		t.Fatal("first paint wrote nothing")
	}
}
