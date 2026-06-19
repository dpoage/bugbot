package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/treesitter"
)

// outline limits.
const (
	// outlineMaxEntries caps the number of symbols returned for one file so a
	// large generated file cannot flood the model's context.
	outlineMaxEntries = 200
)

// outlineTool lists top-level declarations (functions, types, methods, classes)
// with their signatures and 1-based line ranges, backed by tree-sitter. No
// bodies are returned — the model gets structure, not content.
type outlineTool struct {
	nav *CodeNav
}

const outlineParams = `{
  "type": "object",
  "properties": {
    "file": {
      "type": "string",
      "description": "Repo-relative path to the file to outline."
    }
  },
  "required": ["file"]
}`

func (t *outlineTool) Def() llm.ToolDef {
	return llm.ToolDef{
		Name: "outline",
		Description: "List the top-level symbols (functions, methods, types, classes) " +
			"in a file with their signatures and line ranges — no bodies. Use this to " +
			"map a file's structure before deciding which symbol to read in full with " +
			"read_symbol. Supported languages: Go, Python, TypeScript, TSX. Returns an " +
			"ERROR for unsupported file types; fall back to grep in that case.",
		Parameters: json.RawMessage(outlineParams),
	}
}

type outlineArgs struct {
	File string `json:"file"`
}

func (t *outlineTool) Run(_ context.Context, raw json.RawMessage) (string, error) {
	var args outlineArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return toolError(fmt.Errorf("invalid arguments: %w", err)), nil
	}
	if strings.TrimSpace(args.File) == "" {
		return toolError(fmt.Errorf("file is required")), nil
	}

	abs, err := t.nav.root.resolve(args.File)
	if err != nil {
		return toolError(err), nil
	}

	if t.nav.outline == nil || !t.nav.outline.Supports(abs) {
		return toolError(fmt.Errorf("outline: unsupported file type for %s; use grep to search instead", args.File)), nil
	}

	entries, err := t.nav.outline.Outline(abs)
	if err != nil {
		return toolError(fmt.Errorf("outline: %w", err)), nil
	}
	// Sort by start line so the model sees symbols in top-to-bottom source order,
	// regardless of what order the backend returned them.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].StartLine < entries[j].StartLine
	})

	if len(entries) == 0 {
		return fmt.Sprintf("(no top-level symbols found in %s)\n", args.File), nil
	}

	truncated := false
	if len(entries) > outlineMaxEntries {
		entries = entries[:outlineMaxEntries]
		truncated = true
	}

	lines := fileLines(make(map[string][]string), abs)

	var b strings.Builder
	fmt.Fprintf(&b, "outline: %s\n", args.File)
	for _, e := range entries {
		// Render the kind as a short label: "definition.function" -> "func",
		// "definition.type" -> "type", etc.
		label := kindLabel(e.Kind)
		// Signature is the first line of the declaration (trimmed).
		sig := ""
		if e.StartLine >= 1 && e.StartLine <= len(lines) {
			sig = strings.TrimSpace(lines[e.StartLine-1])
			if len(sig) > codeNavMaxLineBytes {
				sig = sig[:codeNavMaxLineBytes] + "…"
			}
		}
		fmt.Fprintf(&b, "%4d-%4d  %-6s  %s  (%s)\n", e.StartLine, e.EndLine, label, e.Name, sig)
	}
	if truncated {
		fmt.Fprintf(&b, "... [truncated at %d symbols]\n", outlineMaxEntries)
	}
	return b.String(), nil
}

// kindLabel maps a tree-sitter definition kind string to a short readable label.
func kindLabel(kind string) string {
	// kind follows the pattern "definition.X" where X is the symbol kind.
	if after, ok := strings.CutPrefix(kind, "definition."); ok {
		switch after {
		case "function":
			return "func"
		case "method":
			return "method"
		case "type":
			return "type"
		case "class":
			return "class"
		case "interface":
			return "iface"
		case "var", "variable":
			return "var"
		case "const", "constant":
			return "const"
		case "module":
			return "module"
		default:
			return after
		}
	}
	return kind
}

// fakeOutlineBackend is a tsOutlineBackend implementation for testing.
// It is unexported but lives in the agent package so tests can use it.
type fakeOutlineBackend struct {
	entries  []treesitter.OutlineEntry
	err      error
	supports bool
}

func (f *fakeOutlineBackend) Outline(_ string) ([]treesitter.OutlineEntry, error) {
	return f.entries, f.err
}

func (f *fakeOutlineBackend) Supports(_ string) bool {
	return f.supports
}
