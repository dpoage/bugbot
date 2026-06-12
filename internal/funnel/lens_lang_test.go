package funnel

import (
	"reflect"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/ingest"
)

// builtinLensNames pins the Name of every builtin lens byte-for-byte. Names
// are part of the dedup fingerprint and the eval harness's routing key, so the
// core/manifestation split must not touch them.
func TestBuiltinLensNames_Stable(t *testing.T) {
	want := []string{
		"nil-safety/error-handling",
		"diff-intent",
		"concurrency",
		"resource-leaks",
		"boundary-conditions",
		"api-contract-misuse",
		"injection/input-validation",
	}
	lenses := BuiltinLenses()
	if len(lenses) != len(want) {
		t.Fatalf("BuiltinLenses() = %d lenses, want %d", len(lenses), len(want))
	}
	for i, l := range lenses {
		if l.Name != want[i] {
			t.Errorf("lens[%d].Name = %q, want %q (names are fingerprint/routing keys)", i, l.Name, want[i])
		}
	}
}

// TestFinderSystemPrompt_PythonOnly_NoGoIdioms: a Python-only chunk composes
// the Python manifestations and carries ZERO Go idioms, for every builtin lens.
func TestFinderSystemPrompt_PythonOnly_NoGoIdioms(t *testing.T) {
	goIdioms := []string{"goroutine", "comma-ok", "WaitGroup", "How this manifests in Go"}
	for _, l := range BuiltinLenses() {
		if l.Name == "diff-intent" {
			continue // language-free, Core-only: no manifestation blocks by design
		}
		p := finderSystemPrompt("senior Python engineer", l, []ingest.Language{ingest.LangPython})
		if !strings.Contains(p, "How this manifests in Python:") {
			t.Errorf("lens %q: Python-only prompt missing the Python manifestation block", l.Name)
		}
		for _, row := range manifestations[l.Name][ingest.LangPython] {
			if !strings.Contains(p, row) {
				t.Errorf("lens %q: Python-only prompt missing Python row %q", l.Name, row)
			}
		}
		for _, idiom := range goIdioms {
			// Case-insensitive so a future recased leak still trips the assertion
			// (mirrors the Go-side test below).
			if strings.Contains(strings.ToLower(p), strings.ToLower(idiom)) {
				t.Errorf("lens %q: Python-only prompt leaks Go idiom %q", l.Name, idiom)
			}
		}
	}
}

// TestFinderSystemPrompt_GoOnly_FullGoContent: a Go-only chunk must carry the
// lens Core plus every Go manifestation row (the original Specializations'
// hunting guidance, reorganized but not weakened) and ZERO Python idioms.
func TestFinderSystemPrompt_GoOnly_FullGoContent(t *testing.T) {
	pyIdioms := []string{"asyncio", "un-awaited", "How this manifests in Python"}
	for _, l := range BuiltinLenses() {
		if l.Name == "diff-intent" {
			continue // language-free, Core-only: no manifestation blocks by design
		}
		p := finderSystemPrompt("senior Go engineer", l, []ingest.Language{ingest.LangGo})
		if !strings.Contains(p, l.Core) {
			t.Errorf("lens %q: Go-only prompt missing the universal Core", l.Name)
		}
		if !strings.Contains(p, "How this manifests in Go:") {
			t.Errorf("lens %q: Go-only prompt missing the Go manifestation block", l.Name)
		}
		for _, row := range manifestations[l.Name][ingest.LangGo] {
			if !strings.Contains(p, row) {
				t.Errorf("lens %q: Go-only prompt missing Go row %q", l.Name, row)
			}
		}
		for _, idiom := range pyIdioms {
			if strings.Contains(strings.ToLower(p), strings.ToLower(idiom)) {
				t.Errorf("lens %q: Go-only prompt leaks Python idiom %q", l.Name, idiom)
			}
		}
	}

	// Spot-pin distinctive clauses of the ORIGINAL Go Specializations so the
	// split can never silently drop today's Go hunting guidance.
	anchors := map[string]string{
		"nil-safety/error-handling":  "comma-ok",
		"concurrency":                "WaitGroup Add/Done imbalances",
		"resource-leaks":             "HTTP response body",
		"boundary-conditions":        "[0] or [len-1]",
		"api-contract-misuse":        "time.After in a hot loop",
		"injection/input-validation": "actually untrusted and unvalidated",
	}
	for _, l := range BuiltinLenses() {
		if l.Name == "diff-intent" {
			continue // change-scoped lens: no historical Go Specialization to anchor
		}
		p := finderSystemPrompt("senior Go engineer", l, []ingest.Language{ingest.LangGo})
		if !strings.Contains(p, anchors[l.Name]) {
			t.Errorf("lens %q: Go-only prompt lost original guidance anchor %q", l.Name, anchors[l.Name])
		}
	}
}

