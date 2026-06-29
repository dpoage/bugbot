package repro

import (
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/sandbox"
)

// TestSystemPromptCapabilityGuidance verifies that systemPrompt correctly
// reflects the CapabilitySet in the returned prompt text.
func TestSystemPromptCapabilityGuidance(t *testing.T) {
	lang := ingest.LangGo
	systems := []ingest.BuildSystem{}

	t.Run("nil_caps_no_constraint_section", func(t *testing.T) {
		p := systemPrompt(lang, systems, nil)
		if strings.Contains(p, "capability constraints") {
			t.Errorf("want no capability section for nil caps, got:\n%s", p)
		}
	})

	t.Run("empty_caps_no_constraint_section", func(t *testing.T) {
		p := systemPrompt(lang, systems, sandbox.CapabilitySet{})
		if strings.Contains(p, "capability constraints") {
			t.Errorf("want no capability section for empty caps, got:\n%s", p)
		}
	})

	t.Run("race_absent_no_dash_race", func(t *testing.T) {
		caps := sandbox.CapabilitySet{"go": {"race": false}}
		p := systemPrompt(lang, systems, caps)
		// Must mention UNAVAILABLE for race.
		if !strings.Contains(p, "UNAVAILABLE") {
			t.Errorf("want UNAVAILABLE in prompt when race=false, got:\n%s", p)
		}
		// Must instruct not to use -race.
		if !strings.Contains(p, "-race") {
			t.Errorf("want -race mentioned in constraint text, got:\n%s", p)
		}
		// Must NOT offer -race as an option.
		if strings.Contains(p, "You MAY use") {
			t.Errorf("must not offer -race when unavailable, got:\n%s", p)
		}
		// Should suggest deterministic-assertion repro.
		if !strings.Contains(p, "deterministic") {
			t.Errorf("want deterministic fallback mentioned when race unavailable, got:\n%s", p)
		}
	})

	t.Run("race_present_offers_race", func(t *testing.T) {
		caps := sandbox.CapabilitySet{"go": {"race": true}}
		p := systemPrompt(lang, systems, caps)
		// Must say AVAILABLE.
		if !strings.Contains(p, "AVAILABLE") {
			t.Errorf("want AVAILABLE in prompt when race=true, got:\n%s", p)
		}
		// Must offer -race.
		if !strings.Contains(p, "You MAY use") {
			t.Errorf("want -race offered when available, got:\n%s", p)
		}
		// Must NOT say UNAVAILABLE.
		if strings.Contains(p, "UNAVAILABLE") {
			t.Errorf("must not say UNAVAILABLE when race is available, got:\n%s", p)
		}
	})

	t.Run("prompt_still_contains_core_requirements", func(t *testing.T) {
		caps := sandbox.CapabilitySet{"go": {"race": false}}
		p := systemPrompt(lang, systems, caps)
		// Core requirements must still be present regardless of capabilities.
		if !strings.Contains(p, "MINIMAL") {
			t.Errorf("core requirement 'MINIMAL' missing from prompt")
		}
		if !strings.Contains(p, "repro plan") {
			t.Errorf("core requirement 'repro plan' missing from prompt")
		}
	})
}

// TestCapabilityGuidanceOutput directly tests the capabilityGuidance helper.
func TestCapabilityGuidanceOutput(t *testing.T) {
	t.Run("nil_returns_empty", func(t *testing.T) {
		g := capabilityGuidance(nil)
		if g != "" {
			t.Errorf("capabilityGuidance(nil) = %q, want empty", g)
		}
	})

	t.Run("empty_returns_empty", func(t *testing.T) {
		g := capabilityGuidance(sandbox.CapabilitySet{})
		if g != "" {
			t.Errorf("capabilityGuidance(empty) = %q, want empty", g)
		}
	})

	t.Run("race_true_positive_guidance", func(t *testing.T) {
		g := capabilityGuidance(sandbox.CapabilitySet{"go": {"race": true}})
		if !strings.Contains(g, "AVAILABLE") {
			t.Errorf("want AVAILABLE in guidance, got %q", g)
		}
	})

	t.Run("race_false_negative_guidance", func(t *testing.T) {
		g := capabilityGuidance(sandbox.CapabilitySet{"go": {"race": false}})
		if !strings.Contains(g, "UNAVAILABLE") {
			t.Errorf("want UNAVAILABLE in guidance, got %q", g)
		}
	})
}

