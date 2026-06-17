package funnel

// Test files for bugbot-mi5.5: defect-class lenses (memory-safety,
// exception-safety, dynamic-typing) plus the per-lens language gate that
// makes them activate only on applicable languages. These tests pin the
// machine-readable contract: lens names, Languages sets, the lensAppliesTo
// helper, buildUnits gating, lensesByYield ordering on a C++ repo, and the
// composition path that renders (or does not render) the per-language
// manifestation block.

import (
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/ingest"
)

// TestBuiltinLenses_DefectClassNames: the three new defect-class lenses
// exist in BuiltinLenses with their stable names. Names are part of the
// dedup fingerprint and the eval harness's routing key, so they MUST be
// byte-for-byte stable.
func TestBuiltinLenses_DefectClassNames(t *testing.T) {
	want := map[string]bool{
		"memory-safety":    false,
		"exception-safety": false,
		"dynamic-typing":   false,
	}
	for _, l := range BuiltinLenses() {
		if _, ok := want[l.Name]; ok {
			want[l.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("BuiltinLenses() missing %q", name)
		}
	}
}

// TestBuiltinLenses_DefectClassLanguages: each defect-class lens declares
// the exact language set it applies to, per the design contract. Other
// (non-defect-class) lenses must NOT set Languages — they remain
// language-free so the gate preserves their byte-for-byte behavior.
func TestBuiltinLenses_DefectClassLanguages(t *testing.T) {
	want := map[string][]ingest.Language{
		"memory-safety":    {ingest.LangCPP, ingest.LangC, ingest.LangRust},
		"exception-safety": {ingest.LangPython, ingest.LangCPP, ingest.LangJavaScript, ingest.LangTypeScript},
		"dynamic-typing":   {ingest.LangPython, ingest.LangJavaScript, ingest.LangTypeScript},
	}
	for _, l := range BuiltinLenses() {
		w, ok := want[l.Name]
		if !ok {
			// not a defect-class lens; Languages must be nil/empty so the
			// gate preserves the language-free default.
			if len(l.Languages) != 0 {
				t.Errorf("non-defect lens %q has Languages set: %v (must be nil/empty to remain language-free)", l.Name, l.Languages)
			}
			continue
		}
		// The defect lens must declare its Languages set EXACTLY (no
		// reordering, no extras) — the gate uses the set as a fingerprint.
		if len(l.Languages) != len(w) {
			t.Fatalf("lens %q: Languages = %v, want %v (set must match the design contract exactly)", l.Name, l.Languages, w)
		}
		have := make(map[ingest.Language]bool, len(l.Languages))
		for _, lang := range l.Languages {
			have[lang] = true
		}
		for _, lang := range w {
			if !have[lang] {
				t.Errorf("lens %q: Languages = %v, want %v (missing %s)", l.Name, l.Languages, w, lang)
			}
		}
	}
	// Every expected defect lens must exist.
	for name := range want {
		found := false
		for _, l := range BuiltinLenses() {
			if l.Name == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("BuiltinLenses() missing %q", name)
		}
	}
}

// TestLensAppliesTo_LanguageFree: a lens with nil/empty Languages applies
// to every language (preserves the byte-for-byte behavior of every existing
// language-free builtin). This is the load-bearing default.
func TestLensAppliesTo_LanguageFree(t *testing.T) {
	l := Lens{Name: "any-lens", Core: "core text"} // no Languages field
	allLangs := []ingest.Language{
		ingest.LangGo, ingest.LangPython, ingest.LangCPP, ingest.LangC,
		ingest.LangRust, ingest.LangJavaScript, ingest.LangTypeScript,
		ingest.LangOther,
	}
	for _, langs := range [][]ingest.Language{nil, {}, allLangs, {ingest.LangGo}} {
		if !lensAppliesTo(l, langs) {
			t.Errorf("language-free lens must apply to langs=%v", langs)
		}
	}
}

// TestLensAppliesTo_Intersection: a lens with a non-empty Languages list
// applies iff at least one declared language is present in the chunk's
// language set. Intersection is the correct semantic — the lens "applies"
// to a chunk whose language mix overlaps its declared set.
func TestLensAppliesTo_Intersection(t *testing.T) {
	l := Lens{
		Name:      "memory-safety",
		Core:      "core text",
		Languages: []ingest.Language{ingest.LangCPP, ingest.LangC, ingest.LangRust},
	}
	cases := []struct {
		name  string
		langs []ingest.Language
		want  bool
	}{
		{"empty chunk langs", nil, false},
		{"empty slice", []ingest.Language{}, false},
		{"LangGo only", []ingest.Language{ingest.LangGo}, false},
		{"LangPython only", []ingest.Language{ingest.LangPython}, false},
		{"LangCPP only", []ingest.Language{ingest.LangCPP}, true},
		{"LangC only", []ingest.Language{ingest.LangC}, true},
		{"LangRust only", []ingest.Language{ingest.LangRust}, true},
		{"LangGo + LangCPP", []ingest.Language{ingest.LangGo, ingest.LangCPP}, true},
		{"LangGo + LangPython", []ingest.Language{ingest.LangGo, ingest.LangPython}, false},
		{"LangJava + LangJavaScript", []ingest.Language{ingest.LangJava, ingest.LangJavaScript}, false},
	}
	for _, c := range cases {
		if got := lensAppliesTo(l, c.langs); got != c.want {
			t.Errorf("lensAppliesTo(memory-safety, %v) = %v, want %v (%s)", c.langs, got, c.want, c.name)
		}
	}
}

// TestBuildUnits_GatesDefectLensesOnGo: a pure Go chunk emits ZERO units
// for the three new defect-class lenses (Go's runtime rules out every
// defect class — GC prevents UAF, error channel sidesteps exceptions,
// static types prevent dynamic-typing bugs). Existing language-free lenses
// are unchanged (one unit each, same as before the gate).
func TestBuildUnits_GatesDefectLensesOnGo(t *testing.T) {
	chunks := []fileChunk{{files: []string{"a.go", "b.go"}, langs: []ingest.Language{ingest.LangGo}}}
	units := buildUnits(BuiltinLenses(), builtinStrategies(), chunks, nil)

	gatedNames := map[string]bool{
		"memory-safety":    false,
		"exception-safety": false,
		"dynamic-typing":   false,
	}
	for _, u := range units {
		if _, ok := gatedNames[u.lens.Name]; ok {
			gatedNames[u.lens.Name] = true
		}
	}
	for name, found := range gatedNames {
		if found {
			t.Errorf("buildUnits emitted a %q unit on a pure Go chunk — the language gate must skip defect-class lenses on Go", name)
		}
	}
}

// TestBuildUnits_GatesExceptionSafetyOnGo: pins a single-lens case for
// the exception-safety lens. A Go chunk must NOT emit any units, regardless
// of how many other lenses are in the lens slice.
func TestBuildUnits_GatesExceptionSafetyOnGo(t *testing.T) {
	chunks := []fileChunk{{files: []string{"x.go"}, langs: []ingest.Language{ingest.LangGo}}}
	units := buildUnits(BuiltinLenses(), builtinStrategies(), chunks, nil)
	for _, u := range units {
		if u.lens.Name == "exception-safety" {
			t.Fatalf("exception-safety emitted a Go-chunk unit: %+v (Go's error channel sidesteps throwing control flow)", u)
		}
	}
}

// TestBuildUnits_EmitsMemorySafetyOnCpp: a C++ chunk DOES emit
// memory-safety units — the lens's primary activation language.
func TestBuildUnits_EmitsMemorySafetyOnCpp(t *testing.T) {
	chunks := []fileChunk{{files: []string{"x.cc"}, langs: []ingest.Language{ingest.LangCPP}}}
	units := buildUnits(BuiltinLenses(), builtinStrategies(), chunks, nil)
	saw := false
	for _, u := range units {
		if u.lens.Name == "memory-safety" {
			saw = true
			break
		}
	}
	if !saw {
		t.Fatal("buildUnits emitted zero memory-safety units on a C++ chunk — the language gate must NOT drop the lens on its primary language")
	}
}

// TestLensesByYield_MemorySafetyFirstOnCpp: the language-gated defect
// lenses drive the yield ranking by language. On a C++ repo,
// memory-safety's yield (100) is the highest of any lens in the table, so
// lensesByYield MUST rank it first. This pins the invariant that the gate
// + the yield table together surface the right defect class first on the
// language it most matters for.
func TestLensesByYield_MemorySafetyFirstOnCpp(t *testing.T) {
	lenses := BuiltinLenses()
	ordered := lensesByYield(lenses, []ingest.Language{ingest.LangCPP})
	if len(ordered) == 0 {
		t.Fatal("lensesByYield returned an empty slice")
	}
	if ordered[0].Name != "memory-safety" {
		var got []string
		for _, l := range ordered {
			got = append(got, l.Name)
		}
		t.Errorf("lensesByYield(C++)[0] = %q, want %q (full order: %v)", ordered[0].Name, "memory-safety", got)
	}
}

// TestLensesByYield_PureGo: on a pure Go repo, none of the defect-class
// lenses should appear in the high-yield head of the ranking — Go's
// effective yield for them is 0 (their Go column in lensYields). The
// language-free lenses (nil-safety, concurrency, etc.) keep their
// historical positions.
func TestLensesByYield_PureGo(t *testing.T) {
	ordered := lensesByYield(BuiltinLenses(), []ingest.Language{ingest.LangGo})
	defectLenses := map[string]bool{
		"memory-safety":    true,
		"exception-safety": true,
		"dynamic-typing":   true,
	}
	// None of the defect-class lenses should be in the top 3 (the head of
	// the ranking is where budget flows first; if a defect lens makes the
	// head on a Go repo, the gate is broken).
	head := 3
	if head > len(ordered) {
		head = len(ordered)
	}
	for i := 0; i < head; i++ {
		if defectLenses[ordered[i].Name] {
			t.Errorf("lensesByYield(Go)[%d] = %q — defect-class lens in Go head (lensYields Go column should be 0)", i, ordered[i].Name)
		}
	}
}

// TestFinderSystemPrompt_CppRendersMemorySafetyBlock: a C++ chunk renders
// the memory-safety manifestation block via the per-language composition
// path. The block must contain a representative row and the lens's Core.
func TestFinderSystemPrompt_CppRendersMemorySafetyBlock(t *testing.T) {
	l := lensByName(t, "memory-safety")
	p := finderSystemPrompt("senior C++ engineer", l, []ingest.Language{ingest.LangCPP})
	if !strings.Contains(p, l.Core) {
		t.Errorf("memory-safety C++ prompt missing the lens Core")
	}
	if !strings.Contains(p, "How this manifests in C++:") {
		t.Errorf("memory-safety C++ prompt missing the C++ manifestation block (data-driven composition broken)")
	}
	for _, row := range manifestations["memory-safety"][ingest.LangCPP] {
		if !strings.Contains(p, row) {
			t.Errorf("memory-safety C++ prompt missing row %q", row)
		}
	}
}

// TestFinderSystemPrompt_GoOmitsMemorySafetyBlock: a Go chunk composes
// Core-only for memory-safety (no "How this manifests in Go:" block, no Go
// rows). The language gate is the upstream guard that keeps Go chunks from
// running the lens; the prompt-composition path is the second-line guard
// that prevents Go-specific noise from leaking into a prompt the lens
// should never see.
func TestFinderSystemPrompt_GoOmitsMemorySafetyBlock(t *testing.T) {
	l := lensByName(t, "memory-safety")
	p := finderSystemPrompt("senior Go engineer", l, []ingest.Language{ingest.LangGo})
	if !strings.Contains(p, l.Core) {
		t.Errorf("memory-safety Go prompt missing the lens Core")
	}
	if strings.Contains(p, "How this manifests in Go:") {
		t.Errorf("memory-safety Go prompt renders a Go block (should be Core-only for a language-gated lens)")
	}
	if strings.Contains(p, "How this manifests in") {
		// Defensive: any manifestation block at all means composition is
		// ignoring the language gate.
		t.Errorf("memory-safety Go prompt contains a manifestation block (any language): composition is not language-aware")
	}
}

// TestFinderSystemPrompt_ExceptionSafetyJSTSMerged: exception-safety uses
// a shared row slice for JavaScript and TypeScript (mirrors the existing
// rowsJSTS pattern), so a mixed JS+TS chunk renders ONE merged block
// (proves the shared-slice composition invariant extends to the new lens).
func TestFinderSystemPrompt_ExceptionSafetyJSTSMerged(t *testing.T) {
	l := lensByName(t, "exception-safety")
	p := finderSystemPrompt("senior JS/TS engineer", l,
		[]ingest.Language{ingest.LangJavaScript, ingest.LangTypeScript})
	if got := strings.Count(p, "How this manifests in"); got != 1 {
		t.Errorf("JS+TS chunk rendered %d exception-safety blocks, want 1 merged block (the shared-slice pattern)", got)
	}
	if !strings.Contains(p, "JavaScript/TypeScript") {
		t.Errorf("merged JS/TS exception-safety block missing the combined language label")
	}
}
