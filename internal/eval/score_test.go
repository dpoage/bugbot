package eval

import (
	"testing"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/funnel"
)

// finding is a tiny constructor for a persisted finding in scoring tests.
func finding(file string, line int, lens, title string) domain.Finding {
	return domain.Finding{File: file, Line: line, Lens: lens, Title: title}
}

func TestScore_Match_WithinTolerance(t *testing.T) {
	c := Case{
		Name:   "tol",
		Seeded: []SeededBug{{File: "a.go", Line: 10, LineTolerance: 2, Kind: "nil"}},
	}
	res := &funnel.Result{Findings: []domain.Finding{
		finding("a.go", 12, "nil-safety/error-handling", "bug"), // |12-10| = 2 <= 2: match
	}}
	cr := score(c, res)
	if cr.TruePositives != 1 || cr.FalsePositives != 0 || cr.FalseNegatives != 0 {
		t.Fatalf("tp/fp/fn = %d/%d/%d, want 1/0/0", cr.TruePositives, cr.FalsePositives, cr.FalseNegatives)
	}
	if cr.PerLensTP["nil-safety/error-handling"] != 1 {
		t.Errorf("per-lens TP not recorded: %v", cr.PerLensTP)
	}
}

func TestScore_Miss_OutsideTolerance(t *testing.T) {
	c := Case{
		Name:   "tol",
		Seeded: []SeededBug{{File: "a.go", Line: 10, LineTolerance: 1}},
	}
	res := &funnel.Result{Findings: []domain.Finding{
		finding("a.go", 12, "lens", "bug"), // |12-10| = 2 > 1: no match
	}}
	cr := score(c, res)
	// The finding matches no seeded bug => FP; the seeded bug is unmatched => FN.
	if cr.TruePositives != 0 || cr.FalsePositives != 1 || cr.FalseNegatives != 1 {
		t.Fatalf("tp/fp/fn = %d/%d/%d, want 0/1/1", cr.TruePositives, cr.FalsePositives, cr.FalseNegatives)
	}
}

func TestScore_ZeroTolerance_ExactLine(t *testing.T) {
	c := Case{Name: "exact", Seeded: []SeededBug{{File: "a.go", Line: 10, LineTolerance: 0}}}
	exact := score(c, &funnel.Result{Findings: []domain.Finding{finding("a.go", 10, "l", "b")}})
	if exact.TruePositives != 1 {
		t.Errorf("exact line should match with zero tolerance: tp=%d", exact.TruePositives)
	}
	off := score(c, &funnel.Result{Findings: []domain.Finding{finding("a.go", 11, "l", "b")}})
	if off.TruePositives != 0 || off.FalsePositives != 1 {
		t.Errorf("off-by-one with zero tolerance must miss: tp=%d fp=%d", off.TruePositives, off.FalsePositives)
	}
}

func TestScore_WrongFile_NoMatch(t *testing.T) {
	c := Case{Name: "file", Seeded: []SeededBug{{File: "a.go", Line: 10, LineTolerance: 5}}}
	cr := score(c, &funnel.Result{Findings: []domain.Finding{finding("b.go", 10, "l", "b")}})
	if cr.TruePositives != 0 || cr.FalsePositives != 1 || cr.FalseNegatives != 1 {
		t.Errorf("different file must not match: tp/fp/fn = %d/%d/%d", cr.TruePositives, cr.FalsePositives, cr.FalseNegatives)
	}
}

func TestScore_FilePathNormalization(t *testing.T) {
	c := Case{Name: "norm", Seeded: []SeededBug{{File: "dir/a.go", Line: 10, LineTolerance: 0}}}
	// Differently-spelled but equivalent path (./ prefix) must still match.
	cr := score(c, &funnel.Result{Findings: []domain.Finding{finding("./dir/a.go", 10, "l", "b")}})
	if cr.TruePositives != 1 {
		t.Errorf("normalized path should match: tp=%d", cr.TruePositives)
	}
}

func TestScore_DuplicateFindings_DoNotInflateTP(t *testing.T) {
	c := Case{Name: "dup", Seeded: []SeededBug{{File: "a.go", Line: 10, LineTolerance: 3}}}
	// Two findings near the same seeded bug: only one can match it; the other is
	// a false positive.
	res := &funnel.Result{Findings: []domain.Finding{
		finding("a.go", 10, "l", "b1"),
		finding("a.go", 11, "l", "b2"),
	}}
	cr := score(c, res)
	if cr.TruePositives != 1 {
		t.Errorf("one seeded bug must yield at most one TP: tp=%d", cr.TruePositives)
	}
	if cr.FalsePositives != 1 {
		t.Errorf("the extra finding must count as FP: fp=%d", cr.FalsePositives)
	}
}

func TestScore_GreedyClosestLine(t *testing.T) {
	// Two seeded bugs and two findings; closest-line assignment must pair them up
	// 1:1 rather than letting one finding claim both.
	c := Case{Name: "greedy", Seeded: []SeededBug{
		{File: "a.go", Line: 10, LineTolerance: 5},
		{File: "a.go", Line: 20, LineTolerance: 5},
	}}
	res := &funnel.Result{Findings: []domain.Finding{
		finding("a.go", 11, "l", "near10"),
		finding("a.go", 21, "l", "near20"),
	}}
	cr := score(c, res)
	if cr.TruePositives != 2 || cr.FalsePositives != 0 || cr.FalseNegatives != 0 {
		t.Fatalf("tp/fp/fn = %d/%d/%d, want 2/0/0", cr.TruePositives, cr.FalsePositives, cr.FalseNegatives)
	}
}

func TestScore_CleanCase_AnyFindingIsFP(t *testing.T) {
	c := Case{Name: "clean", Seeded: nil}
	cr := score(c, &funnel.Result{Findings: []domain.Finding{
		finding("a.go", 1, "lens-x", "spurious"),
	}})
	if !cr.Clean() {
		t.Errorf("case with no seeded bugs must be Clean")
	}
	if cr.FalsePositives != 1 {
		t.Errorf("clean-case finding must be FP: fp=%d", cr.FalsePositives)
	}
	if cr.PerLensFP["lens-x"] != 1 {
		t.Errorf("per-lens FP not recorded: %v", cr.PerLensFP)
	}
	// A clean case that reports something has precision 0 (TP=0, FP>0).
	if cr.Precision() != 0 {
		t.Errorf("precision with TP=0 FP=1 should be 0, got %v", cr.Precision())
	}
}

func TestScore_CleanCase_NoFindings_VacuouslyPerfect(t *testing.T) {
	c := Case{Name: "clean", Seeded: nil}
	cr := score(c, &funnel.Result{Findings: nil})
	if cr.FalsePositives != 0 {
		t.Errorf("clean case with no findings has 0 FP")
	}
	if cr.Precision() != 1.0 || cr.Recall() != 1.0 {
		t.Errorf("empty clean case is vacuously perfect: p=%v r=%v", cr.Precision(), cr.Recall())
	}
}

func TestPrecisionRecall_Formulas(t *testing.T) {
	cr := &CaseResult{TruePositives: 3, FalsePositives: 1, FalseNegatives: 2}
	if got := cr.Precision(); got != 0.75 {
		t.Errorf("precision = %v, want 0.75", got)
	}
	if got := cr.Recall(); got != 0.6 {
		t.Errorf("recall = %v, want 0.6", got)
	}
}
