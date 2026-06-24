package funnel

import (
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/agent"
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

// TestToolGuidance_PromptNamesMatchWiredTools is the drift guard: it builds the
// REAL finder and verifier tool sets from the production builders and asserts
// every tool name a prompt mentions is actually wired to that agent. This binds
// the prose to the wiring rather than to a hand-maintained string list, so the
// exact failure this change fixes — a prompt naming a tool the agent does not
// have, or a finder-only tool leaking into the verifier — fails the build.
func TestToolGuidance_PromptNamesMatchWiredTools(t *testing.T) {
	st, repo := openFixture(t)
	f, err := New(RoleClients{Finder: newScriptedClient(), Verifier: newScriptedClient()}, st, repo, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	base, err := f.readOnlyTools(agent.ReadCaps{})
	if err != nil {
		t.Fatal(err)
	}

	defNames := func(tools ...agent.Tool) map[string]bool {
		m := make(map[string]bool, len(tools))
		for _, tl := range tools {
			m[tl.Def().Name] = true
		}
		return m
	}

	// Finder = readOnlyTools + the per-unit tools hypothesize appends. Build the
	// finder-only tools with the SAME production constructors hypothesize uses so
	// their Def().Name is the real one, not a hand-typed string.
	postLead := agent.NewPostLeadTool("nil-safety/error-handling", []string{"nil-safety/error-handling"},
		func(string, string, int, string, float64) error { return nil })
	pkgCtx := agent.NewPackageContextTool(func(string) (string, bool, error) { return "", false, nil })
	pkgGraph := agent.NewPackageGraphTool(func(string, string) ([]string, []string, error) { return nil, nil, nil })

	wiredFinder := defNames(base...)
	for name := range defNames(postLead, pkgCtx, pkgGraph) {
		wiredFinder[name] = true
	}
	// Verifier = readOnlyTools; sandbox_exec/run_tests/status_note are conditional
	// and never appear in the base prompt under test.
	wiredVerifier := defNames(base...)

	// Every tool name the harness can advertise. The contract: if a prompt names
	// one of these, that tool MUST be wired to the agent.
	universe := []string{
		"read_file", "list_dir", "grep", "git_blame", "git_log",
		"find_definition", "find_references", "find_implementations",
		"read_symbol", "find_usages", "outline",
		"get_package_context", "package_graph", "post_lead", "status_note",
		"sandbox_exec", "run_tests",
	}

	cases := []struct {
		name   string
		prompt string
		wired  map[string]bool
	}{
		{"finder", finderSystemPrompt("senior Go engineer", aLens, nil), wiredFinder},
		{"verifier", verifierSystemPrompt("senior Go engineer", false), wiredVerifier},
	}
	for _, tc := range cases {
		for _, tool := range universe {
			if strings.Contains(tc.prompt, tool) && !tc.wired[tool] {
				t.Errorf("%s prompt names tool %q that is NOT wired to the %s agent (prompt/wiring drift)", tc.name, tool, tc.name)
			}
		}
		// The headline navigation tools must be both wired and documented.
		for _, must := range []string{"find_definition", "find_references", "find_implementations", "read_symbol", "outline", "find_usages"} {
			if !tc.wired[must] {
				t.Errorf("%s: expected %q to be wired", tc.name, must)
			}
			if !strings.Contains(tc.prompt, must) {
				t.Errorf("%s prompt: primary tool %q is wired but not documented", tc.name, must)
			}
		}
	}
}
