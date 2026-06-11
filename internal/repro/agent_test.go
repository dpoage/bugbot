package repro

import (
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/ingest"
)

// noSystems is a convenience alias for an empty build system slice, used in
// tests that exercise the no-systems (generic fallback) path.
var noSystems []ingest.BuildSystem

// TestSystemPrompt_GoGuidance pins the verbatim Go guidance so it never drifts:
// the Go path is the historically-validated wording.
func TestSystemPrompt_GoGuidance(t *testing.T) {
	p := systemPrompt(ingest.LangGo, noSystems)
	if !strings.Contains(p, "*_test.go file in the package that contains the bug") {
		t.Error("Go repro prompt must keep the *_test.go guidance")
	}
	if !strings.Contains(p, "go test -run <TestName> ./<pkg>") {
		t.Error("Go repro prompt must keep the `go test -run` command")
	}
}

// TestSystemPrompt_PythonGuidance confirms a Python finding gets pytest guidance
// and the Go-specific guidance is absent.
func TestSystemPrompt_PythonGuidance(t *testing.T) {
	p := systemPrompt(ingest.LangPython, noSystems)
	if !strings.Contains(p, "pytest") {
		t.Error("Python repro prompt must mention pytest")
	}
	if !strings.Contains(p, "test_*.py") {
		t.Error("Python repro prompt must mention the test_*.py convention")
	}
	if strings.Contains(p, "*_test.go") || strings.Contains(p, "go test -run") {
		t.Error("Python repro prompt must NOT contain Go-specific guidance")
	}
}

// TestHasGuidance verifies that HasGuidance agrees with langGuidance for every
// declared language: HasGuidance(l) must be true exactly when langGuidance(l)
// returns something other than the generic fallback. Both are derived from the
// same constants/map, so divergence is structurally impossible — this test pins
// that property against a future refactor splitting them apart, and pins the
// expected membership so an accidental edit is caught.
//
// With no build systems supplied:
//   - Go, Python, JavaScript, TypeScript, Rust → true (specificGuidance map)
//   - C, C++ → false (cmake/meson absent → generic fallback)
//   - all others → false
//
// With cmake or meson in systems, LangC/LangCPP flip to true.
// With only make/ninja in systems, LangC/LangCPP remain false.
func TestHasGuidance(t *testing.T) {
	generic := langGuidance(ingest.Language("no-such-language"), noSystems)
	all := []ingest.Language{
		ingest.LangGo, ingest.LangPython, ingest.LangJavaScript,
		ingest.LangTypeScript, ingest.LangRust, ingest.LangJava,
		ingest.LangC, ingest.LangCPP, ingest.LangRuby, ingest.LangCSharp,
		ingest.LangPHP, ingest.LangSwift, ingest.LangKotlin,
		ingest.LangShell, ingest.LangOther,
	}

	// Parity check: HasGuidance (no systems) agrees with langGuidance (no systems).
	for _, lang := range all {
		want := langGuidance(lang, noSystems) != generic
		if got := HasGuidance(lang); got != want {
			t.Errorf("HasGuidance(%s) = %v, but langGuidance specific-text presence = %v", lang, got, want)
		}
	}

	// Pin the expected membership with no systems.
	wantNoSystems := map[ingest.Language]bool{
		ingest.LangGo: true, ingest.LangPython: true, ingest.LangJavaScript: true,
		ingest.LangTypeScript: true, ingest.LangRust: true,
	}
	for _, lang := range all {
		if got := HasGuidance(lang); got != wantNoSystems[lang] {
			t.Errorf("HasGuidance(%s) [no systems] = %v, want %v", lang, got, wantNoSystems[lang])
		}
	}

	// C/C++ with cmake or meson → HasGuidance must be true.
	for _, cLang := range []ingest.Language{ingest.LangC, ingest.LangCPP} {
		if !HasGuidance(cLang, ingest.BuildSystemCMake) {
			t.Errorf("HasGuidance(%s, cmake) must be true", cLang)
		}
		if !HasGuidance(cLang, ingest.BuildSystemMeson) {
			t.Errorf("HasGuidance(%s, meson) must be true", cLang)
		}
	}

	// C/C++ with only make or ninja (no cmake/meson) → HasGuidance must be false.
	for _, cLang := range []ingest.Language{ingest.LangC, ingest.LangCPP} {
		if HasGuidance(cLang, ingest.BuildSystemMake) {
			t.Errorf("HasGuidance(%s, make only) must be false (heterogeneous targets, no specific guidance)", cLang)
		}
		if HasGuidance(cLang, ingest.BuildSystemNinja) {
			t.Errorf("HasGuidance(%s, ninja only) must be false (heterogeneous targets, no specific guidance)", cLang)
		}
	}
}

