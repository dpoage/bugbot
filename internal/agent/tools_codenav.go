package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/lsp"
)

// Code-navigation limits.
const (
	// codeNavMaxResults caps the locations rendered for one query so a
	// pathological reference list cannot blow the model's context.
	codeNavMaxResults = 200
	// codeNavMaxFileBytes bounds files we load to locate a symbol or render a
	// snippet (matches the grep tool's ceiling for data/binary files).
	codeNavMaxFileBytes = 5 * 1024 * 1024
	// codeNavMaxLineBytes caps a rendered source line.
	codeNavMaxLineBytes = 256
)

// navigator is the slice of the LSP manager the code-nav tools consume. It is
// an interface so unit tests can script results without a real language
// server; *lsp.Manager is the production implementation.
type navigator interface {
	Definition(ctx context.Context, path string, pos lsp.Position) ([]lsp.Location, error)
	References(ctx context.Context, path string, pos lsp.Position) ([]lsp.Location, error)
	Implementation(ctx context.Context, path string, pos lsp.Position) ([]lsp.Location, error)
	Close() error
}

// CodeNav bundles the three LSP-backed navigation tools (find_definition,
// find_references, find_implementations) sharing one language-server manager
// rooted at a repository. Construct once per scan, hand Tools() to the agents,
// and Close() when the scan finishes to shut the servers down.
//
// The tools are safe for concurrent use by parallel agents. Language servers
// are spawned lazily on the first query for their language; when a server
// binary is not installed (or crashes repeatedly, or is still indexing past
// the query timeout) the tools degrade to a clear "ERROR:" result telling the
// model to fall back to grep — they never hang and never abort the run.
type CodeNav struct {
	root *fsRoot
	nav  navigator
}

// NewCodeNav creates the code-navigation tool bundle rooted at dir. No
// language-server processes are started until a tool issues its first query.
func NewCodeNav(dir string) (*CodeNav, error) {
	root, err := newFSRoot(dir)
	if err != nil {
		return nil, err
	}
	return &CodeNav{root: root, nav: lsp.NewManager(root.root)}, nil
}

// Tools returns the three navigation tools backed by this bundle.
func (c *CodeNav) Tools() []Tool {
	return []Tool{
		&codeNavTool{nav: c, kind: navDefinition},
		&codeNavTool{nav: c, kind: navReferences},
		&codeNavTool{nav: c, kind: navImplementations},
	}
}

// Close shuts down every language server the bundle started. Safe to call
// multiple times.
func (c *CodeNav) Close() error { return c.nav.Close() }

// navKind selects which LSP query a codeNavTool issues.
type navKind int

const (
	navDefinition navKind = iota
	navReferences
	navImplementations
)

// codeNavTool is one of the three navigation tools; they differ only in the
// LSP method and their prompt-facing description.
type codeNavTool struct {
	nav  *CodeNav
	kind navKind
}

// codeNavArgs is the shared argument shape: name + file + line, not an exact
// column. Models reliably produce "symbol on this line"; the tool locates the
// symbol's column itself (including the byte->UTF-16 conversion LSP requires).
type codeNavArgs struct {
	File   string `json:"file"`
	Line   int    `json:"line"`
	Symbol string `json:"symbol"`
}

// codeNavParams is the JSON schema shared by all three tools.
const codeNavParams = `{
  "type": "object",
  "properties": {
    "file": {
      "type": "string",
      "description": "Repository-relative path to the file containing the symbol occurrence (no leading slash, no ..)."
    },
    "line": {
      "type": "integer",
      "description": "1-based line number where the symbol appears in that file.",
      "minimum": 1
    },
    "symbol": {
      "type": "string",
      "description": "The symbol's name exactly as it appears on that line (e.g. 'Hello' or 'pkg.Hello'). The tool finds it on the line for you."
    }
  },
  "required": ["file", "line", "symbol"],
  "additionalProperties": false
}`

