package funnel

import (
	"strings"
	"testing"
)

// TestToolGuidance_PrimaryBeforeFallback pins the tool-selection hierarchy that
// both the finder and verifier prompts must teach: precise/structural tools are
// PRIMARY and must be introduced before grep and whole-file reads, which are the
// FALLBACK. This is the anti-regression guard for the change that stopped the
// prompts from foregrounding grep/read_file (models were defaulting to text
// search instead of code navigation).
func TestToolGuidance_PrimaryBeforeFallback(t *testing.T) {
	prompts := map[string]string{
		"finder":   finderSystemPrompt("senior Go engineer", aLens, nil),
		"verifier": verifierSystemPrompt("senior Go engineer", false),
	}
	for name, p := range prompts {
		primary := strings.Index(p, "PRIMARY")
		fallback := strings.Index(p, "FALLBACK")
		if primary < 0 || fallback < 0 {
			t.Fatalf("%s prompt missing PRIMARY/FALLBACK sections", name)
		}
		if primary >= fallback {
			t.Errorf("%s prompt: PRIMARY section (%d) must precede FALLBACK section (%d)", name, primary, fallback)
		}
		// The headline precise tools must be named before grep is named, and
		// grep itself must live under the FALLBACK section.
		grepAt := strings.Index(p, "grep")
		if grepAt < 0 {
			t.Errorf("%s prompt: grep is no longer mentioned at all", name)
			continue
		}
		if grepAt < fallback {
			t.Errorf("%s prompt: grep (%d) appears before the FALLBACK section (%d) — it must be presented as a fallback", name, grepAt, fallback)
		}
		for _, tool := range []string{"find_references", "read_symbol", "outline"} {
			at := strings.Index(p, tool)
			if at < 0 {
				t.Errorf("%s prompt: missing primary tool %q", name, tool)
				continue
			}
			if at >= grepAt {
				t.Errorf("%s prompt: primary tool %q (%d) must appear before grep (%d)", name, tool, at, grepAt)
			}
		}
	}
}

// TestToolGuidance_FinderOnlyVsShared pins which tools each side's prompt may
// name. The finder coordinates across lenses and pulls cartography, so it names
// post_lead/get_package_context/package_graph; the verifier must NOT, because
// refuter independence is the core false-positive killer (post_lead and the
// cartography tools are deliberately absent from the verifier tool set). Both
// sides share the read-only navigation, git, and structural tools.
func TestToolGuidance_FinderOnlyVsShared(t *testing.T) {
	finder := finderSystemPrompt("senior Go engineer", aLens, nil)
	verifier := verifierSystemPrompt("senior Go engineer", false)

	for _, finderOnly := range []string{"post_lead", "get_package_context", "package_graph"} {
		if !strings.Contains(finder, finderOnly) {
			t.Errorf("finder prompt must name finder-only tool %q", finderOnly)
		}
		if strings.Contains(verifier, finderOnly) {
			t.Errorf("verifier prompt must NOT name finder-only tool %q (refuter independence)", finderOnly)
		}
	}

	for _, shared := range []string{"find_definition", "find_implementations", "find_usages", "outline", "git_blame", "git_log"} {
		if !strings.Contains(finder, shared) {
			t.Errorf("finder prompt must name shared tool %q", shared)
		}
		if !strings.Contains(verifier, shared) {
			t.Errorf("verifier prompt must name shared tool %q", shared)
		}
	}

	// sandbox_exec is advertised only via the conditional sandbox paragraph, so
	// the base verifier prompt (no sandbox) must not mention it — the arbiter
	// no-sandbox test depends on this too.
	if strings.Contains(verifier, "sandbox_exec") {
		t.Error("verifier base prompt must not mention sandbox_exec (conditional paragraph only)")
	}
}
