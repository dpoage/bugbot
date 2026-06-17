package funnel

import (
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/store"
)

// --- Strategy task builder ---------------------------------------------------

// TestStrategyTask_SweepWide_Unchanged verifies that sweepWide.BuildTask is nil,
// meaning finderTask is used unchanged — the default strategy introduces no new
// task framing.
func TestStrategyTask_SweepWide_Unchanged(t *testing.T) {
	if sweepWide.BuildTask != nil {
		t.Error("sweepWide.BuildTask must be nil so finderTask is used unchanged")
	}
}

// TestStrategyTask_ContractTraceDeep_SeedFraming verifies that
// buildContractTraceDeepTask frames the files as SEED FILES and includes the
// standard CROSS-LENS LEADS section when leads are non-empty — using the same
// newline-flattening logic as finderTask.
func TestStrategyTask_ContractTraceDeep_SeedFraming(t *testing.T) {
	files := []string{"pkg/config.go", "pkg/options.go"}
	leads := []store.Lead{
		{
			PosterLens: "concurrency",
			TargetLens: "api-contract-misuse",
			File:       "pkg/config.go",
			Line:       42,
			Note:       "field validated\nincorrectly",
		},
	}

	task := buildContractTraceDeepTask(files, leads)

	// Must include SEED FILES framing (not "audit").
	if !strings.Contains(task, "SEED FILES:") {
		t.Error("deep task must contain SEED FILES framing")
	}
	if strings.Contains(task, "Audit these target files") {
		t.Error("deep task must NOT contain sweep-wide audit framing")
	}

	// All seed files must appear.
	for _, f := range files {
		if !strings.Contains(task, f) {
			t.Errorf("deep task missing seed file %q", f)
		}
	}

	// CROSS-LENS LEADS section must appear.
	if !strings.Contains(task, "CROSS-LENS LEADS") {
		t.Error("deep task missing CROSS-LENS LEADS section")
	}
	if !strings.Contains(task, "pkg/config.go:42") {
		t.Error("deep task missing lead location")
	}

	// Prompt-injection guard: note newlines must be flattened to spaces.
	if strings.Contains(task, "validated\nincorrectly") {
		t.Error("deep task must flatten newlines in lead notes (prompt-injection guard)")
	}
	if !strings.Contains(task, "validated incorrectly") {
		t.Error("deep task must flatten note to single line")
	}
}

// TestStrategyTask_ContractTraceDeep_NoLeads verifies the task is correct when
// the leads slice is empty — no CROSS-LENS LEADS section.
func TestStrategyTask_ContractTraceDeep_NoLeads(t *testing.T) {
	files := []string{"internal/server/config.go"}
	task := buildContractTraceDeepTask(files, nil)

	if !strings.Contains(task, "SEED FILES:") {
		t.Error("deep task must contain SEED FILES framing")
	}
	if !strings.Contains(task, "internal/server/config.go") {
		t.Error("deep task missing seed file")
	}
	if strings.Contains(task, "CROSS-LENS LEADS") {
		t.Error("deep task must not contain CROSS-LENS LEADS section when no leads")
	}
}

