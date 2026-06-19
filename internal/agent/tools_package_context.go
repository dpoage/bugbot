package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/dpoage/bugbot/internal/llm"
)

// packageContextMaxBytes bounds the summary returned to the model. Summaries
// are <=120 words by construction (cartographySystemPrompt contract), but we
// cap defensively to avoid surprising growth if the store has older rows.
const packageContextMaxBytes = 4096

// packageGraphMaxEntries bounds the list length returned for each direction
// in package_graph results. The importer graph is typically sparse for
// well-factored repos; a cap guards against degenerate cases (e.g. a util
// package imported by every package in the repo).
const packageGraphMaxEntries = 200

// PackageContextTool implements get_package_context: a pull-based tool that
// fetches the cached cartographer summary for an arbitrary package. Token-cheap
// alternative to reading N files when a finder has stepped into an unfamiliar
// package during deep traversal.
//
// FINDER-ONLY: cartographer summaries are LLM-generated text that can carry
// framing or mild bias from the summarisation model. Exposing this to refuter
// agents would let cartographer output influence independent verification —
// exactly the contamination path that kills false-positive suppression. See the
// comment in hypothesize.go where refuter tools are assembled.
//
// The onLookup callback is injected by the funnel (wired to
// store.GetPackageSummaries). This keeps internal/agent free of store imports
// and makes the tool unit-testable with a simple fake — the same seam as
// PostLeadTool.onPost and SandboxExecTool.onExec.
type PackageContextTool struct {
	onLookup func(pkg string) (summary string, found bool, err error)
}

// NewPackageContextTool constructs a get_package_context tool. onLookup is
// called with the resolved package directory; it must return the cached summary
// and a found flag, or an error. The funnel passes a closure over
// store.GetPackageSummaries. A nil cart ("feature off") means the funnel passes
// a no-op that always returns found=false, never panicking.
func NewPackageContextTool(onLookup func(pkg string) (summary string, found bool, err error)) *PackageContextTool {
	return &PackageContextTool{onLookup: onLookup}
}

type packageContextArgs struct {
	Pkg string `json:"pkg"`
}

// Def implements agent.Tool.
func (t *PackageContextTool) Def() llm.ToolDef {
	return llm.ToolDef{
		Name: "get_package_context",
		Description: "Return the cached cartographer summary for a package (a <=120-word natural-language" +
			" description of the package's purpose, key invariants, and assumptions about callers)." +
			" Use this when you step into a package outside the seed context to orient quickly" +
			" without reading N files. pkg may be a repo-relative package directory (e.g." +
			" \"internal/store\") or any file path inside the package (the directory is derived" +
			" automatically). Returns a deterministic miss message when no summary is cached" +
			" yet — fall back to reading the source in that case.",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "pkg": {
      "type": "string",
      "description": "Repo-relative package directory or any file path inside the package (e.g. \"internal/store\" or \"internal/store/cartographer.go\")."
    }
  },
  "required": ["pkg"],
  "additionalProperties": false
}`),
	}
}

// Run implements agent.Tool. Arg errors are returned as tool errors (recoverable).
func (t *PackageContextTool) Run(_ context.Context, raw json.RawMessage) (string, error) {
	var args packageContextArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	pkg := strings.TrimSpace(args.Pkg)
	if pkg == "" {
		return "", fmt.Errorf("pkg must be non-empty")
	}

	// Derive directory: if the argument looks like a file (has an extension or
	// a recognisable file suffix), take its Dir — matching the packagesSpanned
	// rule used to build the cache keys.
	dir := pkg
	if ext := path.Ext(pkg); ext != "" {
		dir = path.Dir(pkg)
	}

	// packagesSpanned skips "." (repo-root files). Mirror that rejection so
	// the model gets a clear error rather than a confusing miss.
	if dir == "." || dir == "" {
		return "", fmt.Errorf("pkg resolves to the repo root; supply a package directory such as \"internal/store\"")
	}

	summary, found, err := t.onLookup(dir)
	if err != nil {
		return "", fmt.Errorf("get_package_context %s: %w", dir, err)
	}
	if !found {
		return fmt.Sprintf("no cached summary for %s", dir), nil
	}

	// Cap defensively.
	if len(summary) > packageContextMaxBytes {
		summary = summary[:packageContextMaxBytes]
	}
	return summary, nil
}

// PackageGraphTool implements package_graph: returns the importers and/or
// imports of a package from the importer graph already built during the
// cartography pass. Lets a finder reason about blast radius or call-path
// context mid-traversal without re-reading source files.
//
// package_graph is code-derived (no model opinion), so it is ELIGIBLE for
// refuter agents. v1 wires it finder-only to keep a single assembly site in
// hypothesize.go; refuter exposure is a trivial follow-up.
//
// The onQuery callback is injected by the funnel (backed by the cartography
// struct's QueryGraph method). This keeps internal/agent free of funnel-graph
// imports.
type PackageGraphTool struct {
	onQuery func(pkg, direction string) (importers, imports []string, err error)
}

// NewPackageGraphTool constructs a package_graph tool. onQuery is called with
// the resolved package directory and a direction string ("importers", "imports",
// or "both"); it returns the sorted, bounded lists (the implementation may
// return nil slices for an unknown package — that becomes an empty result, not
// an error).
func NewPackageGraphTool(onQuery func(pkg, direction string) (importers, imports []string, err error)) *PackageGraphTool {
	return &PackageGraphTool{onQuery: onQuery}
}

type packageGraphArgs struct {
	Pkg       string `json:"pkg"`
	Direction string `json:"direction,omitempty"`
}

// Def implements agent.Tool.
func (t *PackageGraphTool) Def() llm.ToolDef {
	return llm.ToolDef{
		Name: "package_graph",
		Description: "Return importers and/or imports of a package from the dependency graph" +
			" built during the cartography pass. Use this to reason about blast radius" +
			" (who imports this package?) or call paths (what does it import?) without" +
			" reading source files. pkg may be a repo-relative package directory or a" +
			" file path inside the package. An unknown package returns an empty result," +
			" not an error.",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "pkg": {
      "type": "string",
      "description": "Repo-relative package directory or any file path inside the package."
    },
    "direction": {
      "type": "string",
      "enum": ["importers", "imports", "both"],
      "description": "Which edges to return: \"importers\" (who imports this pkg), \"imports\" (what this pkg imports), or \"both\" (default)."
    }
  },
  "required": ["pkg"],
  "additionalProperties": false
}`),
	}
}

