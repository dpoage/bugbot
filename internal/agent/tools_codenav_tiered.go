package agent

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/dpoage/bugbot/internal/lsp"
	"github.com/dpoage/bugbot/internal/treesitter"
)

// symbolAt recovers the identifier token at an LSP position in a file. The
// tools convert the model's symbol name to a position landing inside the
// identifier, so the tree-sitter tier — which matches by name, not position —
// reads the name back out from that position. It expands from the byte offset
// in both directions over identifier runes.
func symbolAt(absPath string, pos lsp.Position) (string, error) {
	line, err := readLine(absPath, pos.Line+1)
	if err != nil {
		return "", err
	}
	byteOff := lsp.ByteCol(line, pos.Character)
	if byteOff > len(line) {
		byteOff = len(line)
	}
	start := byteOff
	for start > 0 {
		r, size := utf8.DecodeLastRuneInString(line[:start])
		if !isIdentRune(r) {
			break
		}
		start -= size
	}
	end := byteOff
	for end < len(line) {
		r, size := utf8.DecodeRuneInString(line[end:])
		if !isIdentRune(r) {
			break
		}
		end += size
	}
	if start == end {
		return "", fmt.Errorf("no identifier at position")
	}
	return line[start:end], nil
}

// lspNavigator adapts *lsp.Manager (whose query methods return bare locations)
// to the navigator interface (which carries a tier caveat). The LSP tier is
// authoritative, so its caveat is always empty.
type lspNavigator struct {
	mgr *lsp.Manager
}

func (l *lspNavigator) Definition(ctx context.Context, path string, pos lsp.Position) (navResult, error) {
	locs, err := l.mgr.Definition(ctx, path, pos)
	return navResult{Locations: locs}, err
}

func (l *lspNavigator) References(ctx context.Context, path string, pos lsp.Position) (navResult, error) {
	locs, err := l.mgr.References(ctx, path, pos)
	return navResult{Locations: locs}, err
}

func (l *lspNavigator) Implementation(ctx context.Context, path string, pos lsp.Position) (navResult, error) {
	locs, err := l.mgr.Implementation(ctx, path, pos)
	return navResult{Locations: locs}, err
}

func (l *lspNavigator) Close() error { return l.mgr.Close() }

// tsBackend is the slice of the tree-sitter backend the tiered navigator
// consumes; an interface so the fallback path can be unit-tested without real
// grammars.
type tsBackend interface {
	Definition(absPath, symbol string) (treesitter.Result, error)
	References(absPath, symbol string) (treesitter.Result, error)
	Supports(path string) bool
	Close() error
}

// tieredNavigator selects the backend that answers a navigation query behind
// the same tool names. It prefers the authoritative LSP tier and falls back to
// the syntactic tree-sitter tier only when the LSP failure means a server can
// never answer (binary missing, crashed for the run, language unconfigured, or
// the method unsupported). It deliberately does NOT fall back on a per-query
// timeout: that means the server is likely still indexing and will answer on a
// later call, so the existing "still indexing; fall back to grep" message is
// surfaced as-is.
//
// The tree-sitter tier needs the symbol name, which the tools resolve from the
// query line before calling the navigator; the navigator recovers it from the
// file+position rather than threading a new parameter through the interface,
// keeping the LSP and fake paths unchanged.
type tieredNavigator struct {
	lsp  navigator
	ts   tsBackend
	root string
}

func newTieredNavigator(lspNav navigator, ts tsBackend, root string) *tieredNavigator {
	return &tieredNavigator{lsp: lspNav, ts: ts, root: root}
}

func (n *tieredNavigator) Close() error {
	err := n.lsp.Close()
	if cerr := n.ts.Close(); cerr != nil && err == nil {
		err = cerr
	}
	return err
}

func (n *tieredNavigator) Definition(ctx context.Context, path string, pos lsp.Position) (navResult, error) {
	res, err := n.lsp.Definition(ctx, path, pos)
	if !shouldFallBack(err) {
		return res, err
	}
	return n.tsDefinition(path, pos, err)
}

func (n *tieredNavigator) References(ctx context.Context, path string, pos lsp.Position) (navResult, error) {
	res, err := n.lsp.References(ctx, path, pos)
	if !shouldFallBack(err) {
		return res, err
	}
	return n.tsReferences(path, pos, err)
}

// Implementation has no syntactic equivalent: finding which concrete types
// satisfy an interface is a cross-file semantic question tree-sitter cannot
// answer. When the LSP tier cannot serve it, we surface the LSP degradation
// error unchanged so the model falls back to grep.
func (n *tieredNavigator) Implementation(ctx context.Context, path string, pos lsp.Position) (navResult, error) {
	return n.lsp.Implementation(ctx, path, pos)
}

func (n *tieredNavigator) tsDefinition(path string, pos lsp.Position, lspErr error) (navResult, error) {
	if !n.ts.Supports(path) {
		return navResult{}, degradeErr(path, lspErr)
	}
	symbol, err := symbolAt(path, pos)
	if err != nil {
		return navResult{}, lspErr
	}
	res, err := n.ts.Definition(path, symbol)
	if err != nil {
		return navResult{}, degradeErr(path, lspErr)
	}
	return navResult{Locations: res.Locations, Caveat: defCaveat(res)}, nil
}

func (n *tieredNavigator) tsReferences(path string, pos lsp.Position, lspErr error) (navResult, error) {
	if !n.ts.Supports(path) {
		return navResult{}, degradeErr(path, lspErr)
	}
	symbol, err := symbolAt(path, pos)
	if err != nil {
		return navResult{}, lspErr
	}
	res, err := n.ts.References(path, symbol)
	if err != nil {
		return navResult{}, degradeErr(path, lspErr)
	}
	return navResult{Locations: res.Locations, Caveat: refCaveat(res)}, nil
}

// defCaveat names the tier and warns that ambiguous names yield ranked
// candidates rather than the one true definition.
func defCaveat(res treesitter.Result) string {
	if res.Ambiguous {
		return fmt.Sprintf("(syntactic match — %d candidates ranked by proximity; no language "+
			"server available, so same-named symbols are not disambiguated)", res.Candidates)
	}
	return "(syntactic match — no language server available; resolved by tree-sitter)"
}

// refCaveat names the tier for references and notes the syntactic limit.
func refCaveat(res treesitter.Result) string {
	return fmt.Sprintf("(syntactic match — %d reference(s) from tree-sitter; no language server "+
		"available, so matches are by name and exclude comment/string mentions but not "+
		"cross-package shadowing)", res.Candidates)
}

// degradeErr reports that neither tier could answer, telling the model to fall
// back to grep and preserving the underlying LSP reason.
func degradeErr(path string, lspErr error) error {
	if lspErr != nil {
		return fmt.Errorf("no language server and no tree-sitter grammar for %q — fall back to grep (server: %v)", path, lspErr)
	}
	return fmt.Errorf("no syntactic match available for %q — fall back to grep", path)
}

// shouldFallBack reports whether an LSP error means the server can never answer
// this class of query, so the syntactic tier should try. Indexing timeouts are
// excluded: the server may answer on a later call, and a transient timeout must
// not silently downgrade every subsequent query to the weaker tier.
func shouldFallBack(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// A per-query timeout means the server is likely still indexing and may
	// answer later: keep the LSP tier rather than permanently downgrading.
	if strings.Contains(msg, "timed out") || strings.Contains(msg, "indexing") {
		return false
	}
	for _, marker := range []string{
		"not installed",
		"not found in PATH",
		"no language server is configured",
		"crashed repeatedly",
		"does not support",
		"manager is closed",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}