// TestStrategyTask_StateTraceDeep_SeedFraming verifies that
// buildStateTraceDeepTask frames the files as SEED FILES (with the
// shared-state and resource-lifecycle phrasing) and includes the standard
// CROSS-LENS LEADS section when leads are non-empty — using the same
// newline-flattening logic as finderTask and buildContractTraceDeepTask.
func TestStrategyTask_StateTraceDeep_SeedFraming(t *testing.T) {
	files := []string{"pkg/server/conn.go", "pkg/server/pool.go"}
	leads := []store.Lead{
		{
			PosterLens: "nil-safety/error-handling",
			TargetLens: "concurrency",
			File:       "pkg/server/conn.go",
			Line:       17,
			Note:       "mutex held\nacross defer",
		},
	}

	task := buildStateTraceDeepTask(files, leads)

	// Must include the SEED FILES framing line.
	if !strings.Contains(task, "SEED FILES (a starting point for shared-state and resource-lifecycle tracing):") {
		t.Error("state-trace-deep task must contain the SEED FILES framing with the shared-state/lifecycle qualifier")
	}
	if strings.Contains(task, "Audit these target files") {
		t.Error("state-trace-deep task must NOT contain sweep-wide audit framing")
	}

	// All seed files must appear.
	for _, f := range files {
		if !strings.Contains(task, f) {
			t.Errorf("state-trace-deep task missing seed file %q", f)
		}
	}

	// CROSS-LENS LEADS section must appear.
	if !strings.Contains(task, "CROSS-LENS LEADS") {
		t.Error("state-trace-deep task missing CROSS-LENS LEADS section")
	}
	if !strings.Contains(task, "pkg/server/conn.go:17") {
		t.Error("state-trace-deep task missing lead location")
	}

	// Prompt-injection guard: note newlines must be flattened to spaces.
	if strings.Contains(task, "mutex held\nacross defer") {
		t.Error("state-trace-deep task must flatten newlines in lead notes (prompt-injection guard)")
	}
	if !strings.Contains(task, "mutex held across defer") {
		t.Error("state-trace-deep task must flatten note to single line")
	}
}

// TestStrategyTask_StateTraceDeep_NoLeads verifies the task is correct when
// the leads slice is empty — no CROSS-LENS LEADS section.
func TestStrategyTask_StateTraceDeep_NoLeads(t *testing.T) {
	files := []string{"internal/server/conn.go"}
	task := buildStateTraceDeepTask(files, nil)

	if !strings.Contains(task, "SEED FILES") {
		t.Error("state-trace-deep task must contain SEED FILES framing")
	}
	if !strings.Contains(task, "internal/server/conn.go") {
		t.Error("state-trace-deep task missing seed file")
	}
	if strings.Contains(task, "CROSS-LENS LEADS") {
		t.Error("state-trace-deep task must not contain CROSS-LENS LEADS section when no leads")
	}
}

// TestStrategyTask_LeadsFlatteningShared verifies that finderTask,
// buildContractTraceDeepTask, and buildStateTraceDeepTask all flatten note
// newlines identically — they must share the same helper (appendLeadsSection)
// so the prompt-injection guard cannot fork across strategies.
func TestStrategyTask_LeadsFlatteningShared(t *testing.T) {
	files := []string{"a.go"}
	leads := []store.Lead{{
		PosterLens: "nil-safety/error-handling",
		TargetLens: "api-contract-misuse",
		File:       "a.go",
		Line:       1,
		Note:       "line one\nline two\ttab here",
	}}

	wideTask := finderTask(files, leads)
	deepTask := buildContractTraceDeepTask(files, leads)
	stateTask := buildStateTraceDeepTask(files, leads)

	// All three must contain the flattened note.
	wantNote := "line one line two tab here"
	if !strings.Contains(wideTask, wantNote) {
		t.Errorf("finderTask did not flatten lead note: %q", wideTask)
	}
	if !strings.Contains(deepTask, wantNote) {
		t.Errorf("buildContractTraceDeepTask did not flatten lead note: %q", deepTask)
	}
	if !strings.Contains(stateTask, wantNote) {
		t.Errorf("buildStateTraceDeepTask did not flatten lead note: %q", stateTask)
	}

	// None may contain raw newlines or tabs in the note.
	for _, task := range []string{wideTask, deepTask, stateTask} {
		if strings.Contains(task, "line one\nline two") {
			t.Error("task contains unflattened newline in lead note (prompt-injection risk)")
		}
	}
}

// --- System prompt composition -----------------------------------------------

