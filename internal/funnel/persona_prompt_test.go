package funnel

import (
	"strings"
	"testing"
)

// aLens is a stable lens fixture for prompt-construction tests. Its name and
// specialization must survive into the composed system prompt verbatim, since
// the eval harness routes scripted finders on the lens name.
var aLens = Lens{Name: "nil-safety/error-handling", Specialization: "Look for nil dereferences."}

// TestFinderSystemPrompt_GoPersona pins the Go persona clause so a regression in
// the wording is caught. The Go profile must reproduce the original "senior Go
// engineer" phrasing exactly.
func TestFinderSystemPrompt_GoPersona(t *testing.T) {
	p := finderSystemPrompt("senior Go engineer", aLens)
	if !strings.HasPrefix(p, "You are a meticulous senior Go engineer auditing a real codebase") {
		t.Errorf("Go finder prompt lost its persona clause; got prefix:\n%.80q", p)
	}
	// Lens routing anchors must still be present verbatim.
	if !strings.Contains(p, aLens.Name) {
		t.Error("finder prompt must contain the lens name (eval routing depends on it)")
	}
	if !strings.Contains(p, aLens.Specialization) {
		t.Error("finder prompt must contain the lens specialization")
	}
}

// TestFinderSystemPrompt_NonGoPersona confirms a non-Go profile adapts the
// persona and drops the hardcoded "Go engineer" framing entirely.
func TestFinderSystemPrompt_NonGoPersona(t *testing.T) {
	p := finderSystemPrompt("senior Python engineer", aLens)
	if !strings.Contains(p, "You are a meticulous senior Python engineer") {
		t.Errorf("non-Go finder prompt missing adapted persona; got prefix:\n%.80q", p)
	}
	if strings.Contains(p, "Go engineer") {
		t.Error("non-Go finder prompt must NOT contain 'Go engineer'")
	}
	// The fixed body after the persona must be untouched.
	if !strings.Contains(p, "auditing a real codebase for genuine, concrete bugs") {
		t.Error("finder prompt body changed unexpectedly")
	}
}

// TestVerifierSystemPrompt_GoPersona pins the Go persona clause for the verifier.
func TestVerifierSystemPrompt_GoPersona(t *testing.T) {
	p := verifierSystemPrompt("senior Go engineer", false)
	if !strings.HasPrefix(p, "You are a skeptical, exacting senior Go engineer.") {
		t.Errorf("Go verifier prompt lost its persona clause; got prefix:\n%.80q", p)
	}
}

// TestVerifierSystemPrompt_NonGoPersona confirms the verifier adapts and drops
// the hardcoded "Go engineer" framing.
func TestVerifierSystemPrompt_NonGoPersona(t *testing.T) {
	p := verifierSystemPrompt("senior software engineer with deep Python and JavaScript expertise", false)
	if !strings.Contains(p, "You are a skeptical, exacting senior software engineer with deep Python and JavaScript expertise.") {
		t.Errorf("non-Go verifier prompt missing adapted persona; got prefix:\n%.120q", p)
	}
	if strings.Contains(p, "Go engineer") {
		t.Error("non-Go verifier prompt must NOT contain 'Go engineer'")
	}
	if !strings.Contains(p, "Your ONLY job is to PROVE that the bug report below is WRONG") {
		t.Error("verifier prompt body changed unexpectedly")
	}
}
