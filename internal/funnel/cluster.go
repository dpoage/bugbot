package funnel

import (
	"path"
	"strings"
)

// DefaultMergeWindow is the line-proximity window, in lines, for the
// location-based cross-lens dedup in triage (mergeClusters). Two candidates in
// the same file are eligible to be the SAME underlying defect only when their
// lines are within this many lines of each other.
//
// Proximity is necessary but not sufficient: the same file can hold several
// distinct defects within ten lines of one another (the eval's multi-bug
// fixture seeds two real bugs nine lines apart; a real resource-leak fixture had
// a negative-length panic two lines from a file-descriptor leak). Merging those
// would silently drop a real finding. So mergeClusters pairs the window with a
// content-similarity guard (see mergeSimilarityThreshold): candidates only
// collapse when they are BOTH near each other AND describe the same defect. The
// clustering KEY ignores lens and title entirely — lens is exactly what we are
// deduping across, and title wording varies; the similarity guard reads the
// finder's free-text description, which is the richest signal for "is this the
// same bug" without reintroducing the lens/title coupling.
const DefaultMergeWindow = 10

// mergeSimilarityThreshold is the minimum Jaccard token overlap between two
// candidates' descriptions for them to be considered the same defect during the
// location-based merge. It is the "distinct-defect protection": two findings at
// nearby lines whose descriptions barely overlap (different bugs that happen to
// be close) stay in separate clusters, while genuine cross-lens duplicates of
// one defect — which describe the same code path in similar words — exceed it.
//
// Empirically (the recorded real-model corpus and the seeded fixtures) genuine
// duplicates of one defect score ~0.24–0.54 while distinct nearby defects score
// <=0.13, so 0.18 separates them with margin. It is deliberately conservative:
// when in doubt the pair is NOT merged, which can only cost dedup (an extra
// refuter panel), never a lost finding.
const mergeSimilarityThreshold = 0.18

// normPath normalizes a file path the same way store.Fingerprint does, so two
// spellings of the same file cluster together.
func normPath(file string) string {
	return strings.ToLower(path.Clean(strings.ReplaceAll(file, "\\", "/")))
}

