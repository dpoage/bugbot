package tui

// source.go implements the jump-to-source pane (bugbot-2p8z.9).
//
// Design: openSourceMsg drives two context-pane sub-modes (contextModeSource
// and contextModeGrep) sitting alongside the existing contextModeSummary /
// contextModeFindings / contextModeLeads in the RIGHT pane (paneContext).
// Rationale: the right pane already has the sub-mode machinery and the source
// view is clearly supplementary context (not a replacement for agent detail
// in paneDetail). The user opens a file and the cockpit stays usable — the
// other two panes keep their state.
//
// Async load pattern mirrors loadTranscriptCmd: a tea.Cmd is returned from
// Update so the file I/O never blocks the bubbletea goroutine. A generation
// counter (sourceLoadGen) lets us discard stale completions exactly as
// transcriptLoadedMsg uses detailKey.
//
// Path safety mirrors internal/agent/fsroot.go: repo-relative only, absolute
// paths rejected, ".." escapes rejected, symlinks checked via EvalSymlinks on
// existing prefixes.

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/charmbracelet/lipgloss"
	xansi "github.com/charmbracelet/x/ansi"

	"github.com/dpoage/bugbot/internal/progress"
)

// ── Caps ─────────────────────────────────────────────────────────────────────

const (
	// sourceMaxBytes is the maximum file size read for source display (1 MB).
	sourceMaxBytes = 1 << 20
	// sourceMaxLines is the maximum number of lines kept (mirrors agent read defaults).
	sourceMaxLines = 2000
	// grepMaxFiles is the maximum number of files walked during a grep search.
	grepMaxFiles = 2000
	// grepMaxHits is the maximum number of hits returned.
	grepMaxHits = 200
	// grepFileMaxBytes is the per-file size cap for grep (1 MB).
	grepFileMaxBytes = 1 << 20
	// binaryDetectBytes is the number of bytes checked for NUL to detect binary files.
	binaryDetectBytes = 8192
)

// ── Styles ───────────────────────────────────────────────────────────────────

var (
	gutterStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	highlightStyle  = lipgloss.NewStyle().Background(lipgloss.Color("3")).Foreground(lipgloss.Color("0"))
	sourceNoteStyle = lipgloss.NewStyle().Faint(true).Italic(true)
	grepFileStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	grepLineStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

// ── Messages ─────────────────────────────────────────────────────────────────

// sourceLoadedMsg is the resolution of loadSourceCmd. gen must match
// m.sourceLoadGen for the result to be applied; stale loads are discarded.
type sourceLoadedMsg struct {
	gen     int
	lines   []string // highlighted, ready-to-render lines (may include ANSI)
	note    string   // set on error, oversized, binary, missing
	file    string   // resolved file path that was loaded (for display)
	line    int      // requested line (1-based) to scroll to
	endLine int      // end of highlighted range
}

// grepLoadedMsg is the resolution of loadGrepCmd.
type grepLoadedMsg struct {
	gen  int
	hits []grepHit
	note string
}

// grepHit is one match from the bounded grep walk.
type grepHit struct {
	file    string // repo-relative path
	line    int    // 1-based
	content string // trimmed line content
}

// ── Path safety ──────────────────────────────────────────────────────────────

// resolveSourcePath resolves a repo-relative path against root with the same
// containment guarantees as internal/agent/fsroot.go: absolute inputs are
// rejected, ".." escapes are rejected lexically and after symlink resolution.
// Returns the absolute path or an error.
func resolveSourcePath(root, rel string) (string, error) {
	rel = filepath.FromSlash(rel)
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("source: absolute paths are not allowed (%q)", rel)
	}
	joined := filepath.Join(root, rel)
	cleaned := filepath.Clean(joined)
	absRoot := filepath.Clean(root)

	// Lexical containment: cleaned must be root itself or under root+sep.
	if cleaned != absRoot && !strings.HasPrefix(cleaned, absRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("source: path escapes the repository root: %q", rel)
	}

	// Symlink containment: resolve longest existing prefix and re-check.
	if resolved, err := evalSourcePrefix(cleaned); err == nil {
		resolvedRoot, _ := filepath.EvalSymlinks(absRoot)
		if resolvedRoot == "" {
			resolvedRoot = absRoot
		}
		if resolved != resolvedRoot && !strings.HasPrefix(resolved, resolvedRoot+string(filepath.Separator)) {
			return "", fmt.Errorf("source: path escapes the repository root via symlink: %q", rel)
		}
	}

	return cleaned, nil
}

// evalSourcePrefix resolves symlinks on the longest existing prefix of p,
// mirroring agent.evalExistingPrefixPath.
func evalSourcePrefix(p string) (string, error) {
	tail := ""
	cur := p
	for {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			if tail == "" {
				return resolved, nil
			}
			return filepath.Join(resolved, tail), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p, nil
		}
		tail = filepath.Join(filepath.Base(cur), tail)
		cur = parent
	}
}

