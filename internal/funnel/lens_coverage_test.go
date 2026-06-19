package funnel

import "testing"

// coreLensNames lists the builtin lenses that are intentionally language-free
// (Core-only, no manifestation rows). Adding a lens here exempts it from the
// coverage assertion below; removing one without also adding manifestation rows
// will cause that test to fail.
var coreLensNames = map[LensName]bool{
	LensDiffIntent:            true,
	LensCrossLanguageBoundary: true,
}

// TestBuiltinLensManifestation asserts that every non-core BuiltinLens has at
// least one entry in the manifestations table. This catches the silent precision
// loss that occurs when a lens is added to BuiltinLenses but its per-language
// manifestation rows are forgotten: the lens would run with Core-only prompts,
// which are intentionally language-free and therefore less targeted than they
// should be for a taxonomy lens.
//
// Lenses that are by design language-free (diff-intent, cross-language-boundary)
// are listed in coreLensNames and exempt.
func TestBuiltinLensManifestation(t *testing.T) {
	t.Helper()
	for _, l := range BuiltinLenses() {
		if coreLensNames[l.Name] {
			continue // language-free by design; no manifestation rows expected
		}
		rows, ok := manifestations[l.Name]
		if !ok || len(rows) == 0 {
			t.Errorf("lens %q: no entry in manifestations table (add per-language rows or list it in coreLensNames if it is intentionally language-free)", l.Name)
			continue
		}
		// At least one language must have a non-empty row slice.
		hasRows := false
		for _, langRows := range rows {
			if len(langRows) > 0 {
				hasRows = true
				break
			}
		}
		if !hasRows {
			t.Errorf("lens %q: manifestations entry exists but all language slices are empty", l.Name)
		}
	}
}

// TestCoreLensNamesExist asserts that every name listed in coreLensNames is
// actually present in BuiltinLenses. This prevents coreLensNames from
// accumulating stale entries for deleted lenses.
func TestCoreLensNamesExist(t *testing.T) {
	t.Helper()
	present := make(map[LensName]bool, len(BuiltinLenses()))
	for _, l := range BuiltinLenses() {
		present[l.Name] = true
	}
	for name := range coreLensNames {
		if !present[name] {
			t.Errorf("coreLensNames contains %q but no BuiltinLens has that name (stale entry)", name)
		}
	}
}

// TestLensNameConsts asserts that every LensName constant resolves to a lens
// in BuiltinLenses. A renamed or deleted lens that leaves a dangling const is
// caught here.
func TestLensNameConsts(t *testing.T) {
	t.Helper()
	allConsts := []LensName{
		LensNilSafety,
		LensDiffIntent,
		LensConcurrency,
		LensResourceLeaks,
		LensBoundaryConditions,
		LensAPIContractMisuse,
		LensInjection,
		LensMemorySafety,
		LensExceptionSafety,
		LensDynamicTyping,
		LensCrossLanguageBoundary,
	}
	present := make(map[LensName]bool, len(BuiltinLenses()))
	for _, l := range BuiltinLenses() {
		present[l.Name] = true
	}
	for _, c := range allConsts {
		if !present[c] {
			t.Errorf("LensName const %q does not match any BuiltinLens (rename or delete the const)", c)
		}
	}
}
