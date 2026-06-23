package funnel

import (
	"path/filepath"

	"github.com/dpoage/bugbot/internal/treesitter"
)

// LocusResolver maps a (file, line) to the stable location anchor used by the
// finding fingerprint (store.Fingerprint): the name of the enclosing top-level
// declaration when tree-sitter can resolve one, else a line-based fallback.
//
// Anchoring identity to the enclosing symbol makes a finding's fingerprint
// survive line drift (edits above the bug) and title rewording across scans —
// the drift that kept re-discovered bugs from deduping and produced duplicate
// published issues. A nil resolver, an empty root, an unsupported language, or a
// parse failure all degrade to the line fallback, so the resolver is always
// safe to call.
type LocusResolver struct {
	root    string
	backend *treesitter.Backend
}

// NewLocusResolver builds a resolver rooted at the repository's absolute path.
// An empty root yields a fallback-only resolver (used by tests and by snapshots
// taken without a Root).
func NewLocusResolver(root string) *LocusResolver {
	lr := &LocusResolver{root: root}
	if root != "" {
		lr.backend = treesitter.New(root)
	}
	return lr
}

// Resolve returns the stable location anchor for (file, line). file is the
// repo-relative, forward-slash path from the snapshot.
func (lr *LocusResolver) Resolve(file string, line int) string {
	if lr != nil && lr.backend != nil {
		if entry, ok := lr.enclosing(file, line); ok {
			// kind + name: top-level names are file-unique in practice, but a
			// method and a same-named type can coexist, so the kind disambiguates.
			return "S:" + string(entry.Kind) + "\x00" + entry.Name
		}
	}
	return "L:" + itoa(line)
}

// enclosing returns the narrowest top-level declaration whose 1-based line range
// contains line. ok is false when none does (package-level or blank lines, an
// unsupported language, or a parse error) — the caller falls back to the line.
func (lr *LocusResolver) enclosing(file string, line int) (treesitter.OutlineEntry, bool) {
	entries, err := lr.backend.Outline(filepath.Join(lr.root, file))
	if err != nil || len(entries) == 0 {
		return treesitter.OutlineEntry{}, false
	}
	var best treesitter.OutlineEntry
	found := false
	for _, e := range entries {
		if line < e.StartLine || line > e.EndLine {
			continue
		}
		if !found || (e.EndLine-e.StartLine) < (best.EndLine-best.StartLine) {
			best = e
			found = true
		}
	}
	return best, found
}