// TestCapabilityGuidance_Cpp covers the C++ capability rendering in
// capabilityGuidance and systemPrompt. Mirrors the existing Go-race subtests.
func TestCapabilityGuidance_Cpp(t *testing.T) {
	t.Run("tsan_available_contains_AVAILABLE_and_fsanitize_thread", func(t *testing.T) {
		caps := sandbox.CapabilitySet{"cpp": {"tsan": true, "asan": false, "ubsan": false}}
		g := capabilityGuidance(caps)
		if !strings.Contains(g, "AVAILABLE") {
			t.Errorf("want AVAILABLE in guidance when tsan=true, got:\n%s", g)
		}
		if !strings.Contains(g, "fsanitize=thread") {
			t.Errorf("want fsanitize=thread in guidance when tsan=true, got:\n%s", g)
		}
		// Should mention TSAN_OPTIONS and data race as demonstration signal.
		if !strings.Contains(g, "TSAN_OPTIONS") {
			t.Errorf("want TSAN_OPTIONS in guidance when tsan=true, got:\n%s", g)
		}
	})

	t.Run("tsan_unavailable_contains_UNAVAILABLE_and_fsanitize_thread_and_deterministic_fallback", func(t *testing.T) {
		caps := sandbox.CapabilitySet{"cpp": {"tsan": false, "asan": false, "ubsan": false}}
		g := capabilityGuidance(caps)
		if !strings.Contains(g, "UNAVAILABLE") {
			t.Errorf("want UNAVAILABLE in guidance when tsan=false, got:\n%s", g)
		}
		if !strings.Contains(g, "fsanitize=thread") {
			t.Errorf("want fsanitize=thread mentioned (to say do NOT use it) when tsan=false, got:\n%s", g)
		}
		// Must NOT offer TSan — "You MAY use" must be absent for tsan.
		if strings.Contains(g, "You MAY use `-fsanitize=thread") {
			t.Errorf("must not offer -fsanitize=thread when tsan=false, got:\n%s", g)
		}
		// Must mention deterministic fallback.
		if !strings.Contains(g, "deterministic") {
			t.Errorf("want deterministic fallback mentioned when tsan=false, got:\n%s", g)
		}
	})

	t.Run("asan_available_contains_AVAILABLE_and_fsanitize_address", func(t *testing.T) {
		caps := sandbox.CapabilitySet{"cpp": {"tsan": false, "asan": true, "ubsan": false}}
		g := capabilityGuidance(caps)
		// Offer phrasing uniquely distinguishes the available branch:
		// a bare Contains("AVAILABLE") is weak because "UNAVAILABLE"
		// contains it, and the deny line also mentions fsanitize=address.
		if !strings.Contains(g, "You MAY use `-fsanitize=address`") {
			t.Errorf("want ASan offered (\"You MAY use -fsanitize=address\") when asan=true, got:\n%s", g)
		}
	})

	t.Run("asan_unavailable_contains_UNAVAILABLE", func(t *testing.T) {
		caps := sandbox.CapabilitySet{"cpp": {"tsan": false, "asan": false, "ubsan": false}}
		g := capabilityGuidance(caps)
		if !strings.Contains(g, "UNAVAILABLE") {
			t.Errorf("want UNAVAILABLE in guidance when asan=false, got:\n%s", g)
		}
		if !strings.Contains(g, "Do NOT use `-fsanitize=address`") {
			t.Errorf("want ASan denied (\"Do NOT use -fsanitize=address\") when asan=false, got:\n%s", g)
		}
		if strings.Contains(g, "You MAY use `-fsanitize=address`") {
			t.Errorf("must not offer -fsanitize=address when asan=false, got:\n%s", g)
		}
	})

	t.Run("systemPrompt_cpp_tsan_true", func(t *testing.T) {
		caps := sandbox.CapabilitySet{"cpp": {"tsan": true, "asan": false, "ubsan": false}}
		p := systemPrompt(ingest.LangCPP, []ingest.BuildSystem{ingest.BuildSystemCMake}, caps)
		if !strings.Contains(p, "AVAILABLE") {
			t.Errorf("want AVAILABLE in system prompt when tsan=true")
		}
		if !strings.Contains(p, "fsanitize=thread") {
			t.Errorf("want fsanitize=thread in system prompt when tsan=true")
		}
	})

	t.Run("systemPrompt_cpp_tsan_false", func(t *testing.T) {
		caps := sandbox.CapabilitySet{"cpp": {"tsan": false, "asan": false, "ubsan": false}}
		p := systemPrompt(ingest.LangCPP, []ingest.BuildSystem{ingest.BuildSystemCMake}, caps)
		if !strings.Contains(p, "UNAVAILABLE") {
			t.Errorf("want UNAVAILABLE in system prompt when tsan=false")
		}
		if !strings.Contains(p, "fsanitize=thread") {
			t.Errorf("want fsanitize=thread mentioned in system prompt when tsan=false")
		}
	})
}

