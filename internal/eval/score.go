package eval

import (
	"sort"

	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/store"
)

// Match records that a persisted finding matched a seeded bug.
type Match struct {
	Seeded  SeededBug
	Finding store.Finding
}

// CaseResult is the scored outcome of one Case run.
type CaseResult struct {
	// Name echoes the case name.
	Name string
	// Seeded echoes the case's seeded bug set for downstream Clean() computation.
	Seeded []SeededBug

	// TruePositives are findings that matched a seeded bug (counted as one per
	// matched seeded bug; a single finding cannot satisfy two seeded bugs).
	TruePositives int
	// FalsePositives are persisted findings that matched no seeded bug. On a
	// clean-code case every finding is a false positive.
	FalsePositives int
	// FalseNegatives are seeded bugs no finding matched (a missed bug).
	FalseNegatives int

	// Matches lists each (seeded bug, finding) pair that matched, for reporting.
	Matches []Match
	// UnmatchedFindings are the findings counted as false positives.
	UnmatchedFindings []store.Finding
	// UnmatchedSeeded are the seeded bugs counted as false negatives.
	UnmatchedSeeded []SeededBug

	// PerLensTP / PerLensFP break the counts down by the lens that produced the
	// finding, so a report can show which lens is noisy or productive.
	PerLensTP map[string]int
	PerLensFP map[string]int

	// Stats is the funnel's per-stage accounting passed through verbatim, so a
	// report can show WHERE seeded bugs died (hypothesized → triaged → verified,
	// and the drop breakdown).
	Stats funnel.Stats
	// Findings is the full persisted finding set for the case (post-funnel).
	Findings []store.Finding
}

// Clean reports whether the case was a clean-code case (no seeded bugs).
// Any finding on a clean case is a false positive.
func (r *CaseResult) Clean() bool { return len(r.Seeded) == 0 }

// Precision is TP / (TP + FP). A case that reported nothing has no opinion to
// be wrong about, so precision is defined as 1.0 there (vacuously perfect) —
// this matters for clean cases, whose correct behavior is "report nothing".
func (r *CaseResult) Precision() float64 {
	denom := r.TruePositives + r.FalsePositives
	if denom == 0 {
		return 1.0
	}
	return float64(r.TruePositives) / float64(denom)
}

// Recall is TP / (TP + FN). A case with no seeded bugs has nothing to recall,
// so recall is defined as 1.0 there (vacuously perfect).
func (r *CaseResult) Recall() float64 {
	denom := r.TruePositives + r.FalseNegatives
	if denom == 0 {
		return 1.0
	}
	return float64(r.TruePositives) / float64(denom)
}

// score matches the funnel's persisted findings against the case's seeded bugs.
//
// Matching rule: a finding matches a seeded bug when they name the same file
// and the finding's line is within the seeded bug's LineTolerance of its Line.
// Lens/kind is intentionally NOT part of the match: a real bug found by an
// unexpected lens is still a true positive. Each seeded bug and each finding is
// consumed by at most one match (greedy by closest line), so duplicate findings
// at the same location do not inflate the TP count.
func score(c Case, res *funnel.Result) *CaseResult {
	out := &CaseResult{
		Name:      c.Name,
		Seeded:    c.Seeded,
		PerLensTP: map[string]int{},
		PerLensFP: map[string]int{},
		Stats:     res.Stats,
		Findings:  res.Findings,
	}

	findingMatched := make([]bool, len(res.Findings))

	// For each seeded bug, find the closest unmatched finding in the same file
	// within tolerance. Closest-line greedy keeps assignment stable and prevents
	// one finding from claiming multiple seeded bugs.
	for _, bug := range c.Seeded {
		best := -1
		bestDist := -1
		for i, f := range res.Findings {
			if findingMatched[i] {
				continue
			}
			if !sameFile(f.File, bug.File) {
				continue
			}
			d := abs(f.Line - bug.Line)
			if d > bug.LineTolerance {
				continue
			}
			if best == -1 || d < bestDist {
				best = i
				bestDist = d
			}
		}
		if best >= 0 {
			findingMatched[best] = true
			f := res.Findings[best]
			out.TruePositives++
			out.PerLensTP[f.Lens]++
			out.Matches = append(out.Matches, Match{Seeded: bug, Finding: f})
		} else {
			out.FalseNegatives++
			out.UnmatchedSeeded = append(out.UnmatchedSeeded, bug)
		}
	}

	// Every finding that matched nothing is a false positive. On a clean case
	// (no seeded bugs) this is simply every finding.
	for i, f := range res.Findings {
		if findingMatched[i] {
			continue
		}
		out.FalsePositives++
		out.PerLensFP[f.Lens]++
		out.UnmatchedFindings = append(out.UnmatchedFindings, f)
	}

	sort.Slice(out.UnmatchedFindings, func(i, j int) bool {
		if out.UnmatchedFindings[i].File != out.UnmatchedFindings[j].File {
			return out.UnmatchedFindings[i].File < out.UnmatchedFindings[j].File
		}
		return out.UnmatchedFindings[i].Line < out.UnmatchedFindings[j].Line
	})
	return out
}

// sameFile compares two repo-relative paths using the store's fingerprint
// normalization, so path-case and separator differences never cause a spurious
// miss. The funnel persists findings with the file path the finder reported, so
// this keeps matching robust to that path's exact spelling.
func sameFile(a, b string) bool {
	return store.Fingerprint("", a, 0, "") == store.Fingerprint("", b, 0, "")
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
