package config

import (
	"strings"
	"testing"
)

// TestReviewDefaults confirms Default() populates the review section with the
// documented defaults.
func TestReviewDefaults(t *testing.T) {
	d := Default()
	if d.Review.FailOn != "verified" {
		t.Errorf("default review.fail_on = %q, want verified", d.Review.FailOn)
	}
	if d.Review.Suspected != "summary" {
		t.Errorf("default review.suspected = %q, want summary", d.Review.Suspected)
	}
}

// TestReviewValidation_Accepts confirms valid values (and the empty zero value,
// which resolves to a default at use) pass validation.
func TestReviewValidation_Accepts(t *testing.T) {
	for _, tc := range []struct{ failOn, suspected string }{
		{"verified", "summary"},
		{"none", "withhold"},
		{"", ""}, // empty -> default applied later
	} {
		path := writeTemp(t, validYAML)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("load baseline: %v", err)
		}
		cfg.Review.FailOn = tc.failOn
		cfg.Review.Suspected = tc.suspected
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate(failOn=%q,suspected=%q) = %v, want nil", tc.failOn, tc.suspected, err)
		}
	}
}

// TestReviewValidation_RejectsBad confirms unknown values are rejected with a
// helpful error.
func TestReviewValidation_RejectsBad(t *testing.T) {
	path := writeTemp(t, validYAML)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load baseline: %v", err)
	}

	cfg.Review.FailOn = "always"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "review.fail_on") {
		t.Errorf("bad fail_on should be rejected, got %v", err)
	}

	cfg.Review.FailOn = "verified"
	cfg.Review.Suspected = "inline"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "review.suspected") {
		t.Errorf("bad suspected should be rejected, got %v", err)
	}
}

// TestReviewYAMLParsing confirms the review section is parsed from YAML.
func TestReviewYAMLParsing(t *testing.T) {
	yaml := validYAML + `
review:
  fail_on: none
  suspected: withhold
`
	path := writeTemp(t, yaml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Review.FailOn != "none" || cfg.Review.Suspected != "withhold" {
		t.Errorf("parsed review = %+v", cfg.Review)
	}
}