// TestCapabilityGuidance_Rust covers the Rust capability rendering in
// capabilityGuidance and systemPrompt. Mirrors TestCapabilityGuidance_Cpp.
func TestCapabilityGuidance_Rust(t *testing.T) {
	t.Run("cargo_available_contains_AVAILABLE_and_cargo_test", func(t *testing.T) {
		caps := sandbox.CapabilitySet{"rust": {"cargo": true, "miri": false}}
		g := capabilityGuidance(caps)
		if !strings.Contains(g, "AVAILABLE") {
			t.Errorf("want AVAILABLE in guidance when cargo=true, got:\n%s", g)
		}
		if !strings.Contains(g, "You MAY use `cargo test`") {
			t.Errorf("want cargo test offered when cargo=true, got:\n%s", g)
		}
	})

	t.Run("cargo_unavailable_contains_UNAVAILABLE_and_Do_NOT", func(t *testing.T) {
		caps := sandbox.CapabilitySet{"rust": {"cargo": false, "miri": false}}
		g := capabilityGuidance(caps)
		if !strings.Contains(g, "UNAVAILABLE") {
			t.Errorf("want UNAVAILABLE in guidance when cargo=false, got:\n%s", g)
		}
		if !strings.Contains(g, "Do NOT propose `cargo`") {
			t.Errorf("want cargo denied when cargo=false, got:\n%s", g)
		}
		if strings.Contains(g, "You MAY use `cargo test`") {
			t.Errorf("must not offer cargo test when cargo=false, got:\n%s", g)
		}
	})

	t.Run("miri_available_contains_AVAILABLE_and_miri", func(t *testing.T) {
		caps := sandbox.CapabilitySet{"rust": {"cargo": false, "miri": true}}
		g := capabilityGuidance(caps)
		if !strings.Contains(g, "Rust Miri: AVAILABLE") {
			t.Errorf("want Rust Miri: AVAILABLE when miri=true, got:\n%s", g)
		}
		if !strings.Contains(g, "cargo miri") {
			t.Errorf("want cargo miri mentioned when miri=true, got:\n%s", g)
		}
	})

	t.Run("miri_unavailable_contains_UNAVAILABLE_and_deterministic", func(t *testing.T) {
		caps := sandbox.CapabilitySet{"rust": {"cargo": true, "miri": false}}
		g := capabilityGuidance(caps)
		if !strings.Contains(g, "Rust Miri: UNAVAILABLE") {
			t.Errorf("want Rust Miri: UNAVAILABLE when miri=false, got:\n%s", g)
		}
		if !strings.Contains(g, "Do NOT use `cargo miri`") {
			t.Errorf("want cargo miri denied when miri=false, got:\n%s", g)
		}
		if !strings.Contains(g, "deterministic") {
			t.Errorf("want deterministic fallback when miri=false, got:\n%s", g)
		}
	})
	t.Run("cargo_and_miri_both_absent_emit_no_cargo_suggestion", func(t *testing.T) {
		// Regression guard (B2): cargo absent forces miri absent; the Miri
		// fallback must NOT recommend `cargo test`/`cargo miri` on an image
		// with no cargo, which would contradict the "Do NOT propose cargo" line.
		caps := sandbox.CapabilitySet{"rust": {"cargo": false, "miri": false}}
		g := capabilityGuidance(caps)
		if strings.Contains(g, "Rust Miri:") {
			t.Errorf("must not render a Miri line when cargo is absent, got:\n%s", g)
		}
		if strings.Contains(g, "deterministic") {
			t.Errorf("must not suggest a deterministic `cargo test` fallback when cargo is absent, got:\n%s", g)
		}
		if strings.Contains(g, "cargo miri") {
			t.Errorf("must not mention `cargo miri` when cargo is absent, got:\n%s", g)
		}
		if strings.Contains(g, "Rust Miri:") {
			t.Errorf("must not render a Miri line when cargo is absent, got:\n%s", g)
		}
	})

	t.Run("systemPrompt_rust_cargo_true", func(t *testing.T) {
		caps := sandbox.CapabilitySet{"rust": {"cargo": true, "miri": false}}
		p := systemPrompt(ingest.LangRust, []ingest.BuildSystem{}, caps)
		if !strings.Contains(p, "AVAILABLE") {
			t.Errorf("want AVAILABLE in system prompt when cargo=true")
		}
		if !strings.Contains(p, "cargo test") {
			t.Errorf("want cargo test mentioned in system prompt when cargo=true")
		}
	})

	t.Run("systemPrompt_rust_cargo_false", func(t *testing.T) {
		caps := sandbox.CapabilitySet{"rust": {"cargo": false, "miri": false}}
		p := systemPrompt(ingest.LangRust, []ingest.BuildSystem{}, caps)
		if !strings.Contains(p, "UNAVAILABLE") {
			t.Errorf("want UNAVAILABLE in system prompt when cargo=false")
		}
	})
}

