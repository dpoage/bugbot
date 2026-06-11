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
