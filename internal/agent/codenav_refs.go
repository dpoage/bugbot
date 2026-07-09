package agent

import (
	"context"

	"github.com/dpoage/bugbot/internal/lsp"
)

// RefLocation is one repo-relative reference location: the programmatic
// (non-tool-call) result shape References returns, distinct from navResult
// (which carries raw LSP Locations for the JSON-tool rendering path the
// find_references tool exposes to models).
type RefLocation struct {
	// File is the repo-relative, forward-slash path of the reference.
	File string
	// Line is the 1-based line the reference occurs on.
	Line int
}

// References returns the repo-relative reference locations for the symbol sym
// declared/used at file:line (1-based), reusing the same LSP find_references
// query the find_references tool issues. This is the programmatic seam other
// packages call directly instead of going through the JSON tool-call path:
// funnel's code-nav root-cause fold (triage step 5e) uses it to ask "what
// references this symbol" without a model in the loop.
//
// Locations outside the repository root (stdlib, dependency source) are
// silently dropped — a caller comparing against in-repo loci has no use for
// them, mirroring how render() reports (but a programmatic caller has no text
// output to report them into).
func (c *CodeNav) References(ctx context.Context, file string, line int, sym string) ([]RefLocation, error) {
	abs, err := c.root.Resolve(file)
	if err != nil {
		return nil, err
	}
	lineText, err := readLine(abs, line)
	if err != nil {
		return nil, err
	}
	byteCol, err := symbolColumn(lineText, sym)
	if err != nil {
		return nil, err
	}
	pos := lsp.Position{Line: line - 1, Character: lsp.UTF16Col(lineText, byteCol)}

	res, err := c.nav.References(ctx, abs, pos)
	if err != nil {
		return nil, err
	}
	out := make([]RefLocation, 0, len(res.Locations))
	for _, loc := range res.Locations {
		p, ok := lsp.PathFromURI(loc.URI)
		if !ok {
			continue
		}
		rel, inside := c.relPath(p)
		if !inside {
			continue
		}
		out = append(out, RefLocation{File: rel, Line: loc.Range.Start.Line + 1})
	}
	return out, nil
}
