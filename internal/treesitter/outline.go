package treesitter

import (
	"path/filepath"
	"sort"
	"strings"
)

// OutlineEntry is one top-level declaration in a file: its name, kind (e.g.
// "definition.function"), and the 1-based start/end line of the full node body.
type OutlineEntry struct {
	Name      string
	Kind      string
	StartLine int // 1-based, inclusive
	EndLine   int // 1-based, inclusive
}

// Outline returns all top-level definitions in the single file at absPath,
// ordered by line number. It uses the per-file tag cache and respects the same
// mtime+size invalidation as Definition/References. An unsupported extension
// returns (nil, nil); a real parse error returns (nil, err).
func (b *Backend) Outline(absPath string) ([]OutlineEntry, error) {
	g := grammarForExt(strings.ToLower(filepath.Ext(absPath)))
	if g == nil {
		return nil, nil
	}
	lt, err := b.taggerFor(g, queryDef)
	if err != nil {
		return nil, err
	}

	rawTags := b.tagFile(g, queryDef, lt, absPath)
	// Sort by start row so callers get a top-to-bottom listing.
	sort.Slice(rawTags, func(i, j int) bool {
		ri := rawTags[i].Range.StartPoint.Row
		rj := rawTags[j].Range.StartPoint.Row
		if ri != rj {
			return ri < rj
		}
		return rawTags[i].Range.StartPoint.Column < rawTags[j].Range.StartPoint.Column
	})

	out := make([]OutlineEntry, 0, len(rawTags))
	for _, t := range rawTags {
		// Extend start row backward over decorator lines (mirrors toBodyLocations).
		startRow := decoratorAdjustedStart(absPath, t.Range.StartPoint.Row)
		out = append(out, OutlineEntry{
			Name:      t.Name,
			Kind:      t.Kind,
			StartLine: int(startRow) + 1,
			EndLine:   int(t.Range.EndPoint.Row) + 1,
		})
	}
	return out, nil
}