// TestFinderSystemPrompt_MixedChunk_UnionBlocks: a mixed Go+Python chunk gets
// BOTH manifestation blocks.
func TestFinderSystemPrompt_MixedChunk_UnionBlocks(t *testing.T) {
	l := lensByName(t, "concurrency") // the starkest Go-vs-Python contrast
	p := finderSystemPrompt("senior software engineer with deep Go and Python expertise", l,
		[]ingest.Language{ingest.LangGo, ingest.LangPython})
	if !strings.Contains(p, "How this manifests in Go:") {
		t.Error("mixed chunk missing the Go block")
	}
	if !strings.Contains(p, "How this manifests in Python:") {
		t.Error("mixed chunk missing the Python block")
	}
	if !strings.Contains(p, "goroutines") || !strings.Contains(p, "asyncio") {
		t.Error("mixed chunk must union both languages' rows")
	}
}

// TestFinderSystemPrompt_CoreOnlyLens: a lens with no manifestation entries at
// all (e.g. a language-free, commit-intent lens) composes Core alone, with no
// manifestation scaffolding — first-class, not an error.
func TestFinderSystemPrompt_CoreOnlyLens(t *testing.T) {
	l := Lens{Name: "diff-intent", Core: "Hunt for changes whose code contradicts the commit's stated intent."}
	p := finderSystemPrompt("senior Go engineer", l, []ingest.Language{ingest.LangGo, ingest.LangPython})
	if !strings.Contains(p, "YOUR ASSIGNED FOCUS (diff-intent):\n"+l.Core) {
		t.Error("core-only lens must compose base + focus + Core verbatim")
	}
	if strings.Contains(p, "How this manifests in") {
		t.Error("core-only lens must not emit manifestation blocks")
	}
}

// TestFinderSystemPrompt_DataDriven_NewLanguageRow: adding a language column to
// the manifestations table makes it appear in composed prompts with NO change
// to composition code. (Mutating the package-level registry is the test seam.)
func TestFinderSystemPrompt_DataDriven_NewLanguageRow(t *testing.T) {
	const fakeLang = ingest.Language("fortran")
	const fakeRow = "GOTO-based error handling that skips the cleanup section."
	manifestations["concurrency"][fakeLang] = []string{fakeRow}
	t.Cleanup(func() { delete(manifestations["concurrency"], fakeLang) })

	p := finderSystemPrompt("senior software engineer", lensByName(t, "concurrency"), []ingest.Language{fakeLang})
	if !strings.Contains(p, "How this manifests in fortran:") {
		t.Error("new language column did not produce a manifestation block (composition is not data-driven)")
	}
	if !strings.Contains(p, fakeRow) {
		t.Error("new language row missing from composed prompt")
	}
}

// TestFinderSystemPrompt_JSAndTSMergeBlocks: JavaScript and TypeScript share
// row tables, so a mixed JS/TS chunk renders ONE merged block, not two copies.
func TestFinderSystemPrompt_JSAndTSMergeBlocks(t *testing.T) {
	l := lensByName(t, "nil-safety/error-handling")
	p := finderSystemPrompt("senior software engineer", l,
		[]ingest.Language{ingest.LangJavaScript, ingest.LangTypeScript})
	if got := strings.Count(p, "How this manifests in"); got != 1 {
		t.Errorf("JS+TS chunk rendered %d manifestation blocks, want 1 merged block", got)
	}
	if !strings.Contains(p, "How this manifests in JavaScript/TypeScript:") {
		t.Error("merged block must name both languages")
	}
}