// TestSystemPrompt_PerLanguageGuidance covers the remaining language branches
// and the generic fallback, asserting each emits its matching framework hint.
func TestSystemPrompt_PerLanguageGuidance(t *testing.T) {
	tests := []struct {
		name string
		lang ingest.Language
		want string // substring that must appear
	}{
		{"javascript -> vitest/npm", ingest.LangJavaScript, "vitest run"},
		{"typescript -> vitest/npm", ingest.LangTypeScript, "vitest run"},
		{"rust -> cargo test", ingest.LangRust, "cargo test <test_name>"},
		{"unknown -> generic fallback", ingest.LangJava, "standard test framework for its language"},
		{"other -> generic fallback", ingest.LangOther, "standard test framework for its language"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := systemPrompt(tt.lang, noSystems)
			if !strings.Contains(p, tt.want) {
				t.Errorf("systemPrompt(%v) missing %q", tt.lang, tt.want)
			}
			// The language-independent body must always be present.
			if !strings.Contains(p, "You are Bugbot's reproducer agent.") {
				t.Error("repro prompt lost its fixed intro")
			}
		})
	}
}

// TestSystemPrompt_CppCMake confirms that a C++ finding in a CMake repo gets
// ctest guidance and does NOT contain invented make commands.
func TestSystemPrompt_CppCMake(t *testing.T) {
	cmakeSystems := []ingest.BuildSystem{ingest.BuildSystemCMake}
	p := systemPrompt(ingest.LangCPP, cmakeSystems)

	if !strings.Contains(p, "ctest") {
		t.Error("C++ + cmake prompt must mention ctest")
	}
	if !strings.Contains(p, "cmake --build build") {
		t.Error("C++ + cmake prompt must include cmake build command")
	}
	// Must not contain invented make invocations.
	if strings.Contains(p, "make test") || strings.Contains(p, "make check") {
		t.Error("C++ + cmake prompt must not invent make targets")
	}
	if !strings.Contains(p, "You are Bugbot's reproducer agent.") {
		t.Error("repro prompt lost its fixed intro")
	}
}

// TestSystemPrompt_CppMakeOnly confirms that a C++ finding in a make-only repo
// (no cmake, no meson) falls back to the generic guidance and does not contain
// invented make or ctest commands.
func TestSystemPrompt_CppMakeOnly(t *testing.T) {
	makeSystems := []ingest.BuildSystem{ingest.BuildSystemMake}
	p := systemPrompt(ingest.LangCPP, makeSystems)

	if !strings.Contains(p, "standard test framework for its language") {
		t.Error("C++ + make-only prompt must use generic fallback text")
	}
	// Must not invent specific commands for heterogeneous make targets.
	if strings.Contains(p, "make test") || strings.Contains(p, "make check") {
		t.Error("C++ + make-only prompt must not invent make targets")
	}
	if strings.Contains(p, "ctest") {
		t.Error("C++ + make-only prompt must not mention ctest (cmake absent)")
	}
}

// TestSystemPrompt_CppMeson confirms that a C++ finding in a Meson repo gets
// meson test guidance.
func TestSystemPrompt_CppMeson(t *testing.T) {
	mesonSystems := []ingest.BuildSystem{ingest.BuildSystemMeson}
	p := systemPrompt(ingest.LangCPP, mesonSystems)

	if !strings.Contains(p, "meson test") {
		t.Error("C++ + meson prompt must mention meson test")
	}
	if !strings.Contains(p, "meson setup build") {
		t.Error("C++ + meson prompt must include meson setup command")
	}
	if strings.Contains(p, "ctest") {
		t.Error("C++ + meson prompt must not mention ctest (cmake absent)")
	}
}
