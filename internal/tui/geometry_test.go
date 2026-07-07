package tui

// geometry_test.go exercises the pane-geometry invariants introduced in
// bugbot-2p8z.13: every rendered frame must fit within the declared terminal
// dimensions and scrolling one pane must not shift the others.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/store"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// setSize sends a WindowSizeMsg and returns the resized model.
func setSize(m Model, w, h int) Model {
	next, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return next.(Model)
}

// overflowFrame builds a Frame with deliberately overflowing content:
// 30 agents (roster overflow), 50 findings, 40 leads, long titles, so every
// windowed renderer must clip to innerH rather than render everything.
func overflowFrame() Frame {
	agents := make([]AgentView, 30)
	for i := range agents {
		agents[i] = AgentView{
			Role:    fmt.Sprintf("role-%02d", i),
			Label:   fmt.Sprintf("label-%02d", i),
			Live:    i%3 == 0,
			Started: time.Unix(int64(1000+i), 0),
			Status:  "ok",
		}
	}

	findings := make([]domain.Finding, 50)
	for i := range findings {
		findings[i] = domain.Finding{
			Title: fmt.Sprintf("finding-%02d — a very long title that exceeds innerW for sure", i),
			File:  fmt.Sprintf("src/package/deep/path/file_%02d.go", i),
		}
	}

	leads := make([]store.Lead, 40)
	for i := range leads {
		leads[i] = store.Lead{
			TargetLens: fmt.Sprintf("lens-%02d", i),
			File:       fmt.Sprintf("src/pkg/file_%02d.go", i),
			Line:       i*10 + 1,
			Note:       fmt.Sprintf("potential nil dereference at line %d — needs manual review", i*10+1),
		}
	}

	return Frame{
		HasSnapshot: true,
		World: WorldState{
			HasTallies: true,
			Tallies: domain.FindingTallies{
				OpenByTier: map[int]int{0: 5, 1: 10, 2: 20, 3: 15},
				Fixed:      4,
				Dismissed:  1,
				NeedsHuman: 3,
			},
			Findings:          findings,
			FindingsTotal:     len(findings),
			PendingLeads:      leads,
			PendingLeadsTotal: len(leads),
		},
		Agents:     agents,
		ActionRows: map[string][]ActionRow{},
	}
}

// assertFrameDimensions asserts that the rendered view for model m (at given
// terminal dimensions w×h) fits within those bounds. Must be called on the
// RAW string (not ANSI-stripped) because lipgloss measures with ANSI.
func assertFrameDimensions(t *testing.T, label string, m Model, w, h int) {
	t.Helper()
	got := m.View()
	gotH := lipgloss.Height(got)
	gotW := lipgloss.Width(got)
	if gotH != h {
		t.Errorf("%s: lipgloss.Height(View()) = %d, want %d", label, gotH, h)
	}
	if gotW > w {
		t.Errorf("%s: lipgloss.Width(View()) = %d, want <= %d", label, gotW, w)
	}
}

// ── Golden geometry sweep ─────────────────────────────────────────────────────

// TestGeometry_GoldenSweep exercises every pane-focus × contextMode ×
// detailMode combination with overflowing content at 80×24 and 120×40.
// For every combination it asserts lipgloss.Height == terminal height and
// lipgloss.Width <= terminal width.
func TestGeometry_GoldenSweep(t *testing.T) {
	fr := overflowFrame()

	for _, sz := range []struct{ w, h int }{{80, 24}, {120, 40}} {
		for _, focus := range []pane{paneRoster, paneDetail, paneContext} {
			for _, ctxMode := range []contextMode{
				contextModeSummary,
				contextModeFindings,
				contextModeLeads,
			} {
				for _, detailMode := range []bool{false, true} {
					label := fmt.Sprintf("%dx%d focus=%d ctx=%d detail=%v", sz.w, sz.h, focus, ctxMode, detailMode)
					m := NewModel(context.Background(), &fakeFeed{}, nil)
					m = setSize(m, sz.w, sz.h)
					m = sendFrame(m, fr)
					m.focus = focus
					m.contextMode = ctxMode
					m.detailMode = detailMode
					// cursor at middle so windowing is exercised both ways
					m.cursor = 25
					assertFrameDimensions(t, label, m, sz.w, sz.h)
				}
			}
		}
	}
}