// TestSystemPrompt_SweepWide_ByteIdentical verifies that sweep-wide (empty
// SystemClause) produces a system prompt that is BYTE-IDENTICAL to the output
// of finderSystemPrompt — the strategy axis introduces zero bytes for default
// strategy units. It also exercises an unaffected lens (concurrency) to make
// sure adding state-trace-deep does not change the sweep-wide prompt for any
// lens.
func TestSystemPrompt_SweepWide_ByteIdentical(t *testing.T) {
	for _, name := range []string{"api-contract-misuse", "concurrency", "resource-leaks"} {
		l := lensByName(t, name)
		langs := []ingest.Language{ingest.LangGo}
		persona := "senior Go engineer"

		if sweepWide.SystemClause != "" {
			t.Fatalf("sweepWide.SystemClause must be empty for byte-identical test to be valid (lens %q)", name)
		}
		// Drive the PRODUCTION composition path (what the hypothesize launch loop
		// calls), not a re-derivation: a fork in the production logic must fail here.
		got := composeFinderSystemPrompt(persona, l, langs, sweepWide)
		want := finderSystemPrompt(persona, l, langs)
		if got != want {
			t.Errorf("sweep-wide composed prompt differs from finderSystemPrompt output for lens %q (byte identity broken):\ngot:  %q\nwant: %q", name, got, want)
		}
	}
}

// TestSystemPrompt_ContractTraceDeep_ClauseAppended verifies that the
// contract-trace-deep strategy appends its SystemClause after the lens
// manifestation blocks, under the expected heading.
func TestSystemPrompt_ContractTraceDeep_ClauseAppended(t *testing.T) {
	l := lensByName(t, "api-contract-misuse")
	langs := []ingest.Language{ingest.LangGo}
	persona := "senior Go engineer"

	basePrompt := finderSystemPrompt(persona, l, langs)
	// Drive the PRODUCTION composition path, not a re-derivation.
	deepPrompt := composeFinderSystemPrompt(persona, l, langs, contractTraceDeep)

	// The base prompt must be an exact prefix (clause appended, not injected).
	if !strings.HasPrefix(deepPrompt, basePrompt) {
		t.Fatal("deep prompt must have the base prompt as an exact prefix")
	}

	// Everything after the base prompt must be exactly the heading + clause.
	suffix := deepPrompt[len(basePrompt):]
	wantSuffix := "\n\nYOUR SEARCH STRATEGY (contract-trace-deep):\n" + contractTraceDeep.SystemClause
	if suffix != wantSuffix {
		t.Errorf("deep prompt suffix mismatch:\ngot:  %q\nwant: %q", suffix, wantSuffix)
	}

	// The clause must NOT appear in the sweep-wide (base) prompt.
	if strings.Contains(basePrompt, "YOUR SEARCH STRATEGY") {
		t.Error("base (sweep-wide) prompt must NOT contain strategy heading")
	}
}

// TestSystemPrompt_ContractTraceDeep_ClauseContent spot-checks key phrases
// in the SystemClause to ensure spec text was not accidentally truncated.
func TestSystemPrompt_ContractTraceDeep_ClauseContent(t *testing.T) {
	clause := contractTraceDeep.SystemClause
	required := []string{
		"STARTING SEED",
		"find_references",
		"sentinel semantics",
		"Budget your turns for traversal",
	}
	for _, phrase := range required {
		if !strings.Contains(clause, phrase) {
			t.Errorf("SystemClause missing required phrase %q", phrase)
		}
	}
	// Trap: must NOT contain "unlimited" (benchmark honesty: avoid references
	// to the known live bug about zero-sentinel token limits).
	if strings.Contains(strings.ToLower(clause), "unlimited") {
		t.Error("SystemClause must not contain 'unlimited' (benchmark honesty: avoid known live bug language)")
	}
}

