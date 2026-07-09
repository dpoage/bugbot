package eval

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/funnel"
)

// DupStage names which stage of the composed deterministic v3 identity
// decision (see scoreV3) produced a DupPairResult's Predicted verdict. It
// mirrors triage's own stage order: kind gate first, then exact fingerprint
// equality, then the SimilarFinding tiebreak — so a human (or a future
// threshold-tuning pass) reading a result can see WHERE the decision came
// from, not just what it was.
type DupStage string

const (
	// StageKindGate: funnel.SameOrUnknownKind rejected a non-empty
	// defect_kind mismatch — predicted distinct, no further stage consulted.
	StageKindGate DupStage = "kind-gate"
	// StageExactFP: domain.FingerprintV3 minted an identical fingerprint for
	// both candidates (same file, locus, kind, subject) — predicted
	// duplicate. This is the primary v3 identity path (triage_streaming.go
	// step 3): cross-lens duplicates converge here without ever consulting
	// description similarity.
	StageExactFP DupStage = "exact-fp"
	// StageSimilar: fingerprints differed, but funnel.SimilarFinding's
	// same-file window/jaccard rule matched — predicted duplicate. This is
	// the durable-fold/publish backstop (bugbot-ezmx.3/.4) that catches
	// drift the fingerprint doesn't survive (e.g. a variable rename that
	// also changed the description).
	StageSimilar DupStage = "similar"
	// StageNone: none of the above matched — predicted distinct.
	StageNone DupStage = "none"
)

// DupPairResult is the scored outcome of one labeled DupPair.
type DupPairResult struct {
	Pair      DupPair
	Predicted bool     // the composed v3 identity stack's decision (see scoreV3)
	Stage     DupStage // which stage of that stack decided it
}

// Correct reports whether the identity layer's decision matched ground truth.
func (r DupPairResult) Correct() bool { return r.Predicted == r.Pair.SameDefect }

// DupEvalResult aggregates the scored outcome of a labeled DupPair corpus
// against the COMPOSED deterministic v3 identity decision (see scoreV3):
// funnel.SameOrUnknownKind's kind gate, domain.FingerprintV3 exact equality
// (the primary triage-step-3 identity path), and funnel.SimilarFinding as
// the same-file window/jaccard tiebreaker durable fold/publish already use.
// This is the layer that actually dedups post-v3 (bugbot-ezmx.1) — NOT the
// bugbot-ezmx.2 LLM dedup arbiter, which stays out of this deterministic
// eval entirely. It reports precision/recall of the composed decision
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