// TestGeometry_SourcePaneDoesNotExceedBounds tests the source pane with
// 200 source lines including a 300-column ANSI line.
func TestGeometry_SourcePaneDoesNotExceedBounds(t *testing.T) {
	// Build 200 source lines; line 100 is a 300-char ANSI-coloured line.
	lines := make([]string, 200)
	for i := range lines {
		if i == 99 {
			// Simulate a chroma-highlighted line wider than any pane.
			lines[i] = "\x1b[32m" + strings.Repeat("x", 300) + "\x1b[0m"
		} else {
			lines[i] = fmt.Sprintf("line %d: normal source content here", i+1)
		}
	}

	for _, sz := range []struct{ w, h int }{{80, 24}, {120, 40}} {
		for _, offset := range []int{0, 50, 100, 150, 199} {
			label := fmt.Sprintf("%dx%d offset=%d", sz.w, sz.h, offset)
			m := NewModel(context.Background(), &fakeFeed{}, nil)
			m = setSize(m, sz.w, sz.h)
			fr := overflowFrame()
			m = sendFrame(m, fr)
			m.contextMode = contextModeSource
			m.sourceLines = lines
			m.sourceFile = "src/big.go"
			m.sourceLine = 100
			m.sourceEndLine = 100
			m.sourceOffset = offset
			assertFrameDimensions(t, label, m, sz.w, sz.h)
		}
	}
}

// TestGeometry_GrepPaneDoesNotExceedBounds tests the grep pane with many hits.
func TestGeometry_GrepPaneDoesNotExceedBounds(t *testing.T) {
	hits := make([]grepHit, 200)
	for i := range hits {
		hits[i] = grepHit{
			file:    fmt.Sprintf("src/deep/path/file_%d.go", i),
			line:    i + 1,
			content: strings.Repeat("match content ", 20), // wide line
		}
	}

	for _, sz := range []struct{ w, h int }{{80, 24}, {120, 40}} {
		for _, cursor := range []int{0, 50, 100, 199} {
			label := fmt.Sprintf("%dx%d cursor=%d", sz.w, sz.h, cursor)
			m := NewModel(context.Background(), &fakeFeed{}, nil)
			m = setSize(m, sz.w, sz.h)
			m = sendFrame(m, overflowFrame())
			m.contextMode = contextModeGrep
			m.grepHits = hits
			m.grepCursor = cursor
			m.grepOffset = max(0, cursor-5)
			assertFrameDimensions(t, label, m, sz.w, sz.h)
		}
	}
}

// ── Scroll-isolation regression ───────────────────────────────────────────────

// TestGeometry_ScrollSourceDoesNotShiftSiblings is the core regression for
// bugbot-2p8z.13: scrolling the source (context) pane must leave the roster
// and detail pane rendered substrings byte-identical before and after each
// scroll step.
func TestGeometry_ScrollSourceDoesNotShiftSiblings(t *testing.T) {
	// Build a real source file so the source pane has content to scroll.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for i := 1; i <= 100; i++ {
		fmt.Fprintf(&sb, "// line %d: some code here\n", i)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "scroll_test.go"), []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	m := NewModel(context.Background(), &fakeFeed{}, nil).WithRepoRoot(root)
	m = setSize(m, 80, 24)
	fr := overflowFrame()
	m = sendFrame(m, fr)

	// Drill into source pane: open the file.
	m.contextMode = contextModeSource
	m.sourceFile = "src/scroll_test.go"
	m.sourceLine = 1
	m.sourceEndLine = 0
	// Build lines manually (avoid async load in tests).
	srcLines := make([]string, 100)
	for i := range srcLines {
		srcLines[i] = fmt.Sprintf("// line %d: some code here", i+1)
	}
	m.sourceLines = srcLines
	m.sourceOffset = 0
	m.focus = paneContext

	// extractSiblings slices the roster column (cols 0-25) and detail column
	// (cols 26-51) from every ANSI-stripped terminal row. These column bounds
	// are deterministic: layoutDimensions gives paneW=26 at 80 cols, so each
	// pane occupies exactly 26 screen columns. We join the slices from all
	// rows so a single comparison proves neither sibling pane changed.
	extractSiblings := func(m Model) (roster, detail string) {
		stripped := stripANSI(m.View())
		lines := strings.Split(stripped, "\n")
		var rlines, dlines []string
		for _, l := range lines {
			runes := []rune(l)
			// Roster pane: columns 0-25 (26 wide).
			rEnd := 26
			if rEnd > len(runes) {
				rEnd = len(runes)
			}
			rlines = append(rlines, string(runes[:rEnd]))
			// Detail pane: columns 26-51.
			dStart := 26
			dEnd := 52
			if dStart > len(runes) {
				dStart = len(runes)
			}
			if dEnd > len(runes) {
				dEnd = len(runes)
			}
			dlines = append(dlines, string(runes[dStart:dEnd]))
		}
		return strings.Join(rlines, "\n"), strings.Join(dlines, "\n")
	}

	rosterBefore, detailBefore := extractSiblings(m)

	// Scroll the source pane N times; siblings must be byte-identical.
	for step := 0; step < 10; step++ {
		m = sendKey(m, "j")
		rosterAfter, detailAfter := extractSiblings(m)
		if rosterAfter != rosterBefore {
			t.Errorf("step %d: roster pane changed after source scroll\nbefore: %q\nafter:  %q", step, rosterBefore, rosterAfter)
		}
		if detailAfter != detailBefore {
			t.Errorf("step %d: detail pane changed after source scroll\nbefore: %q\nafter:  %q", step, detailBefore, detailAfter)
		}
	}
}

