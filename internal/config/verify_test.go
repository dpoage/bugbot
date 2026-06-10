package config

import (
	"strings"
	"testing"
)

// TestVerifyDefaults confirms Default() populates the verify section with the
// documented defaults.
func TestVerifyDefaults(t *testing.T) {
	d := Default()
	if d.Verify.SandboxExec {
		t.Errorf("default verify.sandbox_exec = true, want false")
	}
	if d.Verify.SandboxMinSeverity != "high" {
		t.Errorf("default verify.sandbox_min_severity = %q, want high", d.Verify.SandboxMinSeverity)
	}
	if d.Verify.SandboxMaxExecs != 3 {
		t.Errorf("default verify.sandbox_max_execs = %d, want 3", d.Verify.SandboxMaxExecs)
	}
}

// TestVerifyValidation_Accepts confirms valid severity values pass validation.
func TestVerifyValidation_Accepts(t *testing.T) {
	for _, sev := range []string{"critical", "high", "medium", "low"} {
		path := writeTemp(t, validYAML)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("load baseline: %v", err)
		}
		cfg.Verify.SandboxMinSeverity = sev
		cfg.Verify.SandboxMaxExecs = 1
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate(sandbox_min_severity=%q) = %v, want nil", sev, err)
		}
	}
}

// TestVerifyValidation_EmptySeverityAccepted confirms the empty string (meaning
// "use default") is accepted by Validate.
func TestVerifyValidation_EmptySeverityAccepted(t *testing.T) {
	path := writeTemp(t, validYAML)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load baseline: %v", err)
	}
	cfg.Verify.SandboxMinSeverity = "" // empty -> default applied at use time
	cfg.Verify.SandboxMaxExecs = 1
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate with empty sandbox_min_severity = %v, want nil", err)
	}
}

// TestVerifyValidation_RejectsBad confirms junk values are rejected with a
// helpful error.
func TestVerifyValidation_RejectsBad(t *testing.T) {
	path := writeTemp(t, validYAML)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load baseline: %v", err)
	}

	cfg.Verify.SandboxMinSeverity = "extreme"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "verify.sandbox_min_severity") {
		t.Errorf("bad sandbox_min_severity should be rejected, got %v", err)
	}

	cfg.Verify.SandboxMinSeverity = "high"
	cfg.Verify.SandboxMaxExecs = 0
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "verify.sandbox_max_execs") {
		t.Errorf("sandbox_max_execs=0 should be rejected, got %v", err)
	}

	cfg.Verify.SandboxMaxExecs = -1
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "verify.sandbox_max_execs") {
		t.Errorf("sandbox_max_execs=-1 should be rejected, got %v", err)
	}
}

// TestVerifyYAMLParsing confirms the verify section is parsed from YAML.
func TestVerifyYAMLParsing(t *testing.T) {
	yaml := validYAML + `
verify:
  sandbox_exec: true
  sandbox_min_severity: medium
  sandbox_max_execs: 5
`
	path := writeTemp(t, yaml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.Verify.SandboxExec {
		t.Errorf("sandbox_exec = false, want true")
	}
	if cfg.Verify.SandboxMinSeverity != "medium" {
		t.Errorf("sandbox_min_severity = %q, want medium", cfg.Verify.SandboxMinSeverity)
	}
	if cfg.Verify.SandboxMaxExecs != 5 {
		t.Errorf("sandbox_max_execs = %d, want 5", cfg.Verify.SandboxMaxExecs)
	}
}

// TestVerifyEnvOverrides confirms BUGBOT_VERIFY_* env vars override the verify
// section like every other configurable section.
func TestVerifyEnvOverrides(t *testing.T) {
	cfg := Default()
	if err := applyEnvOverrides(&cfg, []string{
		"BUGBOT_VERIFY_SANDBOX_EXEC=true",
		"BUGBOT_VERIFY_SANDBOX_MIN_SEVERITY=critical",
		"BUGBOT_VERIFY_SANDBOX_MAX_EXECS=7",
	}); err != nil {
		t.Fatalf("applyEnvOverrides: %v", err)
	}
	if !cfg.Verify.SandboxExec {
		t.Errorf("env override sandbox_exec = false, want true")
	}
	if cfg.Verify.SandboxMinSeverity != "critical" {
		t.Errorf("env override sandbox_min_severity = %q, want critical", cfg.Verify.SandboxMinSeverity)
	}
	if cfg.Verify.SandboxMaxExecs != 7 {
		t.Errorf("env override sandbox_max_execs = %d, want 7", cfg.Verify.SandboxMaxExecs)
	}
}

// TestVerifyEnvOverrides_FalseValue confirms the false env value disables exec.
func TestVerifyEnvOverrides_FalseValue(t *testing.T) {
	cfg := Default()
	cfg.Verify.SandboxExec = true // start enabled
	if err := applyEnvOverrides(&cfg, []string{
		"BUGBOT_VERIFY_SANDBOX_EXEC=false",
	}); err != nil {
		t.Fatalf("applyEnvOverrides: %v", err)
	}
	if cfg.Verify.SandboxExec {
		t.Errorf("env override sandbox_exec should be false")
	}
}

// TestVerifyEnvOverrides_InvalidBool confirms an invalid boolean value is
// rejected with an error.
func TestVerifyEnvOverrides_InvalidBool(t *testing.T) {
	cfg := Default()
	if err := applyEnvOverrides(&cfg, []string{
		"BUGBOT_VERIFY_SANDBOX_EXEC=yes_please",
	}); err == nil {
		t.Fatal("invalid bool env value should return error")
	}
}

// TestVerifyEnvOverrides_InvalidInt confirms an invalid int value is rejected.
func TestVerifyEnvOverrides_InvalidInt(t *testing.T) {
	cfg := Default()
	if err := applyEnvOverrides(&cfg, []string{
		"BUGBOT_VERIFY_SANDBOX_MAX_EXECS=notanumber",
	}); err == nil {
		t.Fatal("invalid int env value should return error")
	}
}
