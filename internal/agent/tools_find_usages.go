package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/lsp"
)

// find_usages limits.
const (
	// findUsagesDefaultN is the default number of call-site windows returned
	// when the model omits the top_n argument.
	findUsagesDefaultN = 10
	// findUsagesMaxN caps top_n so the model cannot request an unbounded result.
	findUsagesMaxN = 50
	// findUsagesContextLines is the number of lines of surrounding context
	// rendered above and below each call site (half-window on each side).
	findUsagesContextLines = 3
)

// findUsagesTool returns the top-N call sites of a symbol with a few lines of
// surrounding context. It reuses the CodeNav bundle's LSP find_references
// machinery to locate references, then reads context windows from disk.
type findUsagesTool struct {
	nav *CodeNav
}

// findUsagesParams is the JSON schema for the find_usages tool.
const findUsagesParams = `{
  "type": "object",
  "properties": {
    "file": {
      "type": "string",
      "description": "Repo-relative path to a file where the symbol appears (definition or any call site)."
    },
    "line": {
      "type": "integer",
      "description": "1-based line number where the symbol name appears in file."
    },
    "symbol": {
      "type": "string",
      "description": "The symbol name as written on that line (e.g. \"Foo\", \"pkg.Bar\")."
    },
    "top_n": {
      "type": "integer",
      "description": "Maximum number of call-site windows to return (default 10, max 50)."
    }
  },
  "required": ["file", "line", "symbol"]
}`

func (t *findUsagesTool) Def() llm.ToolDef {
	return llm.ToolDef{
		Name: "find_usages",
		Description: "Return the top-N call sites of a symbol with " +
			"surrounding context lines, so you can see how callers actually invoke " +
			"it without reading every file. Point it at any occurrence of the symbol " +
			"(definition or call site): give the repo-relative file, the 1-based line, " +
			"and the symbol name as written on that line. Each result shows the call " +
			"site with a few context lines above and below. Capped at top_n (default " +
			"10, max 50). Falls back gracefully when the language server is unavailable.",
		Parameters: json.RawMessage(findUsagesParams),
	}
}

type findUsagesArgs struct {
	File   string `json:"file"`
	Line   int    `json:"line"`
	Symbol string `json:"symbol"`
	TopN   int    `json:"top_n"`
}

func (t *findUsagesTool) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	var args findUsagesArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return toolError(fmt.Errorf("invalid arguments: %w", err)), nil
	}
	if args.File == "" {
		return toolError(fmt.Errorf("file is required")), nil
	}
	if args.Line < 1 {
		return toolError(fmt.Errorf("line must be a 1-based line number")), nil
	}
	if strings.TrimSpace(args.Symbol) == "" {
		return toolError(fmt.Errorf("symbol is required")), nil
	}
	n := args.TopN
	if n <= 0 {
		n = findUsagesDefaultN
	}
	if n > findUsagesMaxN {
		n = findUsagesMaxN
	}

	abs, err := t.nav.root.resolve(args.File)
	if err != nil {
		return toolError(err), nil
	}
	lineText, err := readLine(abs, args.Line)
	if err != nil {
		return toolError(err), nil
	}
	byteCol, err := symbolColumn(lineText, args.Symbol)
	if err != nil {
		return toolError(fmt.Errorf("%s:%d: %w (line is: %s)", args.File, args.Line, err, strings.TrimSpace(lineText))), nil
	}
	pos := lsp.Position{Line: args.Line - 1, Character: lsp.UTF16Col(lineText, byteCol)}

	res, err := t.nav.nav.References(ctx, abs, pos)
	if err != nil {
		return toolError(err), nil
	}

	locs := res.Locations
	if len(locs) == 0 {
		return "(no usages found — the symbol may be unused, or only referenced via reflection/codegen)\n", nil
	}

	// Deduplicate by file:line (mirrors codeNavTool.render).
	seen := make(map[string]bool, len(locs))
	var unique []lsp.Location
	for _, loc := range locs {
		path, ok := lsp.PathFromURI(loc.URI)
		if !ok {
			continue
		}
		line := loc.Range.Start.Line + 1
		key := fmt.Sprintf("%s:%d", path, line)
		if seen[key] {
			continue
		}
		seen[key] = true
		unique = append(unique, loc)
	}

	shown := unique
	truncated := false
	if len(shown) > n {
		shown = shown[:n]
		truncated = true
	}

	var b strings.Builder
	if res.Caveat != "" {
		fmt.Fprintf(&b, "%s\n", res.Caveat)
	}
	lineCache := make(map[string][]string)
	for _, loc := range shown {
		path, ok := lsp.PathFromURI(loc.URI)
		if !ok {
			continue
		}
		callLine := loc.Range.Start.Line + 1
		rel, inside := t.nav.relPath(path)
		if !inside {
			fmt.Fprintf(&b, "%s:%d (outside repository — dependency or stdlib)\n", path, callLine)
			continue
		}

		lines := fileLines(lineCache, path)
		start := callLine - findUsagesContextLines
		if start < 1 {
			start = 1
		}
		end := callLine + findUsagesContextLines
		if len(lines) > 0 && end > len(lines) {
			end = len(lines)
		}

		fmt.Fprintf(&b, "--- %s:%d ---\n", rel, callLine)
		for i := start; i <= end; i++ {
			line := ""
			if i >= 1 && i <= len(lines) {
				line = lines[i-1]
				if len(line) > codeNavMaxLineBytes {
					line = line[:codeNavMaxLineBytes] + "…"
				}
			}
			marker := " "
			if i == callLine {
				marker = ">"
			}
			fmt.Fprintf(&b, "%s%4d\t%s\n", marker, i, line)
		}
	}
	if truncated {
		fmt.Fprintf(&b, "... [capped at %d usages; use find_references for the full list]\n", n)
	}
	return b.String(), nil
}

// fileLines returns the lines of path using cache to avoid re-reading files
// that appear in multiple results. It delegates to readFileLines.
func fileLines(cache map[string][]string, path string) []string {
	if ls, ok := cache[path]; ok {
		return ls
	}
	ls := readFileLines(path)
	cache[path] = ls
	return ls
}
