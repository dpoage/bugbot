package funnel

import (
	"context"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/store"
)

// --- dox: abstention / quorum helpers ----------------------------------------

// TestExaminedVerdicts verifies that CouldNotReadCode seats are filtered out
// and that unparseable (failed) seats remain as examined.
func TestExaminedVerdicts(t *testing.T) {
	cases := []struct {
		name    string
		input   []refutation
		wantLen int
	}{
		{"empty", nil, 0},
		{"all examined", []refutation{{Refuted: false}, {Refuted: true}}, 2},
		{"one abstain", []refutation{
			{CouldNotReadCode: true, Refuted: false},
			{Refuted: true},
		}, 1},
		{"all abstain", []refutation{
			{CouldNotReadCode: true},
			{CouldNotReadCode: true},
		}, 0},
		{
			// A no-verdict (infra/parse failure) seat is STILL examined for the
			// kill decision (it counts as "not refuted" so it can never cause a
			// kill); only genuineVerdicts excludes it. See TestGenuineVerdicts.
			name: "no-verdict seat is examined",
			input: []refutation{
				{Refuted: false, NoVerdict: true, Reasoning: "refuter produced no parseable verdict", Confidence: "low"},
				{CouldNotReadCode: true, Refuted: false},
			},
			wantLen: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := examinedVerdicts(tc.input)
			if len(got) != tc.wantLen {
				t.Errorf("examinedVerdicts len = %d, want %d", len(got), tc.wantLen)
			}
		})
	}
}

// TestGenuineVerdicts verifies that BOTH abstaining seats (CouldNotReadCode)
// and no-verdict failures (NoVerdict) are excluded — only seats that produced a
// real, parseable verdict are genuine. This is the survive-trust denominator
// (bugbot-8rd): a missing verdict is evidence of nothing.
func TestGenuineVerdicts(t *testing.T) {
	cases := []struct {
		name    string
		input   []refutation
		wantLen int
	}{
		{"empty", nil, 0},
		{"all genuine", []refutation{{Refuted: false}, {Refuted: true}}, 2},
		{"abstain excluded", []refutation{{CouldNotReadCode: true}, {Refuted: true}}, 1},
		{"no-verdict excluded", []refutation{{NoVerdict: true}, {Refuted: false}}, 1},
		{"all failed/abstain excluded", []refutation{
			{NoVerdict: true}, {NoVerdict: true}, {CouldNotReadCode: true},
		}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := genuineVerdicts(tc.input); len(got) != tc.wantLen {
				t.Errorf("genuineVerdicts len = %d, want %d", len(got), tc.wantLen)
			}
		})
	}
}

// TestMajorityRefuted_NoVerdictCannotKill pins the kill-safety invariant: a
// no-verdict failure counts as "not refuted" in the kill denominator, so it can
// only make a refuted-majority HARDER to reach — a broken refuter must never be
// able to CAUSE a kill (bugbot-8rd).
func TestMajorityRefuted_NoVerdictCannotKill(t *testing.T) {
	// 1 genuine refute + 2 no-verdict: 1/3 is not a majority → not killed.
	oneRefuteTwoFailed := []refutation{
		{Refuted: true},
		{Refuted: false, NoVerdict: true},
		{Refuted: false, NoVerdict: true},
	}
	if majorityRefuted(oneRefuteTwoFailed) {
		t.Error("1 genuine refute + 2 no-verdict must NOT kill (a broken refuter cannot enable a kill)")
	}
	// 2 genuine refutes + 1 no-verdict: 2/3 is a genuine majority → killed.
	twoRefuteOneFailed := []refutation{
		{Refuted: true},
		{Refuted: true},
		{Refuted: false, NoVerdict: true},
	}
	if !majorityRefuted(twoRefuteOneFailed) {
		t.Error("2 genuine refutes + 1 no-verdict must kill (genuine majority)")
	}
}

