package treesitter

import (
	"path/filepath"
	"sort"
	"strings"

	gts "github.com/odvcencio/gotreesitter"
	tsregistry "github.com/odvcencio/gotreesitter/grammars"
)

// rankByProximity orders matches so the most likely intended definition comes
// first: the same file as the query, then files sharing the longest directory
// prefix with the query file, then everything else. Ties break on path then
// position for deterministic output. This is the syntactic tier's substitute
// for the semantic resolution an LSP server would do.
func rankByProximity(matches []tag, queryPath string) {
	queryDir := filepath.Dir(queryPath)
	queryParts := splitPath(queryDir)

	sort.SliceStable(matches, func(i, j int) bool {
		mi, mj := matches[i], matches[j]
		si := mi.path == queryPath
		sj := mj.path == queryPath
		if si != sj {
			return si // same-file first
		}
		pi := commonPrefixLen(queryParts, splitPath(filepath.Dir(mi.path)))
		pj := commonPrefixLen(queryParts, splitPath(filepath.Dir(mj.path)))
		if pi != pj {
			return pi > pj // deeper shared prefix wins
		}
		if mi.path != mj.path {
			return mi.path < mj.path
		}
		if mi.NameRange.StartPoint.Row != mj.NameRange.StartPoint.Row {
			return mi.NameRange.StartPoint.Row < mj.NameRange.StartPoint.Row
		}
		return mi.NameRange.StartPoint.Column < mj.NameRange.StartPoint.Column
	})
}

// splitPath splits a cleaned directory path into its components.
func splitPath(dir string) []string {
	dir = filepath.Clean(dir)
	if dir == "." || dir == string(filepath.Separator) {
		return nil
	}
	return strings.Split(dir, string(filepath.Separator))
}

// commonPrefixLen counts the leading path components a and b share.
func commonPrefixLen(a, b []string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

// newTagger compiles a tagger for the grammar's language and query, loading the
// grammar from the registry via a representative filename.
func newTagger(g *grammar, query string) (*gts.Tagger, error) {
	entry := tsregistry.DetectLanguage(g.sample)
	if entry == nil {
		return nil, &unsupportedError{lang: g.name}
	}
	return gts.NewTagger(entry.Language(), query)
}

// extsForGrammar returns the set of extensions registered for a grammar, so the
// file walk parses every file of the language, not just the one the query
// started from.
func extsForGrammar(g *grammar) map[string]bool {
	out := map[string]bool{}
	for ext, gg := range grammarTable {
		if gg == g {
			out[ext] = true
		}
	}
	return out
}

// unsupportedError marks a language whose grammar could not be loaded from the
// registry — surfaced so callers degrade with a clear message.
type unsupportedError struct{ lang string }

func (e *unsupportedError) Error() string {
	return "tree-sitter: no grammar available for " + e.lang
}