// TestSystemPrompt_StateTraceDeep_ClauseAppended verifies that the
// state-trace-deep strategy appends its SystemClause after the lens
// manifestation blocks, under the expected heading — for the lens it applies
// to (concurrency).
func TestSystemPrompt_StateTraceDeep_ClauseAppended(t *testing.T) {
	l := lensByName(t, "concurrency")
	langs := []ingest.Language{ingest.LangGo}
	persona := "senior Go engineer"

	basePrompt := finderSystemPrompt(persona, l, langs)
	// Drive the PRODUCTION composition path, not a re-derivation.
	statePrompt := composeFinderSystemPrompt(persona, l, langs, stateTraceDeep)

	// The base prompt must be an exact prefix (clause appended, not injected).
	if !strings.HasPrefix(statePrompt, basePrompt) {
		t.Fatal("state-trace-deep prompt must have the base prompt as an exact prefix")
	}

	// Everything after the base prompt must be exactly the heading + clause.
	suffix := statePrompt[len(basePrompt):]
	wantSuffix := "\n\nYOUR SEARCH STRATEGY (state-trace-deep):\n" + stateTraceDeep.SystemClause
	if suffix != wantSuffix {
		t.Errorf("state-trace-deep prompt suffix mismatch:\ngot:  %q\nwant: %q", suffix, wantSuffix)
	}

	// The clause must NOT appear in the sweep-wide (base) prompt.
	if strings.Contains(basePrompt, "YOUR SEARCH STRATEGY") {
		t.Error("base (sweep-wide) prompt must NOT contain strategy heading")
	}
}

// TestSystemPrompt_StateTraceDeep_ClauseContent spot-checks key phrases
// in the SystemClause to ensure spec text was not accidentally truncated.
func TestSystemPrompt_StateTraceDeep_ClauseContent(t *testing.T) {
	clause := stateTraceDeep.SystemClause
	required := []string{
		"STARTING SEED",
		"SHARED MUTABLE STATE",
		"ACROSS FILES",
		"sites DISAGREE",
		"Budget your turns for traversal",
	}
	for _, phrase := range required {
		if !strings.Contains(clause, phrase) {
			t.Errorf("state-trace-deep SystemClause missing required phrase %q", phrase)
		}
	}
}

// --- Unit construction -------------------------------------------------------

// TestUnitConstruction_StrategyAxis verifies the (lens × strategy × chunk) unit
// count for a sweep with L lenses and C chunks:
//
//	total = L×C (sweep-wide for all lenses) + C (contract-trace-deep for api-contract-misuse) + 2C (state-trace-deep for concurrency + resource-leaks)
//
// Specifically with C=1 chunk and L=nTaxonomy lenses:
//
//	total = nTaxonomy + 3 (1 contract-trace-deep + 2 state-trace-deep)
func TestUnitConstruction_StrategyAxis(t *testing.T) {
	lenses := BuiltinLenses()
	var taxonomyLenses []Lens
	for _, l := range lenses {
		if l.Name != "diff-intent" {
			taxonomyLenses = append(taxonomyLenses, l)
		}
	}
	L := len(taxonomyLenses)
	files := []string{"a.go", "b.go"} // C=1 chunk (≤ ChunkSize)

	// Count units that would be built for a sweep (no diff-intent).
	strategies := builtinStrategies()
	var units int
	for _, l := range taxonomyLenses {
		for _, s := range strategies {
			if s.AppliesTo(l.Name) {
				units++
			}
		}
	}
	_ = files

	// Expect L wide units + 1 contract-trace-deep + 2 state-trace-deep.
	wantUnits := L + 3
	if units != wantUnits {
		t.Errorf("unit count = %d, want %d (L=%d wide + 1 contract-trace-deep + 2 state-trace-deep)", units, wantUnits, L)
	}
}

// TestUnitConstruction_DeepOnlyForApiContract verifies that contract-trace-deep
// applies ONLY to api-contract-misuse, not to any other lens.
func TestUnitConstruction_DeepOnlyForApiContract(t *testing.T) {
	for _, l := range BuiltinLenses() {
		applies := contractTraceDeep.AppliesTo(l.Name)
		if l.Name == "api-contract-misuse" {
			if !applies {
				t.Errorf("contractTraceDeep.AppliesTo(%q) = false, want true", l.Name)
			}
		} else {
			if applies {
				t.Errorf("contractTraceDeep.AppliesTo(%q) = true, want false (deep is api-contract-misuse only in v1)", l.Name)
			}
		}
	}
}

