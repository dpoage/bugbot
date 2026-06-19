package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/lsp"
)

// read_symbol body limits. These bound a single result so returning a
// declaration cannot blow the model's context. The line cap is far smaller than
// read_file's because the whole point is to fetch ONE declaration; the rare
// giant function is truncated with a read_file follow-up pointer rather than
// inlined. The byte cap guards against a small number of pathologically long
// lines (minified/generated code) defeating the line cap.
const (
	// readSymbolMaxLines caps the body lines rendered for the best match.
	readSymbolMaxLines = 400
	// readSymbolMaxBytes caps the bytes of body text rendered, independent of
	// line count.
	readSymbolMaxBytes = 32 * 1024
	// readSymbolWindowLines is the positional window size (def line + following
	// lines) used when the file's language has no tree-sitter grammar, so an
	// exact body span is unavailable. It is bounded by readSymbolMaxLines too.
	readSymbolWindowLines = 100
	// readSymbolMaxCandidates caps how many alternate candidate locations are
	// listed after the rendered best match, so an ambiguous common name cannot
	// flood the result.
	readSymbolMaxCandidates = 20
)

// readSymbolTool returns the FULL BODY of a symbol's declaration as numbered
// lines, so a finder pulls a 40-line function instead of a capped whole-file
// read. It shares the CodeNav bundle's root, tiered navigator, and tree-sitter
// backend.
type readSymbolTool struct {
	nav *CodeNav
}

func (t *readSymbolTool) Def() llm.ToolDef {
	return llm.ToolDef{
		Name: "read_symbol",
		Description: "Return the full body of a symbol's declaration (function, method, " +
			"type, class, def) as numbered lines. Prefer this over read_file when you " +
			"need one function/method/type: it returns just that declaration's body — " +
			"a ~40-line function instead of a capped whole-file read. Point it at any " +
			"occurrence, exactly like find_definition: give the repo-relative file, the " +
			"1-based line where the symbol appears (a call site is fine), and the symbol " +
			"name as written on that line; the definition may live in another file and " +
			"is rendered from there. For tree-sitter-supported languages (Go, Python, " +
			"TypeScript, TSX) the body is exact; for other languages it returns a " +
			"positional window starting at the definition line, with a caveat. If the " +
			"name is ambiguous it renders the best match and lists the other candidate " +
			"locations so you can re-query. On ERROR (symbol not found, server " +
			"unavailable) fall back to grep or find_definition.",
		Parameters: json.RawMessage(codeNavParams),
	}
}

func (t *readSymbolTool) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	var args codeNavArgs
	if err := unmarshalArgs(raw, &args); err != nil {
		return "", err
	}
	if err := requireField("file", args.File); err != nil {
		return "", err
	}
	if err := requireLineNumber(args.Line); err != nil {
		return "", err
	}
	if err := requireField("symbol", args.Symbol); err != nil {
		return "", err
	}

	abs, err := t.nav.root.resolve(args.File)
	if err != nil {
		return "", err
	}

	// Exact path: the file's language has a tree-sitter grammar, so resolve the
	// declaration's full body span directly by name (proximity-ranked, same as
	// the navigator's fallback tier) and render it.
	if t.nav.body != nil && t.nav.body.Supports(abs) {
		return t.runExact(abs, args)
	}
	// Fallback path: no grammar — locate the definition semantically via the
	// tiered navigator and return a bounded positional window with a caveat.
	return t.runWindow(ctx, abs, args)
}