func (t *codeNavTool) Def() llm.ToolDef {
	var name, desc string
	switch t.kind {
	case navDefinition:
		name = "find_definition"
		desc = "Jump to the definition of a symbol (function, method, type, variable, " +
			"constant) using the project's language server. Point it at any occurrence: " +
			"give the repo-relative file, the 1-based line where the symbol appears " +
			"(e.g. a call site), and the symbol name as written on that line. Returns " +
			"the precise definition location(s) as path:line with the source line — " +
			"unlike grep, this resolves imports, shadowing, and same-named symbols " +
			"correctly. If it returns an ERROR (server not installed or still " +
			"indexing), fall back to grep."
	case navReferences:
		name = "find_references"
		desc = "List every reference to a symbol across the repository (excluding its " +
			"declaration) using the project's language server — the reliable way to " +
			"enumerate a function's callers or a field's readers/writers. Point it at " +
			"the symbol's definition or any usage: give the repo-relative file, the " +
			"1-based line, and the symbol name as written on that line. Use this " +
			"instead of grep to check how callers actually use a function (e.g. " +
			"whether every caller already nil-checks an argument): grep misses " +
			"qualified/aliased calls and matches unrelated same-named identifiers. If " +
			"it returns an ERROR (server not installed or still indexing), fall back " +
			"to grep."
	case navImplementations:
		name = "find_implementations"
		desc = "List the concrete implementations of an interface (or of one interface " +
			"method) using the project's language server. Point it at the interface or " +
			"method name: give the repo-relative file, the 1-based line where that name " +
			"appears, and the name itself. Returns each implementation's location as " +
			"path:line with the source line — use it to find what code can actually run " +
			"behind an interface-typed call. If it returns an ERROR (server not " +
			"installed or still indexing), fall back to grep."
	}
	return llm.ToolDef{
		Name:        name,
		Description: desc,
		Parameters:  json.RawMessage(codeNavParams),
	}
}

func (t *codeNavTool) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	var args codeNavArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if args.File == "" {
		return "", fmt.Errorf("file is required")
	}
	if args.Line < 1 {
		return "", fmt.Errorf("line must be a 1-based line number")
	}
	if strings.TrimSpace(args.Symbol) == "" {
		return "", fmt.Errorf("symbol is required")
	}

	abs, err := t.nav.root.resolve(args.File)
	if err != nil {
		return "", err
	}
	lineText, err := readLine(abs, args.Line)
	if err != nil {
		return "", err
	}

	byteCol, err := symbolColumn(lineText, args.Symbol)
	if err != nil {
		return "", fmt.Errorf("%s:%d: %w (line is: %s)", args.File, args.Line, err, strings.TrimSpace(lineText))
	}
	pos := lsp.Position{Line: args.Line - 1, Character: lsp.UTF16Col(lineText, byteCol)}

	var locs []lsp.Location
	switch t.kind {
	case navDefinition:
		locs, err = t.nav.nav.Definition(ctx, abs, pos)
	case navReferences:
		locs, err = t.nav.nav.References(ctx, abs, pos)
	case navImplementations:
		locs, err = t.nav.nav.Implementation(ctx, abs, pos)
	}
	if err != nil {
		return "", err
	}
	return t.render(locs), nil
}

// render formats locations as repo-relative "path:line: source" lines.
// Locations outside the repository root (stdlib, module cache) are reported
// but not excerpted — the sandbox does not read files outside the root.
func (t *codeNavTool) render(locs []lsp.Location) string {
	if len(locs) == 0 {
		switch t.kind {
		case navReferences:
			return "(no references found — the symbol may be unused, or only referenced via reflection/codegen)\n"
		case navImplementations:
			return "(no implementations found)\n"
		default:
			return "(no definition found)\n"
		}
	}

	shown := locs
	truncated := false
	if len(shown) > codeNavMaxResults {
		shown = shown[:codeNavMaxResults]
		truncated = true
	}

	var b strings.Builder
	seen := make(map[string]bool, len(shown))
	lineCache := make(map[string][]string)
	for _, loc := range shown {
		path, ok := lsp.PathFromURI(loc.URI)
		if !ok {
			fmt.Fprintf(&b, "%s (non-file location)\n", loc.URI)
			continue
		}
		line := loc.Range.Start.Line + 1
		key := fmt.Sprintf("%s:%d", path, line)
		if seen[key] {
			continue
		}
		seen[key] = true

		rel, inside := t.relPath(path)
		if !inside {
			fmt.Fprintf(&b, "%s:%d (outside repository — dependency or stdlib)\n", path, line)
			continue
		}
		fmt.Fprintf(&b, "%s:%d:%s\n", rel, line, sourceLine(lineCache, path, line))
	}
	if truncated {
		fmt.Fprintf(&b, "... [truncated at %d locations]\n", codeNavMaxResults)
	}
	return b.String()
}

