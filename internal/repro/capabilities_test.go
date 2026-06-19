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