// runExact handles tree-sitter-supported files: it pulls the declaration's full
// node range and renders those lines, then lists any other same-named
// candidates so the model can re-query an ambiguous name.
//
// read_symbol intentionally bypasses LSP for tree-sitter-supported languages
// (syntactic exactness + no server dependency), unlike find_definition which
// is LSP-first.
func (t *readSymbolTool) runExact(abs string, args codeNavArgs) (string, error) {
	res, err := t.nav.body.DefinitionBodies(abs, strings.TrimSpace(strings.TrimSuffix(args.Symbol, "()")))
	if err != nil {
		return "", err
	}
	// Walk candidates best-first, rendering the first one inside the repo. An
	// out-of-root candidate (symlink escape, dependency) is skipped, matching
	// the find_definition renderer's containment contract.
	best, rel, ok := t.firstInRoot(res.Locations)
	if !ok {
		return readSymbolNotFound(args), nil
	}

	startLine := best.Range.Start.Line + 1
	requestedEnd := best.Range.End.Line + 1
	body, actualEnd, err := renderBodyRange(t.repoPath(best), startLine, requestedEnd)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	// Use actualEnd (the clamped, possibly truncated end) so the header span
	// matches the lines that renderBodyRange actually emitted.
	fmt.Fprintf(&b, "%s:%d-%d (%s)\n", rel, startLine, actualEnd, args.Symbol)
	b.WriteString(body)

	if len(res.Locations) > 1 {
		writeOtherCandidates(&b, t.nav, res.Locations, best)
	}
	return b.String(), nil
}

// runWindow handles non-tree-sitter files: it resolves the definition location
// through the tiered navigator and renders a bounded line window starting at the
// definition line, clearly captioned as a positional window (not an exact body).
func (t *readSymbolTool) runWindow(ctx context.Context, abs string, args codeNavArgs) (string, error) {
	lineText, err := readLine(abs, args.Line)
	if err != nil {
		return "", err
	}
	byteCol, err := symbolColumn(lineText, args.Symbol)
	if err != nil {
		return "", fmt.Errorf("%s:%d: %w (line is: %s)", args.File, args.Line, err, strings.TrimSpace(lineText))
	}
	pos := lsp.Position{Line: args.Line - 1, Character: lsp.UTF16Col(lineText, byteCol)}

	res, err := t.nav.nav.Definition(ctx, abs, pos)
	if err != nil {
		return "", err
	}
	best, rel, ok := t.firstInRoot(res.Locations)
	if !ok {
		return readSymbolNotFound(args), nil
	}

	startLine := best.Range.Start.Line + 1
	endLine := startLine + readSymbolWindowLines - 1

	var b strings.Builder
	b.WriteString("(positional window — this file's language is not tree-sitter-supported, " +
		"so the exact declaration body is unavailable; showing the definition line plus the " +
		"following lines. Use read_file with offset to see more.)\n")
	if res.Caveat != "" {
		fmt.Fprintf(&b, "%s\n", res.Caveat)
	}
	fmt.Fprintf(&b, "%s:%d (definition of %s)\n", rel, startLine, args.Symbol)
	body, _, err := renderBodyRange(t.repoPath(best), startLine, endLine)
	if err != nil {
		return "", err
	}
	b.WriteString(body)
	return b.String(), nil
}

// firstInRoot returns the first location inside the repository root, with its
// repo-relative path. Out-of-root locations (dependencies, symlink escapes) are
// skipped, never excerpted — the same sandbox contract find_definition enforces.
func (t *readSymbolTool) firstInRoot(locs []lsp.Location) (lsp.Location, string, bool) {
	for _, loc := range locs {
		path, ok := lsp.PathFromURI(loc.URI)
		if !ok {
			continue
		}
		if rel, inside := t.nav.relPath(path); inside {
			return loc, rel, true
		}
	}
	return lsp.Location{}, "", false
}

// repoPath extracts the filesystem path from a location's URI (the caller has
// already confirmed it is in-root via firstInRoot).
func (t *readSymbolTool) repoPath(loc lsp.Location) string {
	path, _ := lsp.PathFromURI(loc.URI)
	return path
}

