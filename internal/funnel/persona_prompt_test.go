package funnel

import (
	"strings"
	"testing"
)

// aLens is a stable lens fixture for prompt-construction tests. Its name and
// core must survive into the composed system prompt verbatim, since the eval
// harness routes scripted finders on the lens name.
var aLens = Lens{Name: "nil-safety/error-handling", Core: "Look for nil dereferences."}

// TestFinderSystemPrompt_GoPersona pins the Go persona clause so a regression in
// the wording is caught. The Go profile must reproduce the original "senior Go
// engineer" phrasing exactly.
func TestFinderSystemPrompt_GoPersona(t *testing.T) {
	p := finderSystemPrompt("senior Go engineer", aLens, nil)
	if !strings.HasPrefix(p, "You are a meticulous senior Go engineer auditing a real codebase") {
		t.Errorf("Go finder prompt lost its persona clause; got prefix:\n%.80q", p)
	}
	// Lens routing anchors must still be present verbatim.
	if !strings.Contains(p, aLens.Name) {
		t.Error("finder prompt must contain the lens name (eval routing depends on it)")
	}
	if !strings.Contains(p, aLens.Core) {
		t.Error("finder prompt must contain the lens core")
	}
}

// TestFinderSystemPrompt_NonGoPersona confirms a non-Go profile adapts the
// persona and drops the hardcoded "Go engineer" framing entirely.
func TestFinderSystemPrompt_NonGoPersona(t *testing.T) {
	p := finderSystemPrompt("senior Python engineer", aLens, nil)
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
	p := verifierSystemPrompt("senior Go engineer", false, true)
	if !strings.HasPrefix(p, "You are a skeptical, exacting senior Go engineer.") {
		t.Errorf("Go verifier prompt lost its persona clause; got prefix:\n%.80q", p)
	}
}

// TestVerifierSystemPrompt_NonGoPersona confirms the verifier adapts and drops
// the hardcoded "Go engineer" framing.
func TestVerifierSystemPrompt_NonGoPersona(t *testing.T) {
	p := verifierSystemPrompt("senior software engineer with deep Python and JavaScript expertise", false, true)
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

// TestVerifierPrompts_StdlibSourceDirective asserts that the stdlib/runtime
// source-verification directive is present in all three verifier-side prompt
// variants: refuter without sandbox, refuter with sandbox, and arbiter.
// This guards against the directive being accidentally dropped from the shared
// verifierRefutationCriteria constant that both agents compose.
func TestVerifierPrompts_StdlibSourceDirective(t *testing.T) {
	const persona = "senior Go engineer"

	// Stable substrings from the directive added to verifierRefutationCriteria.
	// If the wording changes these will catch regressions.
	checks := []struct {
		name   string
		substr string
	}{
		{"reads actual source", "reading the actual source"},
		{"GOROOT available", "GOROOT"},
		{"not from memory", "NEVER assert it from memory"},
		{"cite what was read", "cite what you read"},
		{"unverified claim rejected", "An unverified stdlib/runtime/library claim is not acceptable refutation evidence"},
	}

	prompts := []struct {
		name string
		p    string
	}{
		{"verifierSystemPrompt(no sandbox)", verifierSystemPrompt(persona, false, true)},
		{"verifierSystemPrompt(sandbox)", verifierSystemPrompt(persona, true, true)},
		{"arbiterSystemPrompt(no sandbox)", arbiterSystemPrompt(persona, false, true)},
		{"arbiterSystemPrompt(sandbox)", arbiterSystemPrompt(persona, true, true)},
	}

	for _, pp := range prompts {
		for _, c := range checks {
			if !strings.Contains(pp.p, c.substr) {
				t.Errorf("%s: missing stdlib directive substring %q", pp.name, c.substr)
			}
		}
	}
}

// TestArbiterPrompt_NoSandbox_ForbidsProbeFabrication pins bugbot-mi5.20 AC1/AC2:
// when NO command-execution tool is wired, the arbiter prompt must not invite the
// model to run a command/probe (the pressure that produced a fabricated executed
// probe in the controller live replay) and must explicitly forbid claiming an
// un-run execution. With a sandbox, the probe guidance returns.
func TestArbiterPrompt_NoSandbox_ForbidsProbeFabrication(t *testing.T) {
	const persona = "senior C++ engineer"
	noSandbox := arbiterSystemPrompt(persona, false, true)
	withSandbox := arbiterSystemPrompt(persona, true, true)

	// No execution-inviting wording when the arbiter has no exec tool.
	forbidden := []string{"executable probe", "running a probe", "run a probe", "consult a probe", "observed output", "sandbox_exec"}
	for _, f := range forbidden {
		if strings.Contains(noSandbox, f) {
			t.Errorf("no-sandbox arbiter prompt must not invite/advertise execution; found %q", f)
		}
	}
	// Explicit prohibition present.
	if !strings.Contains(noSandbox, "never invent an execution result") {
		t.Error("no-sandbox arbiter prompt must explicitly forbid inventing an execution result")
	}
	if !strings.Contains(noSandbox, "Do NOT claim to have executed anything") {
		t.Error("no-sandbox arbiter prompt must forbid claiming to have executed anything")
	}
	// With a sandbox, probe/execution guidance returns.
	if !strings.Contains(withSandbox, "sandbox_exec") {
		t.Error("with-sandbox arbiter prompt must advertise sandbox_exec")
	}
	if strings.Contains(withSandbox, "never invent an execution result") {
		t.Error("with-sandbox arbiter prompt must not carry the no-exec prohibition")
	}
}
