package tui

// openSourceMsg asks the source pane to open a file location or a grep
// pattern's hit list. Emitted by the live action feed (bugbot-2p8z.8) when
// the user presses enter on a file-bearing or pattern-bearing tool-call row;
// handled by the jump-to-source pane (bugbot-2p8z.9). A Model that does not
// yet handle it ignores it by design, so the two features compose without a
// hard merge ordering.
type openSourceMsg struct {
	// File is the repo-relative target; empty for pattern-only (grep)
	// actions, which open the hit-list view instead.
	File string
	// Line and EndLine bound the highlighted range, 1-based inclusive;
	// zero means no specific range (open at top).
	Line, EndLine int
	// Pattern, when non-empty, requests the grep hit-list view for the
	// pattern instead of (or in addition to) a single file target.
	Pattern string
}