// TestUnitConstruction_StateTraceDeep_TargetLenses verifies that
// state-trace-deep applies ONLY to concurrency and resource-leaks, and NOT
// to any other lens (including api-contract-misuse, which is contract-trace-deep
// territory).
func TestUnitConstruction_StateTraceDeep_TargetLenses(t *testing.T) {
	stateTraceDeepTargets := map[string]bool{
		"concurrency":    true,
		"resource-leaks": true,
	}
	for _, l := range BuiltinLenses() {
		applies := stateTraceDeep.AppliesTo(l.Name)
		want := stateTraceDeepTargets[l.Name]
		if applies != want {
			t.Errorf("stateTraceDeep.AppliesTo(%q) = %v, want %v (state-trace-deep targets concurrency and resource-leaks only)", l.Name, applies, want)
		}
	}
}

// TestUnitConstruction_DiffIntentNoDeep verifies that diff-intent receives no
// deep strategy units. diff-intent is special: it never enters the per-lens
// chunk loop (handled separately in hypothesize), so AppliesTo is never called
// for it in the normal path. This test makes the invariant explicit for BOTH
// non-default strategies.
func TestUnitConstruction_DiffIntentNoDeep(t *testing.T) {
	// contractTraceDeep.AppliesTo("diff-intent") must return false.
	if contractTraceDeep.AppliesTo("diff-intent") {
		t.Error("contractTraceDeep.AppliesTo(diff-intent) = true; diff-intent must never get deep units")
	}
	// stateTraceDeep.AppliesTo("diff-intent") must return false.
	if stateTraceDeep.AppliesTo("diff-intent") {
		t.Error("stateTraceDeep.AppliesTo(diff-intent) = true; diff-intent must never get deep units")
	}
	// sweepWide.AppliesTo("diff-intent") must return true (it always returns true),
	// which is correct: diff-intent's single custom-task unit uses sweep-wide.
	if !sweepWide.AppliesTo("diff-intent") {
		t.Error("sweepWide.AppliesTo(diff-intent) = false; sweepWide applies to all lenses by definition")
	}
}

// --- Degradation -------------------------------------------------------------

// TestDegradation_DeepShedBeforeWide verifies that under budget pressure, the
// deep unit for a lens is shed BEFORE that lens's wide unit (weight 0.9 < 1.0
// means the deep class ranks below the wide class for the same lens).
func TestDegradation_DeepShedBeforeWide(t *testing.T) {
	// Construct a scenario with enough lenses to overflow degradedLensCount (2).
	// Simulate: all taxonomy lenses (sweep-wide) + api-contract-misuse (deep).
	// Expected: deep is shed before wide for equal-yield considerations.
	//
	// For Go: api-contract-misuse yield = 50.
	//   api-contract-misuse@sweep-wide score = 50 × 1.0 = 50
	//   api-contract-misuse@contract-trace-deep score = 50 × 0.9 = 45
	//
	// Top 2 on Go sweep: nil-safety@sweep-wide(100) and concurrency@sweep-wide(90).
	// Both api-contract-misuse classes rank below them and are both shed.
	// But the deep class (45) ranks BELOW the wide class (50).
	langs := []ingest.Language{ingest.LangGo}
	classes := sweepActiveClasses(lensesByYield(BuiltinLenses(), langs), langs)
	// Add the deep unit for api-contract-misuse.
	classes = append(classes, lensStrategyClass{
		lensName:     "api-contract-misuse",
		strategyName: contractTraceDeep.Name,
		weight:       contractTraceDeep.Weight,
	})

	survivors := degradedUnitClasses(classes, langs)

	// Survivors should be nil-safety@sweep-wide and concurrency@sweep-wide.
	if !survivors["nil-safety/error-handling@sweep-wide"] {
		t.Error("nil-safety/error-handling@sweep-wide must survive degradation on Go (yield 100)")
	}
	if !survivors["concurrency@sweep-wide"] {
		t.Error("concurrency@sweep-wide must survive degradation on Go (yield 90)")
	}

	// Both api-contract-misuse classes are shed.
	if survivors["api-contract-misuse@sweep-wide"] {
		t.Error("api-contract-misuse@sweep-wide should be shed (yield 50, below top 2)")
	}
	if survivors["api-contract-misuse@contract-trace-deep"] {
		t.Error("api-contract-misuse@contract-trace-deep should be shed (yield 45, lowest of the api-contract classes)")
	}

	// When only api-contract-misuse is an active lens (narrow run), wide survives
	// before deep: both are below top-2 threshold, but if degradedLensCount were 1,
	// wide (50) would beat deep (45).
	narrowClasses := []lensStrategyClass{
		{lensName: "api-contract-misuse", strategyName: "sweep-wide", weight: 1.0},
		{lensName: "api-contract-misuse", strategyName: contractTraceDeep.Name, weight: contractTraceDeep.Weight},
	}
	narrowSurvivors := degradedUnitClasses(narrowClasses, langs)
	// With degradedLensCount=2 and only 2 classes, both survive.
	if !narrowSurvivors["api-contract-misuse@sweep-wide"] || !narrowSurvivors["api-contract-misuse@contract-trace-deep"] {
		t.Errorf("when only api-contract-misuse classes exist and count≤2, both survive: %v", narrowSurvivors)
	}
}