// TestEffectiveYield_GoColumnPreserved pins the exact historical Go yields:
// the refactor must not change Go-repo degradation behavior.
func TestEffectiveYield_GoColumnPreserved(t *testing.T) {
	want := map[string]int{
		"nil-safety/error-handling":  100,
		"concurrency":                90,
		"resource-leaks":             80,
		"boundary-conditions":        60,
		"api-contract-misuse":        50,
		"injection/input-validation": 40,
	}
	for name, y := range want {
		if got := effectiveYield(name, []ingest.Language{ingest.LangGo}); got != y {
			t.Errorf("effectiveYield(%q, go) = %d, want %d (historical Go yield)", name, got, y)
		}
	}
}

// TestEffectiveYield_Resolution covers max-over-dominant-languages, the
// anyLanguage default for unlisted languages, empty language sets, and lenses
// absent from the table entirely.
func TestEffectiveYield_Resolution(t *testing.T) {
	// Max over dominant languages: a Go+Python repo keeps concurrency at the Go
	// column (90), not the Python column (45).
	if got := effectiveYield("concurrency", []ingest.Language{ingest.LangPython, ingest.LangGo}); got != 90 {
		t.Errorf("max-over-languages: concurrency(go+python) = %d, want 90", got)
	}
	// A language without a column falls back to the anyLanguage default.
	if got := effectiveYield("concurrency", []ingest.Language{ingest.LangRuby}); got != lensYields["concurrency"][anyLanguage] {
		t.Errorf("default fallback: concurrency(ruby) = %d, want anyLanguage column %d",
			got, lensYields["concurrency"][anyLanguage])
	}
	// No detectable dominant language resolves to the default column.
	if got := effectiveYield("concurrency", nil); got != lensYields["concurrency"][anyLanguage] {
		t.Errorf("empty langs: concurrency() = %d, want anyLanguage column", got)
	}
	// A lens with no yield row at all (e.g. an in-flight lens added on another
	// branch before its row lands) gets the unlisted default, not zero.
	if got := effectiveYield("no-such-lens", []ingest.Language{ingest.LangGo}); got != unlistedLensYield {
		t.Errorf("unlisted lens yield = %d, want %d", got, unlistedLensYield)
	}
	// A lens with ONLY a default column (the language-free shape, e.g.
	// diff-intent) resolves to that value for every language mix.
	lensYields["core-only-lens"] = map[ingest.Language]int{anyLanguage: 95}
	t.Cleanup(func() { delete(lensYields, "core-only-lens") })
	for _, langs := range [][]ingest.Language{nil, {ingest.LangGo}, {ingest.LangPython, ingest.LangCPP}} {
		if got := effectiveYield("core-only-lens", langs); got != 95 {
			t.Errorf("default-only lens yield(%v) = %d, want 95", langs, got)
		}
	}
}

// sweepActiveClasses converts a yield-ordered lens slice (pre-filtered to
// lenses that emitted units on this sweep) to lensStrategyClass values for
// degradedUnitClasses, using sweep-wide (weight 1.0) as the strategy. This
// mirrors what hypothesize builds when only the default strategy is in play.
func sweepActiveClasses(ordered []Lens) []lensStrategyClass {
	var out []lensStrategyClass
	for _, l := range ordered {
		if l.Name == "diff-intent" {
			continue // change-scoped lens: zero chunk units on sweeps
		}
		out = append(out, lensStrategyClass{
			lensName:     l.Name,
			strategyName: sweepWide.Name,
			weight:       sweepWide.Weight,
		})
	}
	return out
}

// TestDegradation_LanguageDependentLensSet: under budget pressure a
// Python-heavy repo keeps a different lens set than a Go repo — degradation
// sheds the lenses that are low-yield for THIS repo's language mix.
// The surviving keys are "lens@sweep-wide" (the unit-class key format).
func TestDegradation_LanguageDependentLensSet(t *testing.T) {
	goLangs := []ingest.Language{ingest.LangGo}
	pyLangs := []ingest.Language{ingest.LangPython}
	goKeep := degradedUnitClasses(sweepActiveClasses(lensesByYield(BuiltinLenses(), goLangs)), goLangs)
	pyKeep := degradedUnitClasses(sweepActiveClasses(lensesByYield(BuiltinLenses(), pyLangs)), pyLangs)

	if !goKeep["nil-safety/error-handling@sweep-wide"] || !goKeep["concurrency@sweep-wide"] {
		t.Errorf("Go-profile degraded set = %v, want {nil-safety/error-handling@sweep-wide, concurrency@sweep-wide} (historical behavior)", goKeep)
	}
	if !pyKeep["nil-safety/error-handling@sweep-wide"] || !pyKeep["injection/input-validation@sweep-wide"] {
		t.Errorf("Python-profile degraded set = %v, want {nil-safety/error-handling@sweep-wide, injection/input-validation@sweep-wide}", pyKeep)
	}
	if reflect.DeepEqual(goKeep, pyKeep) {
		t.Error("Go and Python profiles degrade to the same lens set; yields are not language-dependent")
	}
}

