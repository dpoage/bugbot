package repro

import (
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/ingest"
)

// TestSystemPrompt_GoGuidance pins the verbatim Go guidance so it never drifts:
// the Go path is the historically-validated wording.
func TestSystemPrompt_GoGuidance(t *testing.T) {
	p := systemPrompt(ingest.LangGo)
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
	p := systemPrompt(ingest.LangPython)
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
// returns something other than the generic fallback. Both read the shared
// specificGuidance map, so divergence is structurally impossible — this test
// pins that property against a future refactor splitting them apart, and pins
// the expected membership so an accidental map edit is caught.
func TestHasGuidance(t *testing.T) {
	generic := langGuidance(ingest.Language("no-such-language"))
	all := []ingest.Language{
		ingest.LangGo, ingest.LangPython, ingest.LangJavaScript,
		ingest.LangTypeScript, ingest.LangRust, ingest.LangJava,
		ingest.LangC, ingest.LangCPP, ingest.LangRuby, ingest.LangCSharp,
		ingest.LangPHP, ingest.LangSwift, ingest.LangKotlin,
		ingest.LangShell, ingest.LangOther,
	}
	for _, lang := range all {
		want := langGuidance(lang) != generic
		if got := HasGuidance(lang); got != want {
			t.Errorf("HasGuidance(%s) = %v, but langGuidance specific-text presence = %v", lang, got, want)
		}
	}

	// Pin the expected membership itself, not just internal consistency.
	wantSpecific := map[ingest.Language]bool{
		ingest.LangGo: true, ingest.LangPython: true, ingest.LangJavaScript: true,
		ingest.LangTypeScript: true, ingest.LangRust: true,
	}
	for _, lang := range all {
		if got := HasGuidance(lang); got != wantSpecific[lang] {
			t.Errorf("HasGuidance(%s) = %v, want %v", lang, got, wantSpecific[lang])
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
			p := systemPrompt(tt.lang)
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