// RunDupEval scores pairs against the COMPOSED deterministic v3 identity
// decision (scoreV3: kind gate -> exact FingerprintV3 equality ->
// SimilarFinding tiebreak — the exact stage order and real predicates
// triage_streaming.go step 3 and internal/funnel/reconcile.go use, with NO
// re-implementation of either's thresholds). It is NOT a live LLM call:
// every stage is a deterministic pure function, so every pair produces a
// result and this can never error.
//
// Locus resolution for FingerprintV3's exact-fp stage needs real source text
// (funnel.LocusResolver's symbol/content anchors read the file from disk),
// so RunDupEval materializes each pair's DupCandidate.Source into a scratch
// directory for the duration of the call; a failure to do so (e.g. no
// writable temp dir) degrades every candidate to the locus resolver's own
// "L:<line>" fallback rather than failing the eval.
func RunDupEval(pairs []DupPair) *DupEvalResult {
	root, cleanup := writeCorpusSources(pairs)
	defer cleanup()
	resolver := funnel.NewLocusResolver(root)

	res := &DupEvalResult{ByChannel: make(map[DupChannel]*channelTally)}
	for _, p := range pairs {
		predicted, stage := scoreV3(resolver, p.A, p.B)
		cr := DupPairResult{Pair: p, Predicted: predicted, Stage: stage}
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

// scoreV3 composes the real deterministic v3 identity decision for one pair
// of candidates, in the same order triage applies it:
//
//  1. funnel.SameOrUnknownKind (internal/funnel/cluster.go) — a non-empty
//     defect_kind mismatch is authoritative and short-circuits every merge
//     path (see domain.FingerprintV3's doc); checked first so a mismatch
//     never even reaches the fingerprint or jaccard stages, mirroring
//     internal/funnel/reconcile.go's own candidate nomination order.
//  2. domain.FingerprintV3 exact equality, with the locus argument resolved
//     by the real funnel.LocusResolver — the identical mechanism
//     triage_streaming.go step 3 uses. This is the primary v3 identity
//     path: it is what lets a cross-lens duplicate converge without ever
//     comparing description text.
//  3. funnel.SimilarFinding — the same-file window/jaccard tiebreaker
//     internal/funnel/reconcile.go's durable-fold candidate nomination and
//     the publish planner's durable adoption (bugbot-ezmx.3) already use
//     for drift the fingerprint doesn't survive.
//
// No stage's threshold is re-implemented here; all three are the funnel
// package's real exported (or, for the kind gate, newly-exported) functions.
func scoreV3(resolver *funnel.LocusResolver, a, b DupCandidate) (predicted bool, stage DupStage) {
	if !funnel.SameOrUnknownKind(a.DefectKind, b.DefectKind) {
		return false, StageKindGate
	}
	fpA := domain.FingerprintV3(a.File, resolver.Resolve(a.File, a.Line), a.DefectKind, a.Subject)
	fpB := domain.FingerprintV3(b.File, resolver.Resolve(b.File, b.Line), b.DefectKind, b.Subject)
	if fpA == fpB {
		return true, StageExactFP
	}
	if funnel.SimilarFinding(a.File, a.Line, a.Desc, b.File, b.Line, b.Desc) {
		return true, StageSimilar
	}
	return false, StageNone
}

// writeCorpusSources materializes every non-empty DupCandidate.Source in
// pairs into a fresh scratch directory (one write per distinct File, first
// writer wins) and returns its root plus a cleanup func. Candidates with no
// Source contribute nothing, so their locus resolves through
// funnel.LocusResolver's own fallback chain against a root that simply lacks
// that file. Best-effort: a filesystem error degrades to an empty root
// (funnel.NewLocusResolver("") is a documented fallback-only resolver, so
// this can never panic or error RunDupEval's caller).
func writeCorpusSources(pairs []DupPair) (root string, cleanup func()) {
	dir, err := os.MkdirTemp("", "bugbot-dupeval-corpus-*")
	if err != nil {
		return "", func() {}
	}
	written := make(map[string]bool)
	for _, p := range pairs {
		for _, c := range [2]DupCandidate{p.A, p.B} {
			if c.Source == "" || written[c.File] {
				continue
			}
			abs := filepath.Join(dir, filepath.FromSlash(c.File))
			if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
				continue
			}
			if err := os.WriteFile(abs, []byte(c.Source), 0o644); err == nil {
				written[c.File] = true
			}
		}
	}
	return dir, func() { _ = os.RemoveAll(dir) }
}

// String renders the dup-eval result as a per-channel precision/recall table
// plus an aggregate line, a decision-stage attribution breakdown (how many
// pairs each stage decided, across the whole corpus), and a list of every
// pair the identity layer scored incorrectly with the stage that decided it
// (for a human to inspect which ground-truth channel, and which stage, is
// weakest).
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

	stageCounts := make(map[DupStage]int, 4)
	for _, c := range r.Cases {
		stageCounts[c.Stage]++
	}
	fmt.Fprintf(&b, "decided by: kind-gate=%d exact-fp=%d similar=%d none=%d\n",
		stageCounts[StageKindGate], stageCounts[StageExactFP], stageCounts[StageSimilar], stageCounts[StageNone])

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
			misses = append(misses, fmt.Sprintf("  [%s/%s] %s: want=%s got=%s", c.Pair.Channel, c.Stage, c.Pair.Name, want, got))
		}
	}
	if len(misses) > 0 {
		b.WriteString("misses:\n" + strings.Join(misses, "\n") + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