// TestMajorityRefuted_ExcludesAbstainers verifies that abstaining seats are
// excluded from the denominator. Table covers the dox acceptance criterion:
// an abstainer is neither a refute nor a survive vote.
func TestMajorityRefuted_ExcludesAbstainers(t *testing.T) {
	cases := []struct {
		name     string
		verdicts []refutation
		want     bool
	}{
		{"empty", nil, false},
		{"all abstain → no majority", []refutation{
			{CouldNotReadCode: true}, {CouldNotReadCode: true},
		}, false},
		{"abstainer + refuted: 1/1 examined → kills", []refutation{
			{CouldNotReadCode: true, Refuted: false},
			{Refuted: true},
		}, true},
		{"abstainer + not-refuted: 0/1 examined → survives", []refutation{
			{CouldNotReadCode: true, Refuted: false},
			{Refuted: false},
		}, false},
		{"2 abstain + 1 not-refuted: 0/1 → survives", []refutation{
			{CouldNotReadCode: true},
			{CouldNotReadCode: true},
			{Refuted: false},
		}, false},
		{"2 refuted + 1 abstain: 2/2 examined → kills", []refutation{
			{Refuted: true},
			{Refuted: true},
			{CouldNotReadCode: true},
		}, true},
		{"1 refuted + 1 not-refuted + 1 abstain: tie on 2 examined → survives", []refutation{
			{Refuted: true},
			{Refuted: false},
			{CouldNotReadCode: true},
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := majorityRefuted(tc.verdicts)
			if got != tc.want {
				t.Errorf("majorityRefuted = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestIsSplitVerdict_ExcludesAbstainers verifies that abstaining seats do not
// count toward the split denominator.
func TestIsSplitVerdict_ExcludesAbstainers(t *testing.T) {
	cases := []struct {
		name     string
		verdicts []refutation
		want     bool
	}{
		{"abstainer + refuted: 1 examined → no split", []refutation{
			{CouldNotReadCode: true},
			{Refuted: true},
		}, false},
		{"abstainer + refuted + not-refuted: split on 2 examined", []refutation{
			{CouldNotReadCode: true},
			{Refuted: true},
			{Refuted: false},
		}, true},
		{"all abstain", []refutation{
			{CouldNotReadCode: true},
			{CouldNotReadCode: true},
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isSplitVerdict(tc.verdicts)
			if got != tc.want {
				t.Errorf("isSplitVerdict = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestBelowQuorum verifies the quorum floor (strict majority of panelSize
// must have examined the code).
func TestBelowQuorum(t *testing.T) {
	cases := []struct {
		name      string
		examined  int
		panelSize int
		want      bool // true = below quorum → NeedsHuman
	}{
		{"n=1, 1 examined: at floor", 1, 1, false},
		{"n=1, 0 examined: below floor", 0, 1, true},
		{"n=3, 2 examined: at floor (2*2=4 > 3)", 2, 3, false},
		{"n=3, 1 examined: below floor (1*2=2 <= 3)", 1, 3, true},
		{"n=3, 0 examined: below floor", 0, 3, true},
		{"n=2, 1 examined: below floor (1*2=2 <= 2)", 1, 2, true},
		{"n=2, 2 examined: meets floor", 2, 2, false},
		{"n=5, 3 examined: at floor (3*2=6 > 5)", 3, 5, false},
		{"n=5, 2 examined: below floor (2*2=4 <= 5)", 2, 5, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := belowQuorum(tc.examined, tc.panelSize)
			if got != tc.want {
				t.Errorf("belowQuorum(%d, %d) = %v, want %v", tc.examined, tc.panelSize, got, tc.want)
			}
		})
	}
}

// TestBuildReasoning_AbstainerLabel verifies that abstaining seats are labeled
// distinctly (not "could not refute") in the trace.
func TestBuildReasoning_AbstainerLabel(t *testing.T) {
	verdicts := []refutation{
		{CouldNotReadCode: true, Refuted: false, Reasoning: "tool error: file not found", Confidence: "low"},
		{Refuted: false, Reasoning: "path is reachable, no guard", Confidence: "high"},
	}
	seatNames := []string{"reachability", "semantics"}
	out := buildReasoning(verdicts, seatNames, "", false)
	if !strings.Contains(out, "abstained (could not read cited code)") {
		t.Errorf("abstaining seat not labeled distinctly; trace:\n%s", out)
	}
	// The non-abstaining seat should say "could not refute" not "abstained"
	if !strings.Contains(out, "semantics") {
		t.Errorf("non-abstaining seat (semantics) missing from trace:\n%s", out)
	}
}

// --- dox: #28 replay shape ---------------------------------------------------

// TestDox28Shape: 3-seat panel where 1 seat abstains (CouldNotReadCode) and
// the remaining 2 produce 0 refutations → candidate survives on the genuine
// examined votes, NOT on the abstainer's strength.
func TestDox28Shape(t *testing.T) {
	verdicts := []refutation{
		{CouldNotReadCode: true, Refuted: false, Reasoning: "tool error: could not locate file", Confidence: "low"},
		{Refuted: false, Reasoning: "nil path is reachable", Confidence: "high"},
		{Refuted: false, Reasoning: "no guard before dereference", Confidence: "medium"},
	}
	// majorityRefuted over 2 examined, 0 refuted → false: candidate survives
	if majorityRefuted(verdicts) {
		t.Error("with 0/2 examined refuting, candidate should survive (not refuted)")
	}
	// No split: both examined seats agree (not-refuted)
	if isSplitVerdict(verdicts) {
		t.Error("both examined seats agree → no split expected")
	}
}

// TestDox28Shape_AbstainerDoesNotInflateExamined verifies a case where 2 examined
// refute and 1 abstains: the 2 examined refuters form a majority → killed.
// The abstainer's absence does not save the candidate when the genuine examined
// majority refutes.
func TestDox28Shape_AbstainerDoesNotInflateExamined(t *testing.T) {
	verdicts := []refutation{
		{CouldNotReadCode: true, Refuted: false, Reasoning: "could not open file", Confidence: "low"},
		{Refuted: true, Reasoning: "the guard is present, no nil path", Confidence: "high"},
		{Refuted: true, Reasoning: "caller checks before call", Confidence: "high"},
	}
	// 2/2 examined refuted → true: candidate killed
	if !majorityRefuted(verdicts) {
		t.Error("2/2 examined refuting should kill the candidate")
	}
}

// TestDox_NeedsHuman_BelowQuorumFloor tests the quorum-floor decision helper.
func TestDox_NeedsHuman_BelowQuorumFloor(t *testing.T) {
	// panelSize=3, examined=1 → below floor → NeedsHuman must be true
	if !belowQuorum(1, 3) {
		t.Error("1 examined of 3 panel should be below quorum floor → NeedsHuman")
	}
	// panelSize=3, examined=2 → at floor → NeedsHuman false
	if belowQuorum(2, 3) {
		t.Error("2 examined of 3 panel is at/above quorum floor → no NeedsHuman")
	}
}

// --- dox: quorum floor in full sweep -----------------------------------------

// makeAbstainVerifier returns a scriptedClient where the first n calls return
// abstainJSON (could_not_read_code=true), subsequent refuter calls return
// notRefutedJSON, and the arbiter (PANEL VERDICTS) returns notRefutedJSON.
func makeAbstainVerifier(abstainFirstN int) *scriptedClient {
	const abstainJSON = `{"refuted": false, "reasoning": "tool error: could not open file", "confidence": "low", "could_not_read_code": true}`
	sc := newScriptedClient()
	sc.onTaskContains("PANEL VERDICTS", notRefutedJSON)
	callIdx := 0
	sc.on(func(_ llm.Request) bool {
		i := callIdx
		callIdx++
		return i < abstainFirstN
	}, abstainJSON)
	sc.fallback = notRefutedJSON
	return sc
}

// TestDox_QuorumFloor_NeedsHuman: 3-seat panel where 2 abstain and 1 says
// not-refuted → examined=1 < floor → persisted finding must have NeedsHuman=true.
func TestDox_QuorumFloor_NeedsHuman(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	finder := newScriptedClient().onSystemContains("nil-safety/error-handling", candJSON(realCand))
	// 3-seat panel: first 2 abstain, 3rd does not refute → 1 examined < floor.
	// No split among examined seats → no arbiter.
	verifier := makeAbstainVerifier(2)

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{Discovery: DiscoveryConfig{Lenses: []string{"nil-safety/error-handling"}}, Limits: StageLimits{Refuters: 3}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Findings) != 1 {
		t.Fatalf("want 1 finding (survived, below quorum), got %d", len(res.Findings))
	}
	if !res.Findings[0].NeedsHuman {
		t.Error("finding with <quorum examined seats must have NeedsHuman=true")
	}
}

// --- nn3: corrected description from arbiter ---------------------------------

// TestNN3_CorrectedDescription: split panel where arbiter returns
// corrected_description → persisted finding uses arbiter's description, not
// the finder-original.
func TestNN3_CorrectedDescription(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	finder := newScriptedClient().onSystemContains("nil-safety/error-handling", candJSON(realCand))

	correctedDesc := "cfg is dereferenced via an interface wrapper, not a direct pointer; the nil check is bypassed through the indirect call path"
	// Arbiter response with corrected_description (finding survives).
	arbiterJSON := `{"refuted": false, "reasoning": "The bug is real but the mechanism involves an indirect call path", "confidence": "high", "evidence": ["f.go:1"], "corrected_description": "` + correctedDesc + `"}`

	// Split: seat 1 refuted, seat 2 not-refuted → arbiter runs.
	verifier := makeCallCountVerifier(1, arbiterJSON)

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{Discovery: DiscoveryConfig{Lenses: []string{"nil-safety/error-handling"}}, Limits: StageLimits{Refuters: 2}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Findings) != 1 {
		t.Fatalf("want 1 surviving finding, got %d", len(res.Findings))
	}
	if res.Findings[0].Description != correctedDesc {
		t.Errorf("finding description = %q, want corrected mechanism %q",
			res.Findings[0].Description, correctedDesc)
	}
}

// TestNN3_NoCorrection: arbiter without corrected_description → original
// finder description is preserved unchanged.
func TestNN3_NoCorrection(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	finder := newScriptedClient().onSystemContains("nil-safety/error-handling", candJSON(realCand))

	// Arbiter with no corrected_description field.
	arbiterJSON := `{"refuted": false, "reasoning": "bug stands, no mechanism error", "confidence": "high", "evidence": ["f.go:1"]}`
	verifier := makeCallCountVerifier(1, arbiterJSON)

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{Discovery: DiscoveryConfig{Lenses: []string{"nil-safety/error-handling"}}, Limits: StageLimits{Refuters: 2}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Findings) != 1 {
		t.Fatalf("want 1 surviving finding, got %d", len(res.Findings))
	}
	// Original description from realCand
	if res.Findings[0].Description != "cfg may be nil" {
		t.Errorf("description should be original when arbiter emits no correction; got %q",
			res.Findings[0].Description)
	}
}

// TestNN3_HallucinatedRebuttal: arbiter with hallucinated_rebuttal=true is
// surfaced in the reasoning trace.
func TestNN3_HallucinatedRebuttal(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	finder := newScriptedClient().onSystemContains("nil-safety/error-handling", candJSON(realCand))

	// Arbiter detects a hallucinated rebuttal from a panel seat.
	arbiterJSON := `{"refuted": false, "reasoning": "Seat 1 claimed a nil-check guard exists that is NOT present in the file", "confidence": "high", "evidence": ["f.go:1"], "hallucinated_rebuttal": true}`
	verifier := makeCallCountVerifier(1, arbiterJSON)

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{Discovery: DiscoveryConfig{Lenses: []string{"nil-safety/error-handling"}}, Limits: StageLimits{Refuters: 2}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Findings) != 1 {
		t.Fatalf("want 1 surviving finding, got %d", len(res.Findings))
	}
	// The hallucinated rebuttal flag must be visible in the reasoning trace.
	if !strings.Contains(res.Findings[0].Reasoning, "hallucinated rebuttal") {
		t.Errorf("reasoning trace should flag hallucinated rebuttal; got:\n%s",
			res.Findings[0].Reasoning)
	}
}

// --- nn3: unanimous-survive panel with mechanism correction ------------------

// TestNN3_UnanimousSurvive_CorrectedDescription: the natural #5 shape — all
// panel seats cannot refute (bug stands), one seat sets corrected_description
// because the mechanism is wrong. No split → no arbiter. The persisted
// finding.Description must be the corrected mechanism, not the finder-original.
// This is the primary hole identified by the oracle: the previous implementation
// only folded corrections when the arbiter ran (split panels), silently dropping
// unanimous-survive corrections.
func TestNN3_UnanimousSurvive_CorrectedDescription(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	finder := newScriptedClient().onSystemContains("nil-safety/error-handling", candJSON(realCand))

	correctedDesc := "cfg is dereferenced via a method call on the returned interface, not a direct field access; the nil pointer arises in the method dispatch, not the assignment"
	// Seat 1: not-refuted, mechanism wrong → corrected_description set.
	seat1JSON := `{"refuted": false, "reasoning": "The nil path exists but via an interface method, not direct dereference", "confidence": "high", "corrected_description": "` + correctedDesc + `"}`
	// Seat 2: not-refuted, no mechanism correction (agrees bug stands, mechanism ok to seat 2).
	// Seat 3: not-refuted, no correction.
	// All three seats survive → unanimous survive → no arbiter.
	// Seat 1 has highest confidence (high) and a non-empty correction → it wins.
	callIdx := 0
	v := newScriptedClient()
	// No arbiter needed (not split), but guard it anyway.
	v.onTaskContains("PANEL VERDICTS", notRefutedJSON)
	v.on(func(req llm.Request) bool {
		i := callIdx
		callIdx++
		return i == 0
	}, seat1JSON)
	v.fallback = notRefutedJSON

	f, err := New(RoleClients{Finder: finder, Verifier: v}, st, repo, Options{Discovery: DiscoveryConfig{Lenses: []string{"nil-safety/error-handling"}}, Limits: StageLimits{Refuters: 3}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Findings) != 1 {
		t.Fatalf("want 1 surviving finding (unanimous not-refuted), got %d", len(res.Findings))
	}
	if res.Stats.ArbiterRuns != 0 {
		t.Errorf("ArbiterRuns = %d, want 0 (unanimous panel must not spawn arbiter)", res.Stats.ArbiterRuns)
	}
	if res.Findings[0].Description != correctedDesc {
		t.Errorf("finding description = %q\nwant corrected mechanism = %q",
			res.Findings[0].Description, correctedDesc)
	}
}

// TestNN3_UnanimousSurvive_NoCorrection_PreservesOriginal verifies the
// no-correction unanimous path: if no seat sets corrected_description, the
// finder-original description is preserved.
func TestNN3_UnanimousSurvive_NoCorrection_PreservesOriginal(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	finder := newScriptedClient().onSystemContains("nil-safety/error-handling", candJSON(realCand))
	v := newScriptedClient()
	v.fallback = notRefutedJSON // all seats: not-refuted, no corrected_description

	f, err := New(RoleClients{Finder: finder, Verifier: v}, st, repo, Options{Discovery: DiscoveryConfig{Lenses: []string{"nil-safety/error-handling"}}, Limits: StageLimits{Refuters: 3}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Findings) != 1 {
		t.Fatalf("want 1 surviving finding, got %d", len(res.Findings))
	}
	if res.Findings[0].Description != "cfg may be nil" {
		t.Errorf("description = %q, want original (no correction emitted)", res.Findings[0].Description)
	}
}

// TestBestRefuterCorrection_HighestConfidenceWins verifies that when multiple
// examined not-refuted seats emit corrected_description, the one with the
// highest confidence wins.
func TestBestRefuterCorrection_HighestConfidenceWins(t *testing.T) {
	examined := []refutation{
		{Refuted: false, Confidence: "low", CorrectedDescription: "low-confidence correction"},
		{Refuted: false, Confidence: "high", CorrectedDescription: "high-confidence correction"},
		{Refuted: false, Confidence: "medium", CorrectedDescription: "medium-confidence correction"},
	}
	got := bestRefuterCorrection(examined, "fallback")
	if got != "high-confidence correction" {
		t.Errorf("bestRefuterCorrection = %q, want high-confidence correction", got)
	}
}

// TestBestRefuterCorrection_RefutedSeatSkipped verifies that a seat with
// Refuted=true is never used for a mechanism correction (it claimed to have
// disproved the bug, so no description correction is applicable).
func TestBestRefuterCorrection_RefutedSeatSkipped(t *testing.T) {
	examined := []refutation{
		{Refuted: true, Confidence: "high", CorrectedDescription: "refuter correction (should be ignored)"},
		{Refuted: false, Confidence: "medium", CorrectedDescription: "survivor correction"},
	}
	got := bestRefuterCorrection(examined, "fallback")
	if got != "survivor correction" {
		t.Errorf("bestRefuterCorrection = %q, want survivor correction (refuted seat must be skipped)", got)
	}
}

// TestBestRefuterCorrection_FallbackWhenNone verifies that the finder-original
// fallback is returned when no examined seat emits a correction.
func TestBestRefuterCorrection_FallbackWhenNone(t *testing.T) {
	examined := []refutation{
		{Refuted: false, Confidence: "high", CorrectedDescription: ""},
		{Refuted: false, Confidence: "medium", CorrectedDescription: ""},
	}
	got := bestRefuterCorrection(examined, "original description")
	if got != "original description" {
		t.Errorf("bestRefuterCorrection = %q, want fallback", got)
	}
}

// --- nn3: unanimous-refuted shared hallucination (false-negative pin) --------

// TestNN3_UnanimousRefuted_SharedHallucination_PinnedBehavior documents the
// known limitation: when ALL examined seats assert the existence of nonexistent
// 'safe' code and unanimously refute → the candidate is killed with no arbiter
// and no hallucination flag. This is the unanimous-hallucination false negative
// described in oracle hole H2. The test pins the CURRENT behavior (candidate
// killed, no flag) rather than asserting detection, so any future change that
// adds detection will need to update this test intentionally.
func TestNN3_UnanimousRefuted_SharedHallucination_PinnedBehavior(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	finder := newScriptedClient().onSystemContains("nil-safety/error-handling", candJSON(realCand))
	// All 3 seats refute, each claiming a nonexistent nil-check guard. Unanimous
	// refute → no arbiter → no hallucination flag possible in this path.
	hallucinatedRefuteJSON := `{"refuted": true, "reasoning": "There is a nil-check at line 5 of cfg_setup.go (does not exist)", "confidence": "high", "hallucinated_rebuttal": true}`

	v := newScriptedClient()
	v.fallback = hallucinatedRefuteJSON

	f, err := New(RoleClients{Finder: finder, Verifier: v}, st, repo, Options{Discovery: DiscoveryConfig{Lenses: []string{"nil-safety/error-handling"}}, Limits: StageLimits{Refuters: 3}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// PINNED: unanimous-refuted kills the candidate; no arbiter runs; no flag.
	if len(res.Findings) != 0 {
		t.Errorf("pinned: unanimous refute kills candidate, got %d findings", len(res.Findings))
	}
	if res.Stats.ArbiterRuns != 0 {
		t.Errorf("pinned: no arbiter on unanimous panel, got ArbiterRuns=%d", res.Stats.ArbiterRuns)
	}
	// Document: hallucinated_rebuttal in refuter verdicts is not surfaced on
	// the kill path (no reasoning trace for a killed candidate). This is the
	// known gap: a future improvement would detect unanimous hallucination.
	if res.Stats.Killed != 1 {
		t.Errorf("pinned: killed=1 expected, got %d", res.Stats.Killed)
	}
}

// --- bugbot-8rd: no-verdict failures must not fail-open ----------------------

// TestBelowQuorum_NoVerdictDoesNotCount: a panel with 1 genuine verdict and 2
// no-verdict failures is below the survive quorum (the failures do not count),
// so a survivor must be flagged NeedsHuman. It is NOT zero-genuine, so it is not
// orphaned — one seat genuinely judged it.
func TestBelowQuorum_NoVerdictDoesNotCount(t *testing.T) {
	panel := []refutation{
		{Refuted: false},                  // genuine not-refuted
		{Refuted: false, NoVerdict: true}, // failure
		{Refuted: false, NoVerdict: true}, // failure
	}
	genuine := genuineVerdicts(panel)
	if len(genuine) != 1 {
		t.Fatalf("genuine = %d, want 1", len(genuine))
	}
	if !belowQuorum(len(genuine), len(panel)) {
		t.Error("1 genuine of 3 must be below quorum (no-verdict seats do not count)")
	}
}

// proseVerifier returns a scriptedClient whose every completion is prose that
// can never parse as a refuter verdict, so RunJSON's parse + repair both fail
// and every refuter seat is recorded as NoVerdict (an infrastructure/parse
// failure). This models the live shimmy incident: the verifier provider erroring
// out so no seat returns a usable verdict.
func proseVerifier() *scriptedClient {
	sc := newScriptedClient()
	sc.fallback = "I analyzed the code, but I am returning prose, not JSON."
	return sc
}

// TestSweep_AllRefutersFail_OrphanedAsT3Suspected is the bugbot-8rd regression:
// when EVERY refuter produces no parseable verdict, the candidate must NOT be
// promoted as a confident T2 survivor. It is orphaned as T3 suspected (so the
// next scan re-verifies it) and the degraded panel is recorded in agent_units.
func TestSweep_AllRefutersFail_OrphanedAsT3Suspected(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	finder := newScriptedClient().onSystemContains("nil-safety/error-handling", candJSON(realCand))
	verifier := proseVerifier()

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{
		Discovery: DiscoveryConfig{Lenses: []string{"nil-safety/error-handling"}},
		Limits:    StageLimits{Refuters: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Findings) != 1 {
		t.Fatalf("want 1 finding (orphaned, not dropped), got %d", len(res.Findings))
	}
	got := res.Findings[0]
	if got.Tier != domain.TierSuspected {
		t.Errorf("tier = %d, want %d (T3 suspected): an all-failed panel must NOT promote to T2", got.Tier, domain.TierSuspected)
	}
	if got.Status != store.StatusOpen {
		t.Errorf("status = %q, want open", got.Status)
	}
	if strings.Contains(got.Reasoning, "Survived adversarial verification") {
		t.Errorf("orphaned finding must not claim it survived verification:\n%s", got.Reasoning)
	}
	if !strings.Contains(got.Reasoning, "Verification incomplete") {
		t.Errorf("orphaned reasoning should explain the verification failure:\n%s", got.Reasoning)
	}
	if res.Stats.Verified != 0 || res.Stats.Killed != 0 {
		t.Errorf("verified=%d killed=%d, want 0/0 (no genuine verdict)", res.Stats.Verified, res.Stats.Killed)
	}
	if res.Stats.Suspected != 1 {
		t.Errorf("suspected = %d, want 1", res.Stats.Suspected)
	}
	if res.Stats.VerifierFailures != 3 {
		t.Errorf("verifier_failures = %d, want 3 (every seat failed)", res.Stats.VerifierFailures)
	}

	// AC2: the degraded panel is recorded in the verifier agent_units row.
	units, err := st.ListAgentUnits(ctx, res.ScanRunID)
	if err != nil {
		t.Fatalf("ListAgentUnits: %v", err)
	}
	var foundVerifier bool
	for _, u := range units {
		if u.Role != "verifier" {
			continue
		}
		foundVerifier = true
		if string(u.Status) != "orphaned_verify_failed" {
			t.Errorf("verifier unit status = %q, want orphaned_verify_failed", u.Status)
		}
		if !strings.Contains(u.Detail, "noverdict=3") {
			t.Errorf("verifier unit detail = %q, want it to record noverdict=3", u.Detail)
		}
	}
	if !foundVerifier {
		t.Error("no verifier agent_units row recorded for the orphaned candidate")
	}
}
