package config

import (
	"strings"
	"testing"
)

// TestPublishDefaults confirms Default() populates the publish section with
// the documented defaults.
func TestPublishDefaults(t *testing.T) {
	d := Default()
	if d.Publish.Tracker != "github" {
		t.Errorf("default publish.tracker = %q, want github", d.Publish.Tracker)
	}
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
	if !d.Publish.SeverityLabels {
		t.Errorf("default publish.severity_labels = false, want true")
	}
	if !d.Publish.TierLabels {
		t.Errorf("default publish.tier_labels = false, want true")
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
  severity_labels: false
  tier_labels: false
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
	if cfg.Publish.SeverityLabels {
		t.Errorf("severity_labels = true, want false (explicit yaml false must stick)")
	}
	if cfg.Publish.TierLabels {
		t.Errorf("tier_labels = true, want false (explicit yaml false must stick)")
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
		"BUGBOT_PUBLISH_SEVERITY_LABELS=false",
		"BUGBOT_PUBLISH_TIER_LABELS=false",
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
	if cfg.Publish.SeverityLabels {
		t.Errorf("env override severity_labels = true, want false")
	}
	if cfg.Publish.TierLabels {
		t.Errorf("env override tier_labels = true, want false")
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

// TestPublishLabelKnobs_AbsentKeysKeepTrue confirms the default-true
// semantics survive the yaml overlay: a config with a publish section that
// omits severity_labels/tier_labels — and a config with no publish section
// at all — both load with the knobs still true.
func TestPublishLabelKnobs_AbsentKeysKeepTrue(t *testing.T) {
	for name, yaml := range map[string]string{
		"no publish section":        validYAML,
		"publish section, no knobs": validYAML + "\npublish:\n  enabled: true\n",
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := Load(writeTemp(t, yaml))
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if !cfg.Publish.SeverityLabels {
				t.Errorf("severity_labels = false, want true (absent key must keep default)")
			}
			if !cfg.Publish.TierLabels {
				t.Errorf("tier_labels = false, want true (absent key must keep default)")
			}
		})
	}
}

// TestPublishLabelKnobs_EnvOverYAML confirms env wins over yaml in both
// directions: env false disables a yaml-true knob, and env true re-enables
// a yaml-false knob.
func TestPublishLabelKnobs_EnvOverYAML(t *testing.T) {
	t.Run("env false overrides yaml true", func(t *testing.T) {
		t.Setenv("BUGBOT_PUBLISH_SEVERITY_LABELS", "false")
		t.Setenv("BUGBOT_PUBLISH_TIER_LABELS", "false")
		yaml := validYAML + "\npublish:\n  severity_labels: true\n  tier_labels: true\n"
		cfg, err := Load(writeTemp(t, yaml))
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if cfg.Publish.SeverityLabels {
			t.Errorf("severity_labels = true, want false (env must override yaml)")
		}
		if cfg.Publish.TierLabels {
			t.Errorf("tier_labels = true, want false (env must override yaml)")
		}
	})
	t.Run("env true re-enables yaml false", func(t *testing.T) {
		t.Setenv("BUGBOT_PUBLISH_SEVERITY_LABELS", "true")
		t.Setenv("BUGBOT_PUBLISH_TIER_LABELS", "true")
		yaml := validYAML + "\npublish:\n  severity_labels: false\n  tier_labels: false\n"
		cfg, err := Load(writeTemp(t, yaml))
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if !cfg.Publish.SeverityLabels {
			t.Errorf("severity_labels = false, want true (env true must re-enable)")
		}
		if !cfg.Publish.TierLabels {
			t.Errorf("tier_labels = false, want true (env true must re-enable)")
		}
	})
}

// TestStarterYAML_PublishLabelKnobs confirms the starter template's publish
// section round-trips through the strict loader with both label knobs true.
func TestStarterYAML_PublishLabelKnobs(t *testing.T) {
	cfg, err := Load(writeTemp(t, StarterYAML))
	if err != nil {
		t.Fatalf("StarterYAML failed to load: %v", err)
	}
	if !cfg.Publish.SeverityLabels {
		t.Errorf("template severity_labels = false, want true")
	}
	if !cfg.Publish.TierLabels {
		t.Errorf("template tier_labels = false, want true")
	}
}

// TestPublishLabelKnobs_AsymmetricYAML sets the two knobs to OPPOSITE yaml
// values so a swapped pair of yaml tags on the struct fields cannot pass:
// severity_labels:false must land on SeverityLabels and tier_labels:true on
// TierLabels, not vice versa.
func TestPublishLabelKnobs_AsymmetricYAML(t *testing.T) {
	yaml := validYAML + "\npublish:\n  severity_labels: false\n  tier_labels: true\n"
	cfg, err := Load(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Publish.SeverityLabels {
		t.Errorf("severity_labels = true, want false (yaml severity_labels must map to SeverityLabels)")
	}
	if !cfg.Publish.TierLabels {
		t.Errorf("tier_labels = false, want true (yaml tier_labels must map to TierLabels)")
	}
}

// TestPublishLabelKnobs_AsymmetricEnv sets ONLY the severity env var (no
// yaml knobs, tier left at its default) so a swapped pair of setBool
// destinations in applyEnvOverrides cannot pass: the override must land on
// SeverityLabels and leave TierLabels true.
func TestPublishLabelKnobs_AsymmetricEnv(t *testing.T) {
	cfg := Default()
	if err := applyEnvOverrides(&cfg, []string{
		"BUGBOT_PUBLISH_SEVERITY_LABELS=false",
	}); err != nil {
		t.Fatalf("applyEnvOverrides: %v", err)
	}
	if cfg.Publish.SeverityLabels {
		t.Errorf("severity_labels = true, want false (BUGBOT_PUBLISH_SEVERITY_LABELS must map to SeverityLabels)")
	}
	if !cfg.Publish.TierLabels {
		t.Errorf("tier_labels = false, want true (severity env var must not touch TierLabels)")
	}
}

// TestPublishTracker_YAMLVerbatim confirms an arbitrary tracker name loads
// verbatim: config deliberately does not gate membership (the tracker
// registry in internal/tracker does that at publish time; importing it here
// would create an import cycle).
func TestPublishTracker_YAMLVerbatim(t *testing.T) {
	yaml := validYAML + "\npublish:\n  tracker: gitlab\n"
	cfg, err := Load(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Publish.Tracker != "gitlab" {
		t.Errorf("tracker = %q, want gitlab (config must not gate membership)", cfg.Publish.Tracker)
	}
}

// TestPublishTracker_EmptyRejected confirms an explicit empty tracker in
// yaml is rejected by validation with an error naming the field. This is
// distinct from an absent key, which keeps the default via the overlay.
func TestPublishTracker_EmptyRejected(t *testing.T) {
	yaml := validYAML + "\npublish:\n  tracker: \"\"\n"
	_, err := Load(writeTemp(t, yaml))
	if err == nil || !strings.Contains(err.Error(), "publish.tracker") {
		t.Errorf("Load(tracker=\"\") = %v, want publish.tracker error", err)
	}
}

// TestPublishTracker_AbsentKeyKeepsDefault confirms the default survives the
// yaml overlay: a publish section without the tracker key — and a config
// with no publish section at all — both load with tracker == "github".
func TestPublishTracker_AbsentKeyKeepsDefault(t *testing.T) {
	for name, yaml := range map[string]string{
		"no publish section":          validYAML,
		"publish section, no tracker": validYAML + "\npublish:\n  enabled: true\n",
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := Load(writeTemp(t, yaml))
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if cfg.Publish.Tracker != "github" {
				t.Errorf("tracker = %q, want github (absent key must keep default)", cfg.Publish.Tracker)
			}
		})
	}
}

// TestPublishTracker_EnvOverridesYAML confirms BUGBOT_PUBLISH_TRACKER wins
// over an explicit yaml value.
func TestPublishTracker_EnvOverridesYAML(t *testing.T) {
	t.Setenv("BUGBOT_PUBLISH_TRACKER", "jira")
	yaml := validYAML + "\npublish:\n  tracker: gitlab\n"
	cfg, err := Load(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Publish.Tracker != "jira" {
		t.Errorf("tracker = %q, want jira (env must override yaml)", cfg.Publish.Tracker)
	}
}

// TestPublishTracker_AsymmetricEnv sets ONLY the tracker env var so a
// mis-wired setStr destination cannot pass: the override must land on
// Tracker and leave every neighbouring publish knob at its default.
func TestPublishTracker_AsymmetricEnv(t *testing.T) {
	cfg := Default()
	if err := applyEnvOverrides(&cfg, []string{
		"BUGBOT_PUBLISH_TRACKER=jira",
	}); err != nil {
		t.Fatalf("applyEnvOverrides: %v", err)
	}
	if cfg.Publish.Tracker != "jira" {
		t.Errorf("tracker = %q, want jira (BUGBOT_PUBLISH_TRACKER must map to Tracker)", cfg.Publish.Tracker)
	}
	if cfg.Publish.Enabled {
		t.Errorf("enabled = true, want false (tracker env var must not touch Enabled)")
	}
	if cfg.Publish.TierMin != 2 {
		t.Errorf("tier_min = %d, want 2 (tracker env var must not touch TierMin)", cfg.Publish.TierMin)
	}
	if len(cfg.Publish.Labels) != 1 || cfg.Publish.Labels[0] != "bugbot" {
		t.Errorf("labels = %v, want [bugbot] (tracker env var must not touch Labels)", cfg.Publish.Labels)
	}
	if !cfg.Publish.CloseOnFixed {
		t.Errorf("close_on_fixed = false, want true (tracker env var must not touch CloseOnFixed)")
	}
	if !cfg.Publish.SeverityLabels {
		t.Errorf("severity_labels = false, want true (tracker env var must not touch SeverityLabels)")
	}
	if !cfg.Publish.TierLabels {
		t.Errorf("tier_labels = false, want true (tracker env var must not touch TierLabels)")
	}
}

// TestStarterYAML_PublishTracker confirms the starter template round-trips
// through the strict loader with tracker == "github".
func TestStarterYAML_PublishTracker(t *testing.T) {
	cfg, err := Load(writeTemp(t, StarterYAML))
	if err != nil {
		t.Fatalf("StarterYAML failed to load: %v", err)
	}
	if cfg.Publish.Tracker != "github" {
		t.Errorf("template tracker = %q, want github", cfg.Publish.Tracker)
	}
}
