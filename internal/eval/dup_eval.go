package eval

import (
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/dpoage/bugbot/internal/funnel"
)

// DupPairResult is the scored outcome of one labeled DupPair.
type DupPairResult struct {
	Pair      DupPair
	Predicted bool // what funnel.SimilarFinding decided for this pair
}

// Correct reports whether the identity layer's decision matched ground truth.
func (r DupPairResult) Correct() bool { return r.Predicted == r.Pair.SameDefect }

// DupEvalResult aggregates the scored outcome of a labeled DupPair corpus
// against the current identity layer's cross-scan duplicate decision
// (funnel.SimilarFinding). It reports precision/recall of THAT decision
// treating "predicted same defect" as positive:
//
//   - TP: SameDefect=true,  Predicted=true  (a real duplicate the layer caught)
//   - FP: SameDefect=false, Predicted=true  (distinct defects wrongly merged)
//   - FN: SameDefect=true,  Predicted=false (a real duplicate the layer missed)
//   - TN: SameDefect=false, Predicted=false (distinct defects correctly kept apart)
//
// Precision = TP/(TP+FP) measures over-merging risk (the more costly failure:
// a wrongly-adopted duplicate silently drops a real finding). Recall =
// TP/(TP+FN) measures how much duplicate suppression the layer is actually
// buying. Both are 1.0 when their denominator is zero (vacuously perfect —
// mirrors CaseResult's convention in score.go).
type DupEvalResult struct {
	Cases     []DupPairResult
	ByChannel map[DupChannel]*channelTally
}

// channelTally is the TP/FP/FN/TN breakdown for one DupChannel.
type channelTally struct {
	TP, FP, FN, TN int
}

func (t *channelTally) add(r DupPairResult) {
	switch {
	case r.Pair.SameDefect && r.Predicted:
		t.TP++
	case !r.Pair.SameDefect && r.Predicted:
		t.FP++
	case r.Pair.SameDefect && !r.Predicted:
		t.FN++
	default:
		t.TN++
	}
}

// precision/recall share the CaseResult convention: 1.0 (vacuously perfect)
// when the denominator is zero.
func precision(tp, fp int) float64 {
	if tp+fp == 0 {
		return 1
	}
	return float64(tp) / float64(tp+fp)
}

func recall(tp, fn int) float64 {
	if tp+fn == 0 {
		return 1
	}
	return float64(tp) / float64(tp+fn)
}

// Precision returns the suite-wide TP/(TP+FP) across all pairs.
func (r *DupEvalResult) Precision() float64 {
	tp, fp := 0, 0
	for _, c := range r.Cases {
		if c.Pair.SameDefect && c.Predicted {
			tp++
		} else if !c.Pair.SameDefect && c.Predicted {
			fp++
		}
	}
	return precision(tp, fp)
}

// Recall returns the suite-wide TP/(TP+FN) across all pairs.
func (r *DupEvalResult) Recall() float64 {
	tp, fn := 0, 0
	for _, c := range r.Cases {
		if c.Pair.SameDefect && c.Predicted {
			tp++
		} else if c.Pair.SameDefect && !c.Predicted {
			fn++
		}
	}
	return recall(tp, fn)
}

// RunDupEval scores pairs against the current v2 identity layer's
// funnel.SimilarFinding decision (NOT a live LLM — pure offline evaluation of
// the same file/line/description-jaccard rule the funnel and publish planner
// use). It never errors: SimilarFinding is a deterministic pure function, so
// every pair produces a result.
func RunDupEval(pairs []DupPair) *DupEvalResult {
	res := &DupEvalResult{ByChannel: make(map[DupChannel]*channelTally)}
	for _, p := range pairs {
		predicted := funnel.SimilarFinding(p.A.File, p.A.Line, p.A.Desc, p.B.File, p.B.Line, p.B.Desc)
		cr := DupPairResult{Pair: p, Predicted: predicted}
		res.Cases = append(res.Cases, cr)

		t, ok := res.ByChannel[p.Channel]
		if !ok {
			t = &channelTally{}
			res.ByChannel[p.Channel] = t
		}
		t.add(cr)
	}
	return res
}

// String renders the dup-eval result as a per-channel precision/recall table
// plus an aggregate line and a list of every pair the identity layer scored
// incorrectly (for a human to inspect which ground-truth channel is weakest).
func (r *DupEvalResult) String() string {
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "channel\tpairs\ttp\tfp\tfn\ttn\tprecision\trecall")

	channels := make([]string, 0, len(r.ByChannel))
	for c := range r.ByChannel {
		channels = append(channels, string(c))
	}
	sort.Strings(channels)
	for _, cs := range channels {
		c := DupChannel(cs)
		t := r.ByChannel[c]
		n := t.TP + t.FP + t.FN + t.TN
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%.2f\t%.2f\n",
			c, n, t.TP, t.FP, t.FN, t.TN, precision(t.TP, t.FP), recall(t.TP, t.FN))
	}
	_ = tw.Flush()

	fmt.Fprintf(&b, "aggregate: %d pairs, precision=%.2f recall=%.2f\n",
		len(r.Cases), r.Precision(), r.Recall())

	var misses []string
	for _, c := range r.Cases {
		if !c.Correct() {
			want := "different"
			if c.Pair.SameDefect {
				want = "same"
			}
			got := "different"
			if c.Predicted {
				got = "same"
			}
			misses = append(misses, fmt.Sprintf("  [%s] %s: want=%s got=%s", c.Pair.Channel, c.Pair.Name, want, got))
		}
	}
	if len(misses) > 0 {
		b.WriteString("misses:\n" + strings.Join(misses, "\n") + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