// ── Binary detection ─────────────────────────────────────────────────────────

// isBinary returns true if the first binaryDetectBytes bytes contain a NUL.
func isBinary(data []byte) bool {
	probe := data
	if len(probe) > binaryDetectBytes {
		probe = probe[:binaryDetectBytes]
	}
	return bytes.IndexByte(probe, 0) >= 0
}

// ── Syntax highlighting ──────────────────────────────────────────────────────

// highlightSource applies chroma syntax highlighting to src for the given
// filename (lexer selected by filename, fallback to plain text on any error).
// Returns a slice of ANSI-escaped lines, one per source line (no trailing newline).
// The line count matches the input exactly: no lines are added or removed.
func highlightSource(src []byte, filename string) []string {
	// Binary files never go through chroma.
	if isBinary(src) {
		// caller handles this case before calling us
		return splitLines(src)
	}

	lexer := lexers.Match(filename)
	if lexer == nil {
		lexer = lexers.Fallback
	}
	lexer = chroma.Coalesce(lexer)

	// terminal256 is readable on any 256-colour terminal; terminal16m for true-color.
	// We use terminal256 here: lipgloss uses termenv which negotiates color caps,
	// but chroma's formatter writes directly to the output buffer, so we pick
	// terminal256 as the safe common denominator that works in both.
	formatter := formatters.Get("terminal256")
	if formatter == nil {
		return splitLines(src)
	}

	style := styles.Get("monokai")
	if style == nil {
		style = styles.Fallback
	}

	iter, err := lexer.Tokenise(nil, string(src))
	if err != nil {
		return splitLines(src)
	}

	var buf bytes.Buffer
	if err := formatter.Format(&buf, style, iter); err != nil {
		return splitLines(src)
	}

	// Split the output on newlines, preserving the original line count.
	// chroma may emit a trailing newline; we strip it and align to src lines.
	out := buf.String()
	// Remove trailing newline if present.
	out = strings.TrimRight(out, "\n")
	result := strings.Split(out, "\n")

	// Align: if chroma produced fewer or more lines than source, fall back.
	srcLines := splitLines(src)
	if len(result) != len(srcLines) {
		return srcLines
	}
	return result
}

// splitLines splits src into lines (no trailing newlines on each element).
func splitLines(src []byte) []string {
	s := string(src)
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return []string{}
	}
	return strings.Split(s, "\n")
}

// ── Async load commands ───────────────────────────────────────────────────────

// loadSourceCmd reads and highlights a source file off the Update thread.
// root is the repo root absolute path; msg is the original openSourceMsg.
// gen is the generation counter for stale-load detection.
func loadSourceCmd(gen int, root string, msg openSourceMsg) func() interface{} {
	return func() interface{} {
		absPath, err := resolveSourcePath(root, msg.File)
		if err != nil {
			return sourceLoadedMsg{gen: gen, note: err.Error()}
		}

		f, err := os.Open(absPath)
		if err != nil {
			return sourceLoadedMsg{gen: gen, note: fmt.Sprintf("cannot open file: %v", err)}
		}
		defer func() { _ = f.Close() }()

		limited := io.LimitReader(f, sourceMaxBytes+1)
		data, err := io.ReadAll(limited)
		if err != nil {
			return sourceLoadedMsg{gen: gen, note: fmt.Sprintf("read error: %v", err)}
		}

		truncated := len(data) > sourceMaxBytes
		if truncated {
			data = data[:sourceMaxBytes]
		}

		if isBinary(data) {
			return sourceLoadedMsg{gen: gen, note: fmt.Sprintf("[binary file: %s]", msg.File)}
		}

		lines := highlightSource(data, filepath.Base(msg.File))

		// Truncate to line cap.
		var truncNote string
		if len(lines) > sourceMaxLines {
			lines = lines[:sourceMaxLines]
			truncNote = fmt.Sprintf(" [truncated at %d lines]", sourceMaxLines)
		} else if truncated {
			truncNote = " [truncated at 1 MB]"
		}

		// Append truncation notice as a final dim line.
		if truncNote != "" {
			lines = append(lines, sourceNoteStyle.Render(truncNote))
		}

		return sourceLoadedMsg{
			gen:     gen,
			lines:   lines,
			file:    msg.File,
			line:    msg.Line,
			endLine: msg.EndLine,
		}
	}
}