// TestCapabilityGuidance_Js covers the JS capability rendering in
// capabilityGuidance and systemPrompt. Mirrors TestCapabilityGuidance_Cpp.
func TestCapabilityGuidance_Js(t *testing.T) {
	t.Run("node_available_contains_AVAILABLE_and_node", func(t *testing.T) {
		caps := sandbox.CapabilitySet{"js": {"node": true, "node_test": false}}
		g := capabilityGuidance(caps)
		if !strings.Contains(g, "AVAILABLE") {
			t.Errorf("want AVAILABLE in guidance when node=true, got:\n%s", g)
		}
		if !strings.Contains(g, "Node.js runtime: AVAILABLE") {
			t.Errorf("want Node.js runtime: AVAILABLE when node=true, got:\n%s", g)
		}
	})

	t.Run("node_unavailable_contains_UNAVAILABLE_and_Do_NOT", func(t *testing.T) {
		caps := sandbox.CapabilitySet{"js": {"node": false, "node_test": false}}
		g := capabilityGuidance(caps)
		if !strings.Contains(g, "Node.js runtime: UNAVAILABLE") {
			t.Errorf("want Node.js runtime: UNAVAILABLE when node=false, got:\n%s", g)
		}
		if !strings.Contains(g, "Do NOT propose node") {
			t.Errorf("want node denied when node=false, got:\n%s", g)
		}
		if strings.Contains(g, "You MAY run JS/TS repros") {
			t.Errorf("must not offer node when node=false, got:\n%s", g)
		}
	})

	t.Run("node_test_available_contains_AVAILABLE", func(t *testing.T) {
		caps := sandbox.CapabilitySet{"js": {"node": true, "node_test": true}}
		g := capabilityGuidance(caps)
		if !strings.Contains(g, "Node built-in test runner") {
			t.Errorf("want Node built-in test runner mentioned, got:\n%s", g)
		}
		if !strings.Contains(g, "AVAILABLE (node >= 18)") {
			t.Errorf("want AVAILABLE (node >= 18) when node_test=true, got:\n%s", g)
		}
		if !strings.Contains(g, "You MAY use `node --test`") {
			t.Errorf("want node --test offered when node_test=true, got:\n%s", g)
		}
	})

	t.Run("node_test_unavailable_contains_Do_NOT", func(t *testing.T) {
		caps := sandbox.CapabilitySet{"js": {"node": true, "node_test": false}}
		g := capabilityGuidance(caps)
		if !strings.Contains(g, "UNAVAILABLE (node < 18") {
			t.Errorf("want UNAVAILABLE (node < 18 when node_test=false, got:\n%s", g)
		}
		if !strings.Contains(g, "node script.js") {
			t.Errorf("want fallback script command when node_test=false, got:\n%s", g)
		}
		if strings.Contains(g, "You MAY use `node --test`") {
			t.Errorf("must not offer node --test when node_test=false, got:\n%s", g)
		}
	})
	t.Run("node_and_node_test_both_absent_emit_no_script_suggestion", func(t *testing.T) {
		// Regression guard (B1): node absent forces node_test absent; the
		// node_test fallback must NOT recommend `node script.js` on an image
		// with no node, which would contradict the "Do NOT propose node" line.
		caps := sandbox.CapabilitySet{"js": {"node": false, "node_test": false}}
		g := capabilityGuidance(caps)
		if strings.Contains(g, "node script.js") {
			t.Errorf("must not suggest `node script.js` when node is absent, got:\n%s", g)
		}
		if strings.Contains(g, "Node built-in test runner") {
			t.Errorf("must not render a node --test line when node is absent, got:\n%s", g)
		}
	})
	t.Run("systemPrompt_js_node_true", func(t *testing.T) {
		caps := sandbox.CapabilitySet{"js": {"node": true, "node_test": false}}
		p := systemPrompt(ingest.LangJavaScript, []ingest.BuildSystem{}, caps)
		if !strings.Contains(p, "Node.js runtime: AVAILABLE") {
			t.Errorf("want Node.js runtime: AVAILABLE in system prompt when node=true")
		}
	})

	t.Run("systemPrompt_js_node_false", func(t *testing.T) {
		caps := sandbox.CapabilitySet{"js": {"node": false, "node_test": false}}
		p := systemPrompt(ingest.LangJavaScript, []ingest.BuildSystem{}, caps)
		if !strings.Contains(p, "Node.js runtime: UNAVAILABLE") {
			t.Errorf("want Node.js runtime: UNAVAILABLE in system prompt when node=false")
		}
	})
}