// relPath maps an absolute result path to a repo-relative slash path,
// reporting whether it lies inside the root.
func (t *codeNavTool) relPath(path string) (string, bool) {
	if !t.nav.root.contains(filepath.Clean(path)) {
		return "", false
	}
	rel, err := filepath.Rel(t.nav.root.root, path)
	if err != nil {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// sourceLine returns the trimmed text of the 1-based line in path, using cache
// to avoid re-reading a file that appears many times in one result set. It is
// best-effort: unreadable files yield an empty snippet.
func sourceLine(cache map[string][]string, path string, line int) string {
	lines, ok := cache[path]
	if !ok {
		lines = readFileLines(path)
		cache[path] = lines
	}
	if line < 1 || line > len(lines) {
		return ""
	}
	s := strings.TrimRight(lines[line-1], "\r")
	if len(s) > codeNavMaxLineBytes {
		s = s[:codeNavMaxLineBytes] + "…"
	}
	return s
}

// readFileLines loads a file's lines, bounded by codeNavMaxFileBytes. Errors
// and oversized files yield nil (snippets are best-effort decoration).
func readFileLines(path string) []string {
	info, err := os.Stat(path)
	if err != nil || info.Size() > codeNavMaxFileBytes {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return strings.Split(string(data), "\n")
}

// readLine returns the 1-based line from the file at abs, with bounds and size
// checks that produce model-actionable errors.
func readLine(abs string, line int) (string, error) {
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("cannot read file: %w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("path is a directory, not a file")
	}
	if info.Size() > codeNavMaxFileBytes {
		return "", fmt.Errorf("file is too large for code navigation (%d bytes)", info.Size())
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", fmt.Errorf("cannot read file: %w", err)
	}
	lines := strings.Split(string(data), "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	if line > len(lines) {
		return "", fmt.Errorf("line %d is past the end of the file (%d lines)", line, len(lines))
	}
	return strings.TrimRight(lines[line-1], "\r"), nil
}

// symbolColumn locates symbol on lineText and returns the byte offset of the
// identifier the LSP query should target. A dotted symbol ("pkg.Hello",
// "recv.method") matches as written but the returned offset points at its
// final segment, since LSP positions must land inside a single identifier
// token. When the full symbol is absent, the final segment alone is tried so
// models that over-qualify still succeed.
func symbolColumn(lineText, symbol string) (int, error) {
	symbol = strings.TrimSpace(symbol)
	symbol = strings.TrimSuffix(symbol, "()")

	if off, ok := findIdentifier(lineText, symbol); ok {
		// Point inside the last identifier segment of a qualified name.
		if i := strings.LastIndexByte(symbol, '.'); i >= 0 {
			return off + i + 1, nil
		}
		return off, nil
	}
	if i := strings.LastIndexByte(symbol, '.'); i >= 0 {
		if off, ok := findIdentifier(lineText, symbol[i+1:]); ok {
			return off, nil
		}
	}
	return 0, fmt.Errorf("symbol %q not found on the line", symbol)
}

// findIdentifier finds the first occurrence of sym in lineText that is not
// embedded in a longer identifier (the characters immediately before and after
// must not be identifier characters).
func findIdentifier(lineText, sym string) (int, bool) {
	if sym == "" {
		return 0, false
	}
	for start := 0; start <= len(lineText)-len(sym); {
		i := strings.Index(lineText[start:], sym)
		if i < 0 {
			return 0, false
		}
		i += start
		if !identAdjacent(lineText, i, len(sym)) {
			return i, true
		}
		start = i + 1
	}
	return 0, false
}

// identAdjacent reports whether the match at [i, i+n) in s touches an
// identifier character on either side (meaning it is a substring of a longer
// identifier, not the symbol itself).
func identAdjacent(s string, i, n int) bool {
	if i > 0 {
		r, _ := utf8.DecodeLastRuneInString(s[:i])
		if isIdentRune(r) {
			return true
		}
	}
	if i+n < len(s) {
		r, _ := utf8.DecodeRuneInString(s[i+n:])
		if isIdentRune(r) {
			return true
		}
	}
	return false
}

// isIdentRune reports whether r can be part of an identifier in the languages
// we navigate (letters, digits, underscore; Unicode letters included for Go).
func isIdentRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}