// TestLensesByYield_StableAndComplete: reordering is a permutation (no lens
// gained or lost) and equal yields keep builtin order (deterministic prompts
// and launch order).
func TestLensesByYield_StableAndComplete(t *testing.T) {
	in := BuiltinLenses()
	out := lensesByYield(in, []ingest.Language{ingest.LangPython})
	if len(out) != len(in) {
		t.Fatalf("lensesByYield changed lens count: %d -> %d", len(in), len(out))
	}
	seen := map[string]bool{}
	for _, l := range out {
		seen[l.Name] = true
	}
	for _, l := range in {
		if !seen[l.Name] {
			t.Errorf("lens %q lost in reordering", l.Name)
		}
	}
	for i := 1; i < len(out); i++ {
		yi := effectiveYield(out[i-1].Name, []ingest.Language{ingest.LangPython})
		yj := effectiveYield(out[i].Name, []ingest.Language{ingest.LangPython})
		if yi < yj {
			t.Errorf("lens order not descending by effective yield at %d: %q(%d) before %q(%d)",
				i, out[i-1].Name, yi, out[i].Name, yj)
		}
	}
}

// TestChunkByLanguage_HomogeneousWherePossible: files group by language before
// chunking, full chunks are single-language, and only the leftover tails form
// a mixed chunk carrying the language union.
func TestChunkByLanguage_HomogeneousWherePossible(t *testing.T) {
	files := []string{"a.go", "x.py", "b.go", "c.go", "y.py", "z.py"}
	chunks := chunkByLanguage(files, 2)

	// 3 go + 3 py at size 2: one full go chunk, one full py chunk, and one mixed
	// remainder of the two tails.
	if len(chunks) != 3 {
		t.Fatalf("chunks = %d, want 3 (full go, full py, mixed remainder); got %+v", len(chunks), chunks)
	}
	if !reflect.DeepEqual(chunks[0].files, []string{"a.go", "b.go"}) ||
		!reflect.DeepEqual(chunks[0].langs, []ingest.Language{ingest.LangGo}) {
		t.Errorf("chunk[0] = %+v, want homogeneous go chunk [a.go b.go] in input order", chunks[0])
	}
	if !reflect.DeepEqual(chunks[1].files, []string{"x.py", "y.py"}) ||
		!reflect.DeepEqual(chunks[1].langs, []ingest.Language{ingest.LangPython}) {
		t.Errorf("chunk[1] = %+v, want homogeneous python chunk [x.py y.py]", chunks[1])
	}
	if !reflect.DeepEqual(chunks[2].files, []string{"c.go", "z.py"}) ||
		!reflect.DeepEqual(chunks[2].langs, []ingest.Language{ingest.LangGo, ingest.LangPython}) {
		t.Errorf("chunk[2] = %+v, want mixed remainder [c.go z.py] with union langs", chunks[2])
	}

	// Every input file appears exactly once across chunks.
	count := map[string]int{}
	for _, c := range chunks {
		for _, f := range c.files {
			count[f]++
		}
	}
	for _, f := range files {
		if count[f] != 1 {
			t.Errorf("file %q appears %d times across chunks, want exactly 1", f, count[f])
		}
	}
}

