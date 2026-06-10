package config

import (
	"strings"
	"testing"
)

// TestPublishDefaults confirms Default() populates the publish section with
// the documented defaults.
func TestPublishDefaults(t *testing.T) {
	d := Default()
	if d.Publish.Enabled {
		t.Errorf("default publish.enabled = true, want false")
	}
	if d.Publish.TierMin != 2 {
		t.Errorf("default publish.tier_min = %d, want 2", d.Publish.TierMin)
	}
	if len(d.Publish.Labels) != 1 || d.Publish.Labels[0] != "bugbot" {
		t.Errorf("default publish.labels = %v, want [bugbot]", d.Publish.Labels)
	}
	if !d.Publish.CloseOnFixed {
		t.Errorf("default publish.close_on_fixed = false, want true")
	}
}

// TestPublishValidation_Accepts confirms valid tier_min values pass.
func TestPublishValidation_Accepts(t *testing.T) {
	for _, tier := range []int{0, 1, 2, 3} {
		path := writeTemp(t, validYAML)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("load baseline: %v", err)
		}
		cfg.Publish.TierMin = tier
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate(tier_min=%d) = %v, want nil", tier, err)
		}
	}
}

// TestPublishValidation_RejectsBad confirms out-of-range tier_min is rejected.
func TestPublishValidation_RejectsBad(t *testing.T) {
	for _, tier := range []int{-1, 4, 99} {
		path := writeTemp(t, validYAML)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("load baseline: %v", err)
		}
		cfg.Publish.TierMin = tier
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "publish.tier_min") {
			t.Errorf("Validate(tier_min=%d) = %v, want publish.tier_min error", tier, err)
		}
	}
}

// TestPublishYAMLParsing confirms the publish section is parsed from YAML.
func TestPublishYAMLParsing(t *testing.T) {
	yaml := validYAML + `
publish:
  enabled: true
  tier_min: 1
  labels: [bugbot, security]
  close_on_fixed: false
`
	path := writeTemp(t, yaml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.Publish.Enabled {
		t.Errorf("enabled = false, want true")
	}
	if cfg.Publish.TierMin != 1 {
		t.Errorf("tier_min = %d, want 1", cfg.Publish.TierMin)
	}
	if len(cfg.Publish.Labels) != 2 || cfg.Publish.Labels[0] != "bugbot" || cfg.Publish.Labels[1] != "security" {
		t.Errorf("labels = %v, want [bugbot security]", cfg.Publish.Labels)
	}
	if cfg.Publish.CloseOnFixed {
		t.Errorf("close_on_fixed = true, want false")
	}
}

// TestPublishEnvOverrides confirms BUGBOT_PUBLISH_* env vars override the
// publish section.
func TestPublishEnvOverrides(t *testing.T) {
	cfg := Default()
	if err := applyEnvOverrides(&cfg, []string{
		"BUGBOT_PUBLISH_ENABLED=true",
		"BUGBOT_PUBLISH_TIER_MIN=1",
		"BUGBOT_PUBLISH_LABELS=bugbot, security, critical",
		"BUGBOT_PUBLISH_CLOSE_ON_FIXED=false",
	}); err != nil {
		t.Fatalf("applyEnvOverrides: %v", err)
	}
	if !cfg.Publish.Enabled {
		t.Errorf("env override enabled = false, want true")
	}
	if cfg.Publish.TierMin != 1 {
		t.Errorf("env override tier_min = %d, want 1", cfg.Publish.TierMin)
	}
	if len(cfg.Publish.Labels) != 3 {
		t.Errorf("env override labels = %v, want 3 items", cfg.Publish.Labels)
	}
	if cfg.Publish.CloseOnFixed {
		t.Errorf("env override close_on_fixed = true, want false")
	}
}

// TestPublishEnvOverrides_InvalidTierMin confirms a bad int is rejected.
func TestPublishEnvOverrides_InvalidTierMin(t *testing.T) {
	cfg := Default()
	if err := applyEnvOverrides(&cfg, []string{
		"BUGBOT_PUBLISH_TIER_MIN=notanumber",
	}); err == nil {
		t.Fatal("invalid tier_min env value should return error")
	}
}
