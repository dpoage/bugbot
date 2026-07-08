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

// TestPane_ToolCallRendered verifies that a KindToolCall event updates the
// pane's agent line so the activity note appears in the frame.
func TestPane_ToolCallRendered(t *testing.T) {
	var buf bytes.Buffer
	start := time.Unix(1_000_000, 0)
	p := newTestPane(&buf, 200, fixedClock(start))

	p.Handle(Event{Kind: KindAgentStarted, Role: RoleFinder, Label: "nil-safety"})
	p.Handle(Event{Kind: KindToolCall, Role: RoleFinder, Label: "nil-safety", Phase: "start", Tool: "read_file", File: "main.go"})
	buf.Reset()
	p.paintNow()

	got := StripANSI(buf.String())
	if !strings.Contains(got, "read_file main.go") {
		t.Errorf("expected activity note in pane frame, got:\n%s", got)
	}
}

// TestPane_ToolCallIgnoresUnknownAgent verifies that a KindToolCall event
// for an agent that isn't tracked does NOT add a line or panic.
func TestPane_ToolCallIgnoresUnknownAgent(t *testing.T) {
	var buf bytes.Buffer
	p := newTestPane(&buf, 200, fixedClock(time.Unix(1, 0)))

	// ToolCall for an agent that was never started.
	p.Handle(Event{Kind: KindToolCall, Role: RoleFinder, Label: "ghost", Phase: "start", Tool: "read_file", File: "never.go"})
	buf.Reset()
	p.paintNow()

	got := StripANSI(buf.String())
	if strings.Contains(got, "never.go") {
		t.Errorf("untracked agent tool_call must not appear in pane:\n%s", got)
	}
	if !strings.Contains(got, "(no active agents)") {
		t.Errorf("expected no-active-agents placeholder:\n%s", got)
	}
}

// TestPane_ReproAttemptFoldsIntoAgentActivity verifies a KindReproAttempt
// event folds into the matching active agent's activity note (the same slot
// KindToolCall uses) and updates the pane's last-event line.
func TestPane_ReproAttemptFoldsIntoAgentActivity(t *testing.T) {
	var buf bytes.Buffer
	p := newTestPane(&buf, 200, fixedClock(time.Unix(1_000_000, 0)))

	p.Handle(Event{Kind: KindAgentStarted, Role: RoleReproducer, Label: "nil deref"})
	p.Handle(Event{Kind: KindReproAttempt, Role: RoleReproducer, Label: "nil deref", Attempt: 1, MaxAttempts: 2, Verdict: "exit_zero"})
	buf.Reset()
	p.paintNow()

	got := StripANSI(buf.String())
	if !strings.Contains(got, "attempt 1/2: exit_zero") {
		t.Errorf("expected attempt note in agent line, got:\n%s", got)
	}
	if !strings.Contains(got, "nil deref") {
		t.Errorf("expected finding label in last-event line, got:\n%s", got)
	}
}

// TestPane_ConcurrentAgentsSameLabel_CollisionRegression is the bugbot-r7ub
// regression for the pane: two reproducer agents on the same duplicate
// finding title must render as two distinct agent lines, and a KindToolCall
// carrying one agent's AgentID must only update that agent's activity note.
// Before AgentID/AgentEventKey keying, both KindAgentStarted events folded
// into ONE p.agents entry (keyed by role+label), so this test's line-count
// assertion fails on pre-fix code.
func TestPane_ConcurrentAgentsSameLabel_CollisionRegression(t *testing.T) {
	var buf bytes.Buffer
	p := newTestPane(&buf, 200, fixedClock(time.Unix(1_000_000, 0)))

	const dupTitle = "duplicate open finding"
	p.Handle(Event{Kind: KindAgentStarted, Role: RoleReproducer, Label: dupTitle, AgentID: "agent-A"})
	p.Handle(Event{Kind: KindAgentStarted, Role: RoleReproducer, Label: dupTitle, AgentID: "agent-B"})

	if len(p.agents) != 2 {
		t.Fatalf("p.agents = %d entries, want 2 distinct entries for colliding role+label", len(p.agents))
	}

	p.Handle(Event{
		Kind: KindToolCall, Role: RoleReproducer, Label: dupTitle, AgentID: "agent-A",
		Phase: "start", Tool: "read_file", File: "a.go",
	})
	buf.Reset()
	p.paintNow()
	got := StripANSI(buf.String())

	// Two agent lines must render (both share label/role text, but there must
	// be two occurrences of the label line — one with the activity note, one
	// without).
	if strings.Count(got, dupTitle) != 2 {
		t.Fatalf("expected 2 rendered lines for the colliding label, got:\n%s", got)
	}
	if !strings.Contains(got, "read_file a.go") {
		t.Errorf("expected agent-A's activity note in frame, got:\n%s", got)
	}

	p.Handle(Event{Kind: KindAgentFinished, Role: RoleReproducer, Label: dupTitle, AgentID: "agent-A"})
	if len(p.agents) != 1 {
		t.Fatalf("p.agents = %d after agent-A finished, want 1 (agent-B survives)", len(p.agents))
	}
	if _, ok := p.agents["agent-B"]; !ok {
		t.Errorf("expected agent-B to survive agent-A's finish; p.agents = %+v", p.agents)
	}
}

// TestPane_SpendTicksSumAcrossStreams pins bugbot-psva's aggregation: the
// funnel and the repro stage each emit CUMULATIVE ticks for their own stream
// (Role "" and RoleReproducer); the pane must display the sum of the latest
// per-stream totals, and a scan-finished final total (funnel stream) must not
// erase repro spend.
func TestPane_SpendTicksSumAcrossStreams(t *testing.T) {
	var buf bytes.Buffer
	p := newTestPane(&buf, 200, fixedClock(time.Unix(1_000_000, 0)))

	p.Handle(Event{Kind: KindSpendTick, InputTokens: 1000, OutputTokens: 100})
	p.Handle(Event{Kind: KindSpendTick, Role: RoleReproducer, InputTokens: 300, OutputTokens: 50, CacheReadTokens: 10})
	// Funnel stream ticks again with a HIGHER cumulative total: it must
	// replace the previous funnel value, not stack on top of it.
	p.Handle(Event{Kind: KindSpendTick, InputTokens: 2000, OutputTokens: 200})
	buf.Reset()
	p.paintNow()
	if got := StripANSI(buf.String()); !strings.Contains(got, "in=2300 out=250 total=2550") {
		t.Errorf("pane spend must sum latest per-stream totals (2000+300 / 200+50), got:\n%s", got)
	}

	// Scan-finished totals come from the funnel recorder only.
	p.Handle(Event{Kind: KindScanFinished, InputTokens: 2500, OutputTokens: 220})
	buf.Reset()
	p.paintNow()
	if got := StripANSI(buf.String()); !strings.Contains(got, "in=2800 out=270 total=3070") {
		t.Errorf("scan-finished totals must update the funnel stream without erasing repro spend, got:\n%s", got)
	}
}
