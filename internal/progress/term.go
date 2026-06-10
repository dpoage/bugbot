package progress

import (
	"os"
	"regexp"
	"strconv"
)

// defaultWidth is the assumed terminal width when none can be determined. 80 is
// the universal fallback.
const defaultWidth = 80

// IsTerminal reports whether w is an interactive character device (a TTY),
// using only os.Stat — no isatty/x/sys dependency. It checks the
// os.ModeCharDevice bit, which is set for terminals (and other char devices, but
// the false positives here are harmless: at worst the pane renderer is chosen
// for, say, /dev/null, which a human never sees). A non-*os.File writer is never
// a terminal.
func IsTerminal(w any) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// terminalWidth returns the usable column count for in-place rendering. It is
// syscall-free by design (the spec forbids pulling in x/sys for TIOCGWINSZ):
// it reads $COLUMNS and falls back to defaultWidth. A shell exports COLUMNS for
// interactive sessions; when it is absent or unparseable we assume 80 and
// truncate to it, which never produces garbage — only conservatively short
// lines on an unusually wide terminal.
func terminalWidth() int {
	if v := os.Getenv("COLUMNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultWidth
}

// ansiRE matches CSI escape sequences (ESC [ ... final-byte). Used by tests to
// strip escapes for content assertions and internally to measure visible width.
var ansiRE = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]")

// StripANSI removes CSI escape sequences from s, leaving the visible text. It is
// exported so tests can assert on rendered content without matching exact escape
// bytes.
func StripANSI(s string) string {
	return ansiRE.ReplaceAllString(s, "")
}

// truncate shortens s so its visible length is at most width runes, appending an
// ellipsis when it cuts. It assumes s contains no ANSI escapes (the pane builds
// plain text first, then the renderer adds escapes around whole lines). A
// non-positive width returns "".
func truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	if width == 1 {
		return string(r[:1])
	}
	return string(r[:width-1]) + "…"
}
