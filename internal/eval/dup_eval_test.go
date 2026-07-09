package eval

import "testing"

// TestRunDupEval_SmilarFindingDecidesPredicted pins that RunDupEval's
// Predicted field is exactly funnel.SimilarFinding's decision, not some
// reimplementation: a pair engineered to satisfy SimilarFinding's rule
// (same normalized file, within DefaultMergeWindow lines, jaccard over
// threshold) scores TP when labeled a duplicate, and a pair outside the
// window scores FN despite matching wording.
func TestRunDupEval_SmilarFindingDecidesPredicted(t *testing.T) {
	pairs := []DupPair{
		{
			Name:       "within-window-high-overlap",
			Channel:    DupChannelParaphrase,
			A:          DupCandidate{File: "x.go", Line: 10, Desc: "nil pointer dereference on the response writer here"},
			B:          DupCandidate{File: "x.go", Line: 12, Desc: "nil pointer dereference on the response writer there"},
			SameDefect: true,
		},
		{
			Name:       "outside-window-high-overlap",
			Channel:    DupChannelRename,
			A:          DupCandidate{File: "x.go", Line: 10, Desc: "nil pointer dereference on the response writer here"},
			B:          DupCandidate{File: "x.go", Line: 30, Desc: "nil pointer dereference on the response writer there"},
			SameDefect: true,
		},
	}
	res := RunDupEval(pairs)
	if len(res.Cases) != 2 {
		t.Fatalf("len(Cases) = %d, want 2", len(res.Cases))
	}
	if !res.Cases[0].Predicted || !res.Cases[0].Correct() {
		t.Errorf("within-window pair: Predicted=%v Correct=%v, want true/true", res.Cases[0].Predicted, res.Cases[0].Correct())
	}
	if res.Cases[1].Predicted || res.Cases[1].Correct() {
		t.Errorf("outside-window pair: Predicted=%v Correct=%v, want false/false (FN)", res.Cases[1].Predicted, res.Cases[1].Correct())
	}
}

// TestRunDupEval_PrecisionRecallFormulas pins the TP/FP/FN/TN bucketing and
// the vacuous-1.0 convention shared with score.go's CaseResult.
func TestRunDupEval_PrecisionRecallFormulas(t *testing.T) {
	pairs := []DupPair{
		// TP: predicted duplicate, really a duplicate.
		{Name: "tp", Channel: DupChannelParaphrase, SameDefect: true,
			A: DupCandidate{File: "a.go", Line: 1, Desc: "leaked file handle on error path here"},
			B: DupCandidate{File: "a.go", Line: 2, Desc: "leaked file handle on error path there"}},
		// FN: predicted not-duplicate, but really is (line far apart).
		{Name: "fn", Channel: DupChannelRename, SameDefect: true,
			A: DupCandidate{File: "a.go", Line: 1, Desc: "leaked file handle on error path here"},
			B: DupCandidate{File: "a.go", Line: 90, Desc: "leaked file handle on error path there"}},
		// TN: predicted not-duplicate, and really isn't (different files).
		{Name: "tn", Channel: DupChannelCrossLens, SameDefect: false,
			A: DupCandidate{File: "a.go", Line: 1, Desc: "leaked file handle on error path here"},
			B: DupCandidate{File: "b.go", Line: 1, Desc: "totally unrelated defect about locking"}},
	}
	res := RunDupEval(pairs)
	if got, want := res.Precision(), 1.0; got != want {
		t.Errorf("Precision() = %v, want %v (no FPs seeded)", got, want)
	}
	if got, want := res.Recall(), 0.5; got != want {
		t.Errorf("Recall() = %v, want %v (1 TP, 1 FN)", got, want)
	}
}

// TestRunDupEval_VacuousWhenEmpty pins the empty-corpus edge case: both
// metrics report 1.0 (nothing to be wrong about), mirroring CaseResult.
func TestRunDupEval_VacuousWhenEmpty(t *testing.T) {
	res := RunDupEval(nil)
	if res.Precision() != 1 || res.Recall() != 1 {
		t.Fatalf("empty corpus precision/recall = %v/%v, want 1/1", res.Precision(), res.Recall())
	}
}

// TestBuiltinDupPairs_CoversAllChannels pins the corpus shape the acceptance
// criteria require: every channel present with at least a handful of pairs,
// and a mix of SameDefect true/false so both precision and recall are
// actually exercised per channel.
func TestBuiltinDupPairs_CoversAllChannels(t *testing.T) {
	pairs := BuiltinDupPairs()
	const minPerChannel = 4
	want := []DupChannel{DupChannelParaphrase, DupChannelCrossLens, DupChannelCallerCallee, DupChannelRename}

	counts := make(map[DupChannel]struct{ pos, neg int })
	names := make(map[string]bool, len(pairs))
	for _, p := range pairs {
		if names[p.Name] {
			t.Fatalf("duplicate pair name %q", p.Name)
		}
		names[p.Name] = true
		c := counts[p.Channel]
		if p.SameDefect {
			c.pos++
		} else {
			c.neg++
		}
		counts[p.Channel] = c
	}

	for _, ch := range want {
		c, ok := counts[ch]
		total := c.pos + c.neg
		if !ok || total < minPerChannel {
			t.Errorf("channel %s has %d pairs, want at least %d", ch, total, minPerChannel)
		}
		if c.pos == 0 {
			t.Errorf("channel %s has no SameDefect=true pairs (no recall signal)", ch)
		}
		if c.neg == 0 {
			t.Errorf("channel %s has no SameDefect=false pairs (no precision signal)", ch)
		}
	}
}

// TestRunDupEval_BuiltinBaseline pins the CURRENT v2 identity layer's
// baseline against the seeded corpus: perfect precision (it never merges two
// distinct defects in this corpus — the conservative threshold documented in
// cluster.go holds) but well under perfect recall (it misses roughly half of
// the true duplicates: heavy paraphrase, cross-lens vocabulary drift,
// cross-file caller/callee splits, and rename line-drift all fall outside
// the file+10-line+jaccard rule). This is the number every later identity
// change (bugbot-ezmx.1's v3, etc.) must be measured against — a regression
// here means dedup got WORSE, not better.
func TestRunDupEval_BuiltinBaseline(t *testing.T) {
	res := RunDupEval(BuiltinDupPairs())

	if got, want := res.Precision(), 1.0; got != want {
		t.Errorf("baseline precision = %v, want %v (v2 must not have started merging distinct defects)", got, want)
	}
	const wantRecall = 8.0 / 14.0 // 8 TP out of 14 SameDefect=true pairs
	if got := res.Recall(); got < wantRecall-1e-9 || got > wantRecall+1e-9 {
		t.Errorf("baseline recall = %v, want %v (regression: identity layer got worse or better without updating this pin)", got, wantRecall)
	}

	// Every channel must show at least one recall gap in the CURRENT layer —
	// that gap is precisely what the epic's later children are meant to
	// close, and losing it here would mean the corpus stopped being a useful
	// baseline.
	for ch, t2 := range res.ByChannel {
		if t2.FN == 0 {
			t.Errorf("channel %s has zero false negatives in the v2 baseline; corpus should demonstrate a known gap for every channel", ch)
		}
	}
}