// Run implements agent.Tool. Arg errors are returned as tool errors (recoverable).
func (t *PackageGraphTool) Run(_ context.Context, raw json.RawMessage) (string, error) {
	var args packageGraphArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	pkg := strings.TrimSpace(args.Pkg)
	if pkg == "" {
		return "", fmt.Errorf("pkg must be non-empty")
	}

	// Derive directory (mirrors packagesSpanned rule).
	dir := pkg
	if ext := path.Ext(pkg); ext != "" {
		dir = path.Dir(pkg)
	}
	if dir == "." || dir == "" {
		return "", fmt.Errorf("pkg resolves to the repo root; supply a package directory such as \"internal/store\"")
	}

	// Default direction.
	direction := args.Direction
	if direction == "" {
		direction = "both"
	}
	switch direction {
	case "importers", "imports", "both":
	default:
		return "", fmt.Errorf("direction must be \"importers\", \"imports\", or \"both\", got %q", direction)
	}

	importerList, importList, err := t.onQuery(dir, direction)
	if err != nil {
		return "", fmt.Errorf("package_graph %s: %w", dir, err)
	}

	// Build deterministic, bounded output.
	var b strings.Builder
	b.WriteString("package: ")
	b.WriteString(dir)
	b.WriteByte('\n')

	writeList := func(label string, pkgs []string) {
		b.WriteString(label)
		b.WriteString(":\n")
		if len(pkgs) == 0 {
			b.WriteString("  (none)\n")
			return
		}
		sorted := make([]string, len(pkgs))
		copy(sorted, pkgs)
		sort.Strings(sorted)
		cap := len(sorted)
		truncated := false
		if cap > packageGraphMaxEntries {
			cap = packageGraphMaxEntries
			truncated = true
		}
		for _, p := range sorted[:cap] {
			b.WriteString("  ")
			b.WriteString(p)
			b.WriteByte('\n')
		}
		if truncated {
			fmt.Fprintf(&b, "  [truncated: %d more]\n", len(sorted)-packageGraphMaxEntries)
		}
	}

	if direction == "importers" || direction == "both" {
		writeList("importers (packages that import this package)", importerList)
	}
	if direction == "imports" || direction == "both" {
		writeList("imports (packages this package imports)", importList)
	}

	return b.String(), nil
}