// TestDegradation_SweepWideSameAsPreStrategy verifies the CRITICAL INVARIANT:
// with only sweep-wide units (no deep-admitting lens in the active set, or
// only sweepWide strategy), the survivors must equal exactly what the old
// lens-only degradation would have produced — same lens names, same count.
func TestDegradation_SweepWideSameAsPreStrategy(t *testing.T) {
	langs := []ingest.Language{ingest.LangGo}
	// Simulate a run where no lens admits a deep strategy (e.g. api-contract-misuse
	// is not in the active set).
	onlyWideLenses := []Lens{}
	for _, l := range lensesByYield(BuiltinLenses(), langs) {
		if l.Name != "diff-intent" && l.Name != "api-contract-misuse" {
			onlyWideLenses = append(onlyWideLenses, l)
		}
	}

	classes := sweepActiveClasses(onlyWideLenses, langs)
	survivors := degradedUnitClasses(classes, langs)

	// Must keep the top-2 by yield (on Go: nil-safety and concurrency).
	if len(survivors) != degradedLensCount {
		t.Errorf("survivors count = %d, want %d (same as pre-strategy degradation)", len(survivors), degradedLensCount)
	}
	if !survivors["nil-safety/error-handling@sweep-wide"] {
		t.Error("nil-safety/error-handling@sweep-wide must survive (top-1 Go yield)")
	}
	if !survivors["concurrency@sweep-wide"] {
		t.Error("concurrency@sweep-wide must survive (top-2 Go yield)")
	}
}

// TestDegradation_SortStable verifies that the degradation ranking is stable and
// deterministic: equal-score entries do not reorder between calls.
func TestDegradation_SortStable(t *testing.T) {
	// Two unit-classes with the same effective yield.
	classes := []lensStrategyClass{
		{lensName: "boundary-conditions", strategyName: "sweep-wide", weight: 1.0},
		{lensName: "api-contract-misuse", strategyName: "sweep-wide", weight: 1.0},
	}
	langs := []ingest.Language{ingest.LangGo}

	// Run multiple times; result must be identical every time.
	first := degradedUnitClasses(classes, langs)
	for i := 0; i < 10; i++ {
		got := degradedUnitClasses(classes, langs)
		if len(got) != len(first) {
			t.Fatalf("run %d: survivor count changed (%d → %d)", i, len(first), len(got))
		}
		for k := range first {
			if !got[k] {
				t.Errorf("run %d: key %q was in first result but not in run %d", i, k, i)
			}
		}
	}
}