// loadGrepCmd walks the repo root searching for pattern off the Update thread.
// Caps: max grepMaxFiles files, grepFileMaxBytes per file, grepMaxHits total.
// Invalid regex degrades to literal substring search.
func loadGrepCmd(gen int, root, pattern string) func() interface{} {
	return func() interface{} {
		var rx *regexp.Regexp
		var rxErr error
		rx, rxErr = regexp.Compile(pattern)
		if rxErr != nil {
			// Degrade to literal substring.
			rx = regexp.MustCompile(regexp.QuoteMeta(pattern))
		}

		skipDirs := map[string]bool{".git": true, "vendor": true, "node_modules": true}

		var hits []grepHit
		filesWalked := 0

		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if skipDirs[d.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			if filesWalked >= grepMaxFiles || len(hits) >= grepMaxHits {
				return fs.SkipAll
			}
			filesWalked++

			info, err := d.Info()
			if err != nil || info.Size() > grepFileMaxBytes {
				return nil
			}

			data, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			if isBinary(data) {
				return nil
			}
			if !utf8.Valid(data) {
				return nil
			}

			rel, err := filepath.Rel(root, path)
			if err != nil {
				rel = path
			}
			rel = filepath.ToSlash(rel)

			lines := strings.Split(string(data), "\n")
			for i, line := range lines {
				if len(hits) >= grepMaxHits {
					return nil
				}
				if rx.MatchString(line) {
					trimmed := strings.TrimSpace(line)
					if len(trimmed) > 120 {
						trimmed = trimmed[:117] + "..."
					}
					hits = append(hits, grepHit{
						file:    rel,
						line:    i + 1,
						content: trimmed,
					})
				}
			}
			return nil
		})

		if len(hits) == 0 {
			return grepLoadedMsg{gen: gen, note: fmt.Sprintf("no matches for %q", pattern)}
		}
		return grepLoadedMsg{gen: gen, hits: hits}
	}
}

// ── Rendering ────────────────────────────────────────────────────────────────

// renderSourceView renders the source pane content given the loaded lines,
// file path, target line range, viewport offset, and inner dimensions.
// offset is the 0-based first visible line.
func renderSourceView(lines []string, file string, targetLine, endLine, offset, innerW, innerH int) string {
	var b strings.Builder

	header := headerStyle.Render("Source: "+file) + "\n"
	b.WriteString(header)
	innerH-- // account for header

	if len(lines) == 0 {
		b.WriteString(dimStyle.Render("(empty file)"))
		return b.String()
	}

	// Clamp offset.
	maxOffset := len(lines) - 1
	if offset < 0 {
		offset = 0
	}
	if offset > maxOffset {
		offset = maxOffset
	}

	gutterW := len(fmt.Sprintf("%d", len(lines)))
	linesFmt := fmt.Sprintf("%%%dd", gutterW) // right-aligned line number

	shown := 0
	for i := offset; i < len(lines) && shown < innerH; i++ {
		lineNum := i + 1
		gutter := gutterStyle.Render(fmt.Sprintf(linesFmt, lineNum)) + " "

		lineContent := lines[i]

		// Highlight target range: apply reverse video on top of chroma ANSI.
		// We do this by line index, not by regex, so it's safe over ANSI output.
		// endLine==0 means single-line: clamp end to targetLine so only that
		// one line is marked (not the whole file tail).
		end := endLine
		if end < targetLine {
			end = targetLine
		}
		inRange := targetLine > 0 && lineNum >= targetLine && lineNum <= end
		if inRange {
			// Strip existing ANSI and re-apply highlight style so the mark is visible.
			plain := progress.StripANSI(lineContent)
			// Truncate long lines to inner width minus gutter (ANSI-safe).
			maxContent := innerW - gutterW - 2
			if maxContent < 0 {
				maxContent = 0
			}
			plain = xansi.Truncate(plain, maxContent, "")
			b.WriteString(gutter + highlightStyle.Render(plain) + "\n")
		} else {
			// Truncate preserving chroma ANSI escapes; naive rune-slicing
			// would cut mid-escape and corrupt the terminal output.
			maxContent := innerW - gutterW - 2
			if maxContent < 0 {
				maxContent = 0
			}
			lineContent = xansi.Truncate(lineContent, maxContent, "")
			b.WriteString(gutter + lineContent + "\n")
		}
		shown++
	}

	return b.String()
}

// renderGrepView renders the grep hit list.
func renderGrepView(hits []grepHit, cursor, offset, innerW, innerH int) string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("Grep hits") + "\n")
	innerH--

	if len(hits) == 0 {
		b.WriteString(dimStyle.Render("no matches"))
		return b.String()
	}

	for i := offset; i < len(hits) && i-offset < innerH; i++ {
		h := hits[i]
		prefix := "  "
		if i == cursor {
			prefix = "> "
		}
		filePart := grepFileStyle.Render(h.file)
		linePart := grepLineStyle.Render(fmt.Sprintf(":%d", h.line))
		row := prefix + filePart + linePart + "  " + h.content
		b.WriteString(row + "\n")
	}

	return b.String()
}