// TestChunkByLanguage_EdgeCases covers the single-chunk fast paths and the
// chunk-size cap.
func TestChunkByLanguage_EdgeCases(t *testing.T) {
	if got := chunkByLanguage(nil, 4); got != nil {
		t.Errorf("empty input: got %v, want nil", got)
	}
	// Non-positive size: one chunk of everything, langs = union.
	all := chunkByLanguage([]string{"a.go", "x.py"}, 0)
	if len(all) != 1 || !reflect.DeepEqual(all[0].langs, []ingest.Language{ingest.LangGo, ingest.LangPython}) {
		t.Errorf("size<=0: got %+v, want single chunk with union langs", all)
	}
	// len <= size: one chunk, original order preserved.
	one := chunkByLanguage([]string{"x.py", "a.go"}, 8)
	if len(one) != 1 || !reflect.DeepEqual(one[0].files, []string{"x.py", "a.go"}) {
		t.Errorf("len<=size: got %+v, want single chunk in input order", one)
	}
	// Every chunk respects the size cap.
	files := []string{"a.go", "b.go", "c.go", "d.py", "e.py", "f.rs", "g.md"}
	for _, c := range chunkByLanguage(files, 2) {
		if len(c.files) > 2 {
			t.Errorf("chunk %+v exceeds size cap 2", c)
		}
	}
}

// TestFinderSystemPrompt_JSAndTSSplitWhenUnequal pins the inverse of the merge
// behavior: if the JS and TS row slices ever diverge in content, the composed
// prompt must render two separate blocks rather than silently merging them.
func TestFinderSystemPrompt_JSAndTSSplitWhenUnequal(t *testing.T) {
	manifestations["split-test-lens"] = map[ingest.Language][]string{
		ingest.LangJavaScript: {"a JS-only manifestation row"},
		ingest.LangTypeScript: {"a TS-only manifestation row"},
	}
	t.Cleanup(func() { delete(manifestations, "split-test-lens") })

	l := Lens{Name: "split-test-lens", Core: "core text"}
	p := finderSystemPrompt("senior engineer", l, []ingest.Language{ingest.LangJavaScript, ingest.LangTypeScript})
	if !strings.Contains(p, "How this manifests in JavaScript:") ||
		!strings.Contains(p, "How this manifests in TypeScript:") {
		t.Errorf("unequal JS/TS rows must render two separate blocks, got:\n%s", p)
	}
	if strings.Contains(p, "JavaScript/TypeScript") {
		t.Errorf("unequal JS/TS rows must not render a merged block, got:\n%s", p)
	}
}

// TestChunkByLanguage_GlobalHeatOrderAtChunkGranularity pins the heat-ordering
// repair: with heat-ordered input interleaving two languages, the chunk
// containing the globally hottest file of the SECOND language must not be
// deferred behind every chunk of the first language group.
func TestChunkByLanguage_GlobalHeatOrderAtChunkGranularity(t *testing.T) {
	// Heat order: hottest first. go1 (rank 0), py1 (rank 1), then cold gos.
	files := []string{"hot1.go", "hot2.py", "cold3.go", "cold4.go", "cold5.go", "cold6.go"}
	chunks := chunkByLanguage(files, 2)
	if len(chunks) == 0 {
		t.Fatal("no chunks")
	}
	// First chunk carries the globally hottest file.
	first := chunks[0].files
	foundHot1 := false
	for _, f := range first {
		if f == "hot1.go" {
			foundHot1 = true
		}
	}
	if !foundHot1 {
		t.Errorf("first chunk %v must contain the globally hottest file hot1.go", first)
	}
	// The chunk containing hot2.py (rank 1) must come before the chunk whose
	// hottest member is cold4.go (rank 3). cold3.go is not a valid probe: it
	// rides in the same chunk as hot1.go, whose hottest-member rank is 0.
	pos := func(name string) int {
		for i, c := range chunks {
			for _, f := range c.files {
				if f == name {
					return i
				}
			}
		}
		return -1
	}
	if pos("hot2.py") > pos("cold4.go") {
		t.Errorf("chunk order defers hot2.py (rank 1) behind cold4.go's chunk (hottest rank 3): %v", chunks)
	}
}

// lensByName fetches a builtin lens by Name; index-based selection broke when
// diff-intent joined the table.
func lensByName(t *testing.T, name string) Lens {
	t.Helper()
	for _, l := range BuiltinLenses() {
		if l.Name == name {
			return l
		}
	}
	t.Fatalf("lens %q not found", name)
	return Lens{}
}
