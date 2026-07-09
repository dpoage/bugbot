package funnel

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"

	"github.com/dpoage/bugbot/internal/treesitter"
)

// LocusResolver maps a (file, line) to the stable location anchor used by the
// finding fingerprint (domain.Fingerprint): the name of the enclosing top-level
// declaration when tree-sitter can resolve one, else a content anchor hashed
// from the implicated line's normalized text, else (only when the file/line
// cannot even be read) the line number itself.
//
// Anchoring identity to the enclosing symbol makes a finding's fingerprint
// survive line drift (edits above the bug) and title rewording across scans —
// the drift that kept re-discovered bugs from deduping and produced duplicate
// published issues. Package-level code, unsupported languages, and parse
// failures all degrade past the symbol anchor; the content anchor keeps THAT
// case drift-stable too (bugbot-ezmx.5), since it identifies the line by what
// it says, not where it currently sits. A nil resolver, an empty root, a
// missing file, or an out-of-range line degrade further to "L:<line>" — a
// last resort with no drift protection, reachable only when there is no
// source text left to anchor to at all.
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
	if lr != nil {
		if anchor, ok := lr.contentAnchor(file, line); ok {
			return anchor
		}
	}
	return "L:" + itoa(line)
}

// LegacyLocus returns the pre-bugbot-ezmx.5 fallback locus for line: the bare
// "L:<line>" anchor every non-symbol locus used to resolve to. It exists
// purely for dual-lookup against identity minted before this resolver grew
// the content anchor (store.IsSuppressed's legacy path) — callers hash it the
// same way a freshly resolved locus is hashed and check both, so a
// suppression recorded against the old line-anchored scheme keeps matching
// until the row is naturally rewritten (e.g. by a rename). It is never used
// to mint new identity.
func (lr *LocusResolver) LegacyLocus(line int) string {
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

// contentAnchor computes a drift-stable fallback anchor for (file, line) from
// the implicated line's own text, so package-level code, unsupported
// languages, and parse failures stop re-minting identity on every edit above
// the line the way the bare line number did. ok is false when there is no
// text to anchor to: the file cannot be read, line is out of range, or the
// normalized line is empty (a blank or whitespace-only line carries no
// distinguishing content — hashing "" would collide every blank line in every
// file, which is worse than the line-number fallback it would replace).
//
// Anchor shape: "C:<12-hex-char sha256 of the normalized line>#<ordinal>".
// ordinal is the 1-based count of prior lines (top of file through line,
// inclusive) whose normalized text equals this one's. It exists to break
// anchor collisions between distinct defects that happen to sit on
// identical lines (e.g. two unrelated `return err` statements at
// package level) — without it they would fold into one fingerprint.
// Because it only counts EQUAL-content lines, editing unrelated lines above
// the implicated line never changes it, which is what keeps the anchor
// stable under ordinary drift; only a change that alters how many prior
// lines share this line's exact normalized text — inserting, deleting, or
// editing a neighbor INTO or OUT OF identical content — can shift the
// ordinal. That is an accepted, documented edge of the tie-break: it only
// bites when the file already contains duplicate lines, and only for a
// duplicate whose relative order changes (see locus_test.go).
func (lr *LocusResolver) contentAnchor(file string, line int) (string, bool) {
	if lr.root == "" || line < 1 {
		return "", false
	}
	data, err := os.ReadFile(filepath.Join(lr.root, file))
	if err != nil {
		return "", false
	}
	lines := strings.Split(string(data), "\n")
	if line > len(lines) {
		return "", false
	}
	norm := normalizeLocusLine(lines[line-1])
	if norm == "" {
		return "", false
	}
	ordinal := 0
	for i := 0; i < line; i++ {
		if normalizeLocusLine(lines[i]) == norm {
			ordinal++
		}
	}
	sum := sha256.Sum256([]byte(norm))
	return "C:" + hex.EncodeToString(sum[:])[:12] + "#" + itoa(ordinal), true
}

// normalizeLocusLine canonicalizes a source line for content-anchor hashing:
// trim leading/trailing whitespace and collapse internal whitespace runs to a
// single space, so pure reformatting (retabbing, trailing-whitespace cleanup)
// does not mint a new identity for an otherwise-unchanged line.
// strings.Fields already splits on any whitespace run and drops empties, so
// joining its result with single spaces performs both trims in one pass.
func normalizeLocusLine(line string) string {
	return strings.Join(strings.Fields(line), " ")
}
