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
