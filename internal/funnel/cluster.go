package funnel

import (
	"path"
	"sort"
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

// confidenceRank maps a confidence string to a sort key (lower = more
// confident). Unknown values rank last so a malformed confidence never wins a
// primary tie-break over a well-formed one.
func confidenceRank(s string) int {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "high":
		return 0
	case "medium":
		return 1
	case "low":
		return 2
	default:
		return 3
	}
}

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

// mergeClusters performs the location-based cross-lens dedup. It runs AFTER the
// per-candidate triage filters (low-confidence, scope, exact-fingerprint dedup,
// suppression) and BEFORE verification. Candidates that point at the same
// underlying defect — same file, nearby line, similar description — collapse to
// a single primary that proceeds to verification; the other members' lenses are
// recorded on the primary as corroboration and counted in the stats as
// within-lens or cross-lens relative to the primary's lens.
//
// # Clustering rule (proximity + similarity, transitive)
//
// Within a file, candidates are sorted by line. A candidate joins an existing
// cluster when it is within window lines of ANY current member AND its
// description is similar (Jaccard >= mergeSimilarityThreshold) to ANY current
// member; otherwise it starts a new cluster. Membership is therefore transitive
// through chains of pairwise-similar, pairwise-near members — but the similarity
// guard keeps a chain from drifting onto an unrelated defect, so a cluster stays
// anchored to one bug. This is the "distinct-defect protection": two real bugs
// that sit within window lines of each other but describe different defects
// never merge (verified by the eval's multi-bug fixture).
//
// # Primary selection
//
// The primary is the most severe member; ties break by highest confidence, then
// by the longest (most specific) description, then by stable original order so
// the result is deterministic. The primary keeps its OWN fingerprint, so
// suppression memory — which keys off store.Fingerprint(lens,file,line,title) —
// is unaffected.
//
// window <= 0 disables merging (every candidate is its own cluster); callers
// pass DefaultMergeWindow.
func mergeClusters(candidates []Candidate, window int, stats *Stats) []Candidate {
	if len(candidates) == 0 || window <= 0 {
		return candidates
	}

	// Group by normalized file path, preserving first-seen file order so the
	// returned survivor list is deterministic and independent of map iteration.
	fileOrder := make([]string, 0)
	byFile := make(map[string][]indexedCand)
	for i, c := range candidates {
		key := normPath(c.File)
		if _, ok := byFile[key]; !ok {
			fileOrder = append(fileOrder, key)
		}
		byFile[key] = append(byFile[key], indexedCand{c: c, pos: i, tok: descTokens(c.Description)})
	}

	primaries := make([]indexedCand, 0, len(candidates))

	for _, key := range fileOrder {
		group := byFile[key]
		// Sort by line, then by original position, so clustering is deterministic
		// when several candidates share a line.
		sort.SliceStable(group, func(i, j int) bool {
			if group[i].c.Line != group[j].c.Line {
				return group[i].c.Line < group[j].c.Line
			}
			return group[i].pos < group[j].pos
		})

		var clusters [][]indexedCand
		for _, it := range group {
			joined := false
			for ci := range clusters {
				if clusterAccepts(clusters[ci], it, window) {
					clusters[ci] = append(clusters[ci], it)
					joined = true
					break
				}
			}
			if !joined {
				clusters = append(clusters, []indexedCand{it})
			}
		}
		for _, cl := range clusters {
			primaries = append(primaries, finalizeCluster(cl, stats))
		}
	}

	// Restore original (first-seen) order so downstream stages see a stable,
	// input-anchored ordering rather than per-file line order.
	sort.SliceStable(primaries, func(i, j int) bool { return primaries[i].pos < primaries[j].pos })
	out := make([]Candidate, len(primaries))
	for i, p := range primaries {
		out[i] = p.c
	}
	return out
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

// finalizeCluster selects the cluster's primary, records the other members'
// lenses on it as corroboration (deduped, sorted, excluding the primary's own
// lens), and counts each merged member as within- or cross-lens in stats. It
// returns the primary (carrying its original position for order restoration).
func finalizeCluster(cluster []indexedCand, stats *Stats) indexedCand {
	primary := pickPrimary(cluster)

	corrob := make(map[string]bool)
	for _, m := range cluster {
		if m.pos == primary.pos {
			continue
		}
		// Stats: a merged member is within-lens when its lens equals the primary's,
		// cross-lens otherwise.
		if strings.EqualFold(m.c.Lens, primary.c.Lens) {
			stats.MergedWithinLens++
		} else {
			stats.MergedCrossLens++
		}
		// Corroboration excludes the primary's own lens; a same-lens merge adds no
		// new corroborating lens but is still counted above.
		if !strings.EqualFold(m.c.Lens, primary.c.Lens) && m.c.Lens != "" {
			corrob[m.c.Lens] = true
		}
	}

	if len(corrob) > 0 {
		lenses := make([]string, 0, len(corrob))
		for l := range corrob {
			lenses = append(lenses, l)
		}
		sort.Strings(lenses)
		primary.c.CorroboratingLenses = lenses
	}
	return primary
}

// pickPrimary chooses the cluster's representative: most severe, then most
// confident, then longest/most-specific description, then stable original
// order. cluster is non-empty.
func pickPrimary(cluster []indexedCand) indexedCand {
	best := cluster[0]
	for _, it := range cluster[1:] {
		if primaryLess(best, it) {
			best = it
		}
	}
	return best
}

// primaryLess reports whether b is a STRICTLY better primary than a. The
// ordering is: lower severity rank (more severe) wins; then lower confidence
// rank (more confident); then longer description (more specific); then earlier
// original position (stable). Ties at every level fall through to "not better",
// so the first-seen candidate is retained.
func primaryLess(a, b indexedCand) bool {
	sa, sb := severityRank(a.c.Severity), severityRank(b.c.Severity)
	if sa != sb {
		return sb < sa
	}
	ca, cb := confidenceRank(a.c.Confidence), confidenceRank(b.c.Confidence)
	if ca != cb {
		return cb < ca
	}
	la, lb := len(strings.TrimSpace(a.c.Description)), len(strings.TrimSpace(b.c.Description))
	if la != lb {
		return lb > la
	}
	return b.pos < a.pos
}

// abs returns the absolute value of n.
func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