// ── Cursor visibility ─────────────────────────────────────────────────────────

// TestGeometry_CursorVisibleFindings verifies that with 50 findings and the
// cursor at positions 0, 25, and 49, the selected row is present in the
// rendered output of the context pane.
func TestGeometry_CursorVisibleFindings(t *testing.T) {
	fr := overflowFrame()
	for _, cursorPos := range []int{0, 25, 49} {
		m := NewModel(context.Background(), &fakeFeed{}, nil)
		m = setSize(m, 80, 24)
		m = sendFrame(m, fr)
		m.focus = paneContext
		m.contextMode = contextModeFindings
		m.cursor = cursorPos

		view := stripANSI(m.View())
		// The selected finding title should appear somewhere in the view.
		wantTitle := fmt.Sprintf("finding-%02d", cursorPos)
		if !strings.Contains(view, wantTitle) {
			t.Errorf("cursor=%d: finding title %q not visible in rendered view", cursorPos, wantTitle)
		}
	}
}

// TestGeometry_CursorVisibleLeads verifies cursor visibility for 40 leads.
func TestGeometry_CursorVisibleLeads(t *testing.T) {
	fr := overflowFrame()
	for _, cursorPos := range []int{0, 20, 39} {
		m := NewModel(context.Background(), &fakeFeed{}, nil)
		m = setSize(m, 80, 24)
		m = sendFrame(m, fr)
		m.focus = paneContext
		m.contextMode = contextModeLeads
		m.cursor = cursorPos

		view := stripANSI(m.View())
		wantLens := fmt.Sprintf("lens-%02d", cursorPos)
		if !strings.Contains(view, wantLens) {
			t.Errorf("cursor=%d: lead lens %q not visible in rendered view", cursorPos, wantLens)
		}
	}
}

// TestGeometry_CursorVisibleRoster verifies cursor visibility in roster pane
// with 30 agents.
func TestGeometry_CursorVisibleRoster(t *testing.T) {
	fr := overflowFrame()
	for _, cursorPos := range []int{0, 15, 29} {
		m := NewModel(context.Background(), &fakeFeed{}, nil)
		m = setSize(m, 120, 40)
		m = sendFrame(m, fr)
		m.focus = paneRoster
		m.cursor = cursorPos

		view := stripANSI(m.View())
		wantLabel := fmt.Sprintf("label-%02d", cursorPos)
		if !strings.Contains(view, wantLabel) {
			t.Errorf("cursor=%d: agent label %q not visible in rendered view", cursorPos, wantLabel)
		}
	}
}

// ── ANSI-safe truncation ──────────────────────────────────────────────────────

// TestANSITruncate_SourceViewWidthBound verifies that renderSourceView never
// emits a line whose ANSI-stripped width exceeds innerW, including when input
// lines contain mid-SGR sequences and multi-byte runes.
func TestANSITruncate_SourceViewWidthBound(t *testing.T) {
	cases := []struct {
		name    string
		line    string
		innerW  int
		gutterW int
	}{
		{
			name:    "plain ASCII wider than innerW",
			line:    strings.Repeat("x", 300),
			innerW:  40,
			gutterW: 3,
		},
		{
			name:    "chroma ANSI wider than innerW",
			line:    "\x1b[32m" + strings.Repeat("x", 300) + "\x1b[0m",
			innerW:  40,
			gutterW: 3,
		},
		{
			name:    "multi-byte runes (CJK) wider than innerW",
			line:    strings.Repeat("中", 80),
			innerW:  40,
			gutterW: 3,
		},
		{
			name:    "mid-SGR boundary: reset mid-line then more colour",
			line:    "\x1b[1;31mhello\x1b[0m\x1b[34m" + strings.Repeat("b", 300) + "\x1b[0m",
			innerW:  40,
			gutterW: 3,
		},
		{
			name:    "line exactly innerW - gutterW - 2 (no truncation needed)",
			line:    strings.Repeat("y", 35),
			innerW:  40,
			gutterW: 3,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Render through renderSourceView with a single line.
			lines := []string{tc.line}
			got := renderSourceView(lines, "test.go", 0, 0, 0, tc.innerW, 5)
			// Check every rendered content line (skip the header).
			rendered := strings.Split(got, "\n")
			for _, rl := range rendered[1:] { // skip "Source: test.go" header
				if rl == "" {
					continue
				}
				stripped := stripANSI(rl)
				// Measure rune width (CJK counts as 2).
				w := 0
				for _, r := range stripped {
					if r >= 0x1100 { // rough CJK wide check
						w += 2
					} else {
						w++
					}
				}
				if w > tc.innerW {
					t.Errorf("rendered line width %d > innerW %d\nline: %q\nstripped: %q", w, tc.innerW, rl, stripped)
				}
			}
		})
	}
}

// max is a small helper for Go <1.21 compat (min/max builtins added in 1.21).
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