// TestUnitLabel_DefaultStrategy verifies that the progress label for a
// sweep-wide unit is the bare lens name (no "@strategy" suffix) to preserve
// existing output format.
func TestUnitLabel_DefaultStrategy(t *testing.T) {
	got := unitLabel("nil-safety/error-handling", sweepWide.Name)
	want := "nil-safety/error-handling"
	if got != want {
		t.Errorf("unitLabel(lens, sweep-wide) = %q, want %q (bare lens name for default strategy)", got, want)
	}
}

// TestUnitLabel_NonDefaultStrategy verifies that a non-default strategy uses
// "lens@strategy" format so the deep unit is distinguishable in progress events.
func TestUnitLabel_NonDefaultStrategy(t *testing.T) {
	for _, c := range []struct {
		lens, strategy, want string
	}{
		{"api-contract-misuse", contractTraceDeep.Name, "api-contract-misuse@contract-trace-deep"},
		{"concurrency", stateTraceDeep.Name, "concurrency@state-trace-deep"},
		{"resource-leaks", stateTraceDeep.Name, "resource-leaks@state-trace-deep"},
	} {
		got := unitLabel(c.lens, c.strategy)
		if got != c.want {
			t.Errorf("unitLabel(%q, %q) = %q, want %q", c.lens, c.strategy, got, c.want)
		}
	}
}

// TestBuildUnits_ChunkMajorInterleave verifies the launch-order policy: every
// active lens (and applicable strategy) visits chunk 0 before any lens visits
// chunk 1, so low-yield lenses get running coverage within the first chunks
// instead of after every higher-yield lens has covered the whole repo.
func TestBuildUnits_ChunkMajorInterleave(t *testing.T) {
	lensA := lensByName(t, "nil-safety/error-handling") // no deep strategy
	lensB := lensByName(t, "api-contract-misuse")       // admits contract-trace-deep
	chunks := []fileChunk{
		{files: []string{"a1.go", "a2.go"}, langs: []ingest.Language{ingest.LangGo}},
		{files: []string{"b1.go"}, langs: []ingest.Language{ingest.LangGo}},
	}

	units := buildUnits([]Lens{lensA, lensB}, builtinStrategies(), chunks, nil)

	type key struct{ lens, strategy, firstFile string }
	got := make([]key, len(units))
	for i, u := range units {
		got[i] = key{u.lens.Name, u.strategy.Name, u.files[0]}
	}
	want := []key{
		// chunk 0: all lenses × applicable strategies
		{"nil-safety/error-handling", "sweep-wide", "a1.go"},
		{"api-contract-misuse", "sweep-wide", "a1.go"},
		{"api-contract-misuse", "contract-trace-deep", "a1.go"},
		// chunk 1: same lens sequence again
		{"nil-safety/error-handling", "sweep-wide", "b1.go"},
		{"api-contract-misuse", "sweep-wide", "b1.go"},
		{"api-contract-misuse", "contract-trace-deep", "b1.go"},
	}
	if len(got) != len(want) {
		t.Fatalf("unit count = %d, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("units[%d] = %v, want %v (chunk-major interleave broken)", i, got[i], want[i])
		}
	}
}

// TestBuildUnits_DiffIntentExcluded verifies diff-intent never receives
// chunk-based units regardless of position in the lens slice.
func TestBuildUnits_DiffIntentExcluded(t *testing.T) {
	chunks := []fileChunk{{files: []string{"x.go"}, langs: []ingest.Language{ingest.LangGo}}}
	units := buildUnits([]Lens{diffIntentLens(), lensByName(t, "concurrency")}, builtinStrategies(), chunks, nil)
	for _, u := range units {
		if u.lens.Name == "diff-intent" {
			t.Fatalf("diff-intent received a chunk unit: %+v", u)
		}
	}
	// concurrency now admits BOTH sweep-wide and state-trace-deep — 2 units.
	if len(units) != 2 {
		t.Fatalf("unit count = %d, want 2 (concurrency sweep-wide + concurrency state-trace-deep)", len(units))
	}
}