// TestCapabilityGuidance_Python covers the Python capability rendering in
// capabilityGuidance and systemPrompt. Mirrors TestCapabilityGuidance_Cpp.
func TestCapabilityGuidance_Python(t *testing.T) {
	t.Run("pytest_available_contains_AVAILABLE_and_pytest", func(t *testing.T) {
		caps := sandbox.CapabilitySet{"python": {"python": true, "pytest": true}}
		g := capabilityGuidance(caps)
		if !strings.Contains(g, "Python pytest: AVAILABLE") {
			t.Errorf("want Python pytest: AVAILABLE when pytest=true, got:\n%s", g)
		}
		if !strings.Contains(g, "You MAY use `pytest`") {
			t.Errorf("want pytest offered when pytest=true, got:\n%s", g)
		}
	})

	t.Run("pytest_unavailable_python_available_fallback_to_unittest", func(t *testing.T) {
		caps := sandbox.CapabilitySet{"python": {"python": true, "pytest": false}}
		g := capabilityGuidance(caps)
		if !strings.Contains(g, "Python pytest: UNAVAILABLE") {
			t.Errorf("want Python pytest: UNAVAILABLE when pytest=false, got:\n%s", g)
		}
		if !strings.Contains(g, "python3 -m unittest") {
			t.Errorf("want python3 -m unittest fallback when python=true, pytest=false, got:\n%s", g)
		}
		if strings.Contains(g, "You MAY use `pytest`") {
			t.Errorf("must not offer pytest when pytest=false, got:\n%s", g)
		}
	})

	t.Run("pytest_unavailable_python_unavailable_no_python_fallback", func(t *testing.T) {
		caps := sandbox.CapabilitySet{"python": {"python": false, "pytest": false}}
		g := capabilityGuidance(caps)
		if !strings.Contains(g, "Python pytest: UNAVAILABLE") {
			t.Errorf("want Python pytest: UNAVAILABLE when pytest=false, got:\n%s", g)
		}
		if !strings.Contains(g, "do NOT propose Python") {
			t.Errorf("want no-Python fallback when python=false, got:\n%s", g)
		}
	})

	t.Run("python_available_only_silent_when_pytest_unavailable", func(t *testing.T) {
		// python=true, pytest=false: must NOT mention python -m unittest for the
		// pytest branch; must mention it as the fallback. The fallback path
		// branches on python=true.
		caps := sandbox.CapabilitySet{"python": {"python": true, "pytest": false}}
		g := capabilityGuidance(caps)
		if strings.Contains(g, "do NOT propose Python") {
			t.Errorf("must not say no-Python when python=true, got:\n%s", g)
		}
	})

	t.Run("systemPrompt_python_pytest_true", func(t *testing.T) {
		caps := sandbox.CapabilitySet{"python": {"python": true, "pytest": true}}
		p := systemPrompt(ingest.LangPython, []ingest.BuildSystem{}, caps)
		if !strings.Contains(p, "Python pytest: AVAILABLE") {
			t.Errorf("want Python pytest: AVAILABLE in system prompt when pytest=true")
		}
	})

	t.Run("systemPrompt_python_pytest_false", func(t *testing.T) {
		caps := sandbox.CapabilitySet{"python": {"python": true, "pytest": false}}
		p := systemPrompt(ingest.LangPython, []ingest.BuildSystem{}, caps)
		if !strings.Contains(p, "Python pytest: UNAVAILABLE") {
			t.Errorf("want Python pytest: UNAVAILABLE in system prompt when pytest=false")
		}
	})
}
