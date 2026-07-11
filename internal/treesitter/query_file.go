package treesitter

import (
	"strings"

	gts "github.com/odvcencio/gotreesitter"
	tsregistry "github.com/odvcencio/gotreesitter/grammars"
)

// QueryMatch is an alias for the gotreesitter QueryMatch type, re-exported so
// callers in sibling packages (e.g. miner) import only internal/treesitter.
type QueryMatch = gts.QueryMatch

// QueryCapture is an alias for the gotreesitter QueryCapture type.
type QueryCapture = gts.QueryCapture

// QueryFile parses src with the grammar registered for the lowercase file
// extension ext (e.g. ".ts", ".tsx") and runs queryStr against the resulting
// syntax tree, returning all QueryMatch values.
//
// It is the seam the miner package uses to run custom structural queries
// against individual snapshot files without triggering a full-repository walk.
// queryStr is compiled on every call; callers that hot-loop over many files
// may cache the compiled *gts.Query and call QueryFileParsed instead.
//
// Returns (nil, nil) when no grammar is registered for ext. Returns (nil, err)
// on query-compile or parse errors. An empty result set returns (nil, nil).
func QueryFile(src []byte, ext string, queryStr string) ([]QueryMatch, error) {
	ext = strings.ToLower(ext)
	g := grammarForExt(ext)
	if g == nil {
		return nil, nil
	}
	entry := tsregistry.DetectLanguage(g.sample)
	if entry == nil {
		return nil, &unsupportedError{lang: g.name}
	}
	lang := entry.Language()
	q, err := gts.NewQuery(queryStr, lang)
	if err != nil {
		return nil, err
	}
	parser := gts.NewParser(lang)
	tree, err := parser.Parse(src)
	if err != nil {
		return nil, err
	}
	if tree == nil || tree.RootNode() == nil {
		return nil, nil
	}
	defer tree.Release()
	matches := q.Execute(tree)
	if len(matches) == 0 {
		return nil, nil
	}
	return matches, nil
}