// descTokens splits a description into a set of lowercased word tokens (length
// > 2, to drop noise words like "is"/"of"/"a"). It backs the similarity guard.
func descTokens(desc string) map[string]bool {
	out := make(map[string]bool)
	isWord := func(r rune) bool {
		return (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
	}
	for _, w := range strings.FieldsFunc(strings.ToLower(desc), func(r rune) bool {
		return !isWord(r)
	}) {
		if len(w) > 2 {
			out[w] = true
		}
	}
	return out
}

// jaccard returns the Jaccard similarity (intersection over union) of two token
// sets. Empty on either side yields 0 — an empty description carries no signal,
// so it never similarity-merges with anything (it can still be a singleton
// cluster, and a same-line exact-fingerprint duplicate was already removed in
// triage's earlier pass).
func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for w := range a {
		if b[w] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// indexedCand pairs a candidate with its original position in the triage
// survivor list (for stable primary tie-breaks and order restoration) and its
// precomputed description token set (for the similarity guard).
type indexedCand struct {
	c   Candidate
	pos int
	tok map[string]bool
}

// clusterAccepts reports whether candidate it belongs in cluster cl: it must be
// within window lines of, AND description-similar to, at least one existing
// member. Both conditions against the same member are not required — proximity
// to one member and similarity to another still binds, since both relations are
// about "same defect" and members of a real cluster satisfy them mutually.
func clusterAccepts(cl []indexedCand, it indexedCand, window int) bool {
	near := false
	for _, m := range cl {
		if abs(m.c.Line-it.c.Line) <= window {
			near = true
			break
		}
	}
	if !near {
		return false
	}
	for _, m := range cl {
		if jaccard(m.tok, it.tok) >= mergeSimilarityThreshold {
			return true
		}
	}
	return false
}

// abs returns the absolute value of n.
func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// SimilarFinding reports whether two findings describe the same defect under the
// cross-scan publish dedup rule: identical normalized file, lines within
// DefaultMergeWindow, and description-token Jaccard at or above the in-scan
// similarity threshold. It reuses the same machinery as the in-scan
// location-based merge so both layers agree on "same bug". The publish planner
// uses it to adopt an existing open issue for a re-discovered finding whose
// fingerprint drifted (e.g. a symbol rename, or the one-time v1->v2 scheme
// change) instead of filing a duplicate.
func SimilarFinding(fileA string, lineA int, descA, fileB string, lineB int, descB string) bool {
	if normPath(fileA) != normPath(fileB) {
		return false
	}
	if abs(lineA-lineB) > DefaultMergeWindow {
		return false
	}
	return jaccard(descTokens(descA), descTokens(descB)) >= mergeSimilarityThreshold
}

// sameRootCauseThreshold is the minimum Jaccard overlap for the broad same-file
// and cross-file decl/def root-cause merge. Higher than mergeSimilarityThreshold
// (0.18) to guard the wider merge window.
const sameRootCauseThreshold = 0.35

// sameRootCauseMinSharedTokens is the minimum number of description tokens that
// must appear in BOTH candidates for a same-root-cause merge. This prevents
// short descriptions whose Jaccard ratio is high due to small vocabulary from
// binding across genuinely distinct defect patterns (e.g. "index buffer length
// overflow" vs "index buffer length underflow" at lines 40/900 score jaccard
// 0.6 on 3 shared tokens, but the distinct overflow/underflow words are exactly
// the signal). Together with the Jaccard threshold, both conditions must hold.
const sameRootCauseMinSharedTokens = 5

// sharedTokenCount returns the number of tokens common to both sets.
func sharedTokenCount(a, b map[string]bool) int {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	n := 0
	for w := range a {
		if b[w] {
			n++
		}
	}
	return n
}

// sameFileSameRootCause reports whether it should merge into the same-file
// cluster cl due to matching defect pattern, ignoring line distance. Both
// candidates must be in the same file; the caller enforces that precondition.
//
// Both conditions must hold for ANY cluster member:
//  1. Jaccard >= sameRootCauseThreshold (strong overlap ratio)
//  2. At least sameRootCauseMinSharedTokens tokens in common (prevents
//     high-ratio-but-tiny-vocabulary false positives)
func sameFileSameRootCause(cl []indexedCand, it indexedCand) bool {
	for _, m := range cl {
		if jaccard(m.tok, it.tok) >= sameRootCauseThreshold &&
			sharedTokenCount(m.tok, it.tok) >= sameRootCauseMinSharedTokens {
			return true
		}
	}
	return false
}

// sourceExtensions maps source file extensions to their header/declaration
// counterparts. The mapping is symmetric: each extension listed as a key can
// be matched against the extensions in its value, and vice versa.
var sourceExtensions = map[string][]string{
	".cpp": {".hpp", ".h"},
	".cc":  {".hpp", ".h", ".hh"},
	".cxx": {".hpp", ".h", ".hh"},
	".c":   {".h"},
	".hpp": {".cpp", ".cc", ".cxx"},
	".hh":  {".cc", ".cxx"},
	".h":   {".cpp", ".cc", ".cxx", ".c"},
}

// fileStem returns the base filename without extension, lowercased.
func fileStem(file string) string {
	base := path.Base(normPath(file))
	ext := path.Ext(base)
	if ext == "" {
		return base
	}
	return base[:len(base)-len(ext)]
}

// fileExt returns the lowercased extension including the leading dot.
func fileExt(file string) string {
	return path.Ext(normPath(file))
}

// fileDir returns the lowercased directory component of a path.
func fileDir(file string) string {
	return path.Dir(normPath(file))
}

// isSrcHdrPair reports whether files a and b form a source/header declaration-
// definition pair. Both the DIRECTORY, the stem, and the extension pairing must
// match. The directory guard prevents render/utils.cpp from pairing with
// audio/utils.hpp — a ubiquitous false-positive shape in C++ projects.
func isSrcHdrPair(a, b string) bool {
	if fileDir(a) != fileDir(b) {
		return false // different directories: cannot be the same translation unit
	}
	if fileStem(a) != fileStem(b) {
		return false
	}
	extA := fileExt(a)
	extB := fileExt(b)
	if extA == extB {
		return false // same extension, not a pair
	}
	for _, mate := range sourceExtensions[extA] {
		if mate == extB {
			return true
		}
	}
	return false
}

// crossFileDeclDefSameRootCause reports whether it (from a different file than
// the cluster primary) is the same root cause via decl/def pairing. Conditions:
//  1. Same directory + same stem + complementary extension (isSrcHdrPair).
//  2. Strongly description-similar (Jaccard >= sameRootCauseThreshold AND at
//     least sameRootCauseMinSharedTokens tokens in common).
//
// Both conditions are required; either failing means no merge.
func crossFileDeclDefSameRootCause(cl []indexedCand, it indexedCand) bool {
	if len(cl) == 0 {
		return false
	}
	primary := cl[0]
	if !isSrcHdrPair(primary.c.File, it.c.File) {
		return false
	}
	// Check description similarity against any member.
	for _, m := range cl {
		if jaccard(m.tok, it.tok) >= sameRootCauseThreshold &&
			sharedTokenCount(m.tok, it.tok) >= sameRootCauseMinSharedTokens {
			return true
		}
	}
	return false
}
