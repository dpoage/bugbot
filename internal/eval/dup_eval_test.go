package eval

import "testing"

// TestRunDupEval_SimilarFindingDecidesPredicted pins that funnel.SimilarFinding
// is the composed v3 stack's TIEBREAK stage (scoreV3's stage 3), reached
// unchanged when the pairs carry no DefectKind/Subject (both empty, so the
// kind gate is a wildcard no-op) and land on different lines (so exact-fp
// never fires): a pair engineered to satisfy SimilarFinding's rule (same
// normalized file, within DefaultMergeWindow lines, jaccard over threshold)
// scores TP when labeled a duplicate, and a pair outside the window scores
// FN despite matching wording.
func TestRunDupEval_SimilarFindingDecidesPredicted(t *testing.T) {
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

// TestRunDupEval_BuiltinBaseline pins the POST-v3 composed identity stack's
// baseline (bugbot-ezmx.9) against the seeded corpus: RunDupEval now scores
// funnel.SameOrUnknownKind -> domain.FingerprintV3 exact equality ->
// funnel.SimilarFinding, not funnel.SimilarFinding alone.
//
// Pre-v3 pin (bugbot-ezmx.8, funnel.SimilarFinding only): precision=1.00
// recall=0.57 (8/14 TP), with every channel showing at least one false
// negative.
//
// Post-v3 pin, per channel (deltas vs the pre-v3 8/14 TP overall):
//   - paraphrase:     4/4 TP (was 3/4) — the heavy-rewrite pair now converges
//     at exact-fp (same locus/kind/subject), bypassing its low description
//     overlap entirely.
//   - cross_lens:     4/4 TP (was 3/4) — the security-vs-perf vocabulary
//     pair converges the same way: two lenses, one root cause, one locus.
//   - caller_callee:  1/3 TP (unchanged) — both root-cause/symptom pairs are
//     cross-file, so FingerprintV3's file component and SimilarFinding's
//     same-file window both still miss them; a cross-file merge is out of
//     scope for this same-file identity/tiebreak composition.
//   - rename:         2/3 TP (was 1/3) — the large-shift pair now converges
//     at exact-fp: the symbol-anchored locus survives 85 lines of drift
//     inserted above the bug (bugbot-ezmx.5's whole point), even though it
//     is still outside SimilarFinding's merge window. The wording-drift
//     pair remains a false negative: it renames the enclosing FUNCTION
//     itself, so even the symbol locus changes.
//
// Aggregate: 11/14 TP, recall RISES from 0.57 to 11/14 ≈ 0.79. Precision
// stays 1.00 — the composed stack introduces no new false positives; every
// SameDefect=false pair is still separated by the kind gate or a subject
// mismatch at exact-fp, exactly as designed into the corpus.
//
// This is the number every later identity change must be measured against —
// a regression here means dedup got WORSE, not better. The two remaining
// caller_callee false negatives and the one rename false negative are
// documented, expected gaps (cross-file merge and function-level rename
// respectively), not bugs in this eval.
func TestRunDupEval_BuiltinBaseline(t *testing.T) {
	res := RunDupEval(BuiltinDupPairs())

	if got, want := res.Precision(), 1.0; got != want {
		t.Errorf("baseline precision = %v, want %v (composed v3 stack must not have started merging distinct defects)", got, want)
	}
	const wantRecall = 11.0 / 14.0 // 11 TP out of 14 SameDefect=true pairs
	if got := res.Recall(); got < wantRecall-1e-9 || got > wantRecall+1e-9 {
		t.Errorf("baseline recall = %v, want %v (regression: identity layer got worse or better without updating this pin)", got, wantRecall)
	}

	wantChannelTally := map[DupChannel]channelTally{
		DupChannelParaphrase:   {TP: 4, FN: 0, TN: 1},
		DupChannelCrossLens:    {TP: 4, FN: 0, TN: 1},
		DupChannelCallerCallee: {TP: 1, FN: 2, TN: 1},
		DupChannelRename:       {TP: 2, FN: 1, TN: 1},
	}
	for ch, want := range wantChannelTally {
		got, ok := res.ByChannel[ch]
		if !ok {
			t.Fatalf("channel %s missing from result", ch)
		}
		if got.TP != want.TP || got.FN != want.FN || got.TN != want.TN || got.FP != 0 {
			t.Errorf("channel %s tally = %+v, want TP=%d FN=%d TN=%d FP=0", ch, *got, want.TP, want.FN, want.TN)
		}
	}
}