// writeOtherCandidates lists same-named candidate locations other than best, so
// the model can re-query when a name is ambiguous. The list is bounded.
func writeOtherCandidates(b *strings.Builder, nav *CodeNav, locs []lsp.Location, best lsp.Location) {
	type cand struct {
		rel  string
		line int
	}
	var others []cand
	for _, loc := range locs {
		if loc == best {
			continue
		}
		path, ok := lsp.PathFromURI(loc.URI)
		if !ok {
			continue
		}
		rel, inside := nav.relPath(path)
		if !inside {
			continue
		}
		others = append(others, cand{rel: rel, line: loc.Range.Start.Line + 1})
		if len(others) >= readSymbolMaxCandidates {
			break
		}
	}
	if len(others) == 0 {
		return
	}
	fmt.Fprintf(b, "\nOther candidates with the same name (re-query read_symbol with one of these files):\n")
	for _, c := range others {
		fmt.Fprintf(b, "  %s:%d\n", c.rel, c.line)
	}
}

// renderBodyRange reads abs and renders its lines in the inclusive 1-based range
// [startLine, endLine] as numbered lines, applying the read_symbol line/byte
// caps. The numbering mirrors read_file (line number, tab, source), so the model
// sees consistent, absolute line numbers it can hand to read_file.
//
// It returns the rendered text, the actual last line number emitted (which may
// be less than endLine after file-length clamping or line/byte cap truncation),
// and any error.
func renderBodyRange(abs string, startLine, endLine int) (string, int, error) {
	info, err := os.Stat(abs)
	if err != nil {
		return "", 0, fmt.Errorf("cannot read file: %w", err)
	}
	if info.Size() > navMaxFileBytes {
		return "", 0, fmt.Errorf("file is too large for read_symbol (%d bytes)", info.Size())
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", 0, fmt.Errorf("cannot read file: %w", err)
	}
	lines := strings.Split(string(data), "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	total := len(lines)

	if startLine < 1 {
		startLine = 1
	}
	if endLine > total {
		endLine = total
	}
	if startLine > total {
		return "(definition line is past the end of the file)\n", startLine, nil
	}

	// Apply the line cap up front so the printed line-number width matches the
	// lines actually shown.
	lineTruncated := false
	if endLine-startLine+1 > readSymbolMaxLines {
		endLine = startLine + readSymbolMaxLines - 1
		lineTruncated = true
	}

	var b strings.Builder
	width := len(strconv.Itoa(endLine))
	byteBudget := readSymbolMaxBytes
	byteTruncated := false
	last := startLine - 1
	for i := startLine; i <= endLine; i++ {
		line := strings.TrimRight(lines[i-1], "\r")
		// +1 for the trailing newline each rendered line carries.
		cost := len(line) + 1
		// First line: never emit unbounded. Slice the line to the remaining
		// byte budget, walking back to a valid UTF-8 rune boundary so we
		// never split a multi-byte character. If even the smallest slice
		// would exceed the budget, emit nothing for the body but still
		// signal truncation with the marker.
		if i == startLine && cost > byteBudget {
			emit := byteBudget
			if emit > len(line) {
				emit = len(line)
			}
			for emit > 0 && emit < len(line) && !utf8.RuneStart(line[emit]) {
				emit--
			}
			if emit > 0 {
				fmt.Fprintf(&b, "%*d\t%s\n", width, i, line[:emit])
				last = i
			}
			byteTruncated = true
			fmt.Fprint(&b, "… [line truncated]\n")
			break
		}
		if byteBudget-cost < 0 && i > startLine {
			byteTruncated = true
			break
		}
		byteBudget -= cost
		fmt.Fprintf(&b, "%*d\t%s\n", width, i, line)
		last = i
	}
	if lineTruncated || byteTruncated {
		fmt.Fprintf(&b, "… [truncated at line %d — call read_file with offset=%d to continue]\n", last, last+1)
	}
	return b.String(), last, nil
}

// readSymbolNotFound is the ERROR result when no in-root definition was located,
// telling the model how to recover.
func readSymbolNotFound(args codeNavArgs) string {
	return fmt.Sprintf("ERROR: no definition found for %q in the repository — "+
		"check the symbol name, or use grep / find_definition to locate it.\n", args.Symbol)
}
