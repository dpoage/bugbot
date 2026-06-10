package config

import (
	"strings"
	"testing"
)

// TestPatchProverDefaults confirms Default() populates the repro.patch_prover
// section with the documented defaults.
func TestPatchProverDefaults(t *testing.T) {
	d := Default()
	if d.Repro.PatchProver {
		t.Errorf("default repro.patch_prover = true, want false")
	}
	if d.Repro.PatchMaxAttempts != 3 {
		t.Errorf("default repro.patch_max_attempts = %d, want 3", d.Repro.PatchMaxAttempts)
	}
}

// TestPatchProverValidation_Accepts confirms valid patch_max_attempts values
// pass Validate.
func TestPatchProverValidation_Accepts(t *testing.T) {
	path := writeTemp(t, validYAML)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load baseline: %v", err)
	}
	for _, n := range []int{1, 2, 3, 10} {
		cfg.Repro.PatchMaxAttempts = n
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate(patch_max_attempts=%d) = %v, want nil", n, err)
		}
	}
}

// TestPatchProverValidation_RejectsBad confirms zero and negative values are
// rejected with a helpful error.
func TestPatchProverValidation_RejectsBad(t *testing.T) {
	path := writeTemp(t, validYAML)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load baseline: %v", err)
	}

	cfg.Repro.PatchMaxAttempts = 0
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "repro.patch_max_attempts") {
		t.Errorf("patch_max_attempts=0 should be rejected, got %v", err)
	}

	cfg.Repro.PatchMaxAttempts = -1
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "repro.patch_max_attempts") {
		t.Errorf("patch_max_attempts=-1 should be rejected, got %v", err)
	}
}

// TestPatchProverYAMLParsing confirms the repro section is parsed from YAML.
func TestPatchProverYAMLParsing(t *testing.T) {
	yaml := validYAML + `
repro:
  patch_prover: true
  patch_max_attempts: 5
`
	path := writeTemp(t, yaml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.Repro.PatchProver {
		t.Errorf("patch_prover = false, want true")
	}
	if cfg.Repro.PatchMaxAttempts != 5 {
		t.Errorf("patch_max_attempts = %d, want 5", cfg.Repro.PatchMaxAttempts)
	}
}

// TestPatchProverEnvOverrides confirms BUGBOT_REPRO_* env vars override the
// repro section.
func TestPatchProverEnvOverrides(t *testing.T) {
	cfg := Default()
	if err := applyEnvOverrides(&cfg, []string{
		"BUGBOT_REPRO_PATCH_PROVER=true",
		"BUGBOT_REPRO_PATCH_MAX_ATTEMPTS=7",
	}); err != nil {
		t.Fatalf("applyEnvOverrides: %v", err)
	}
	if !cfg.Repro.PatchProver {
		t.Errorf("env override patch_prover = false, want true")
	}
	if cfg.Repro.PatchMaxAttempts != 7 {
		t.Errorf("env override patch_max_attempts = %d, want 7", cfg.Repro.PatchMaxAttempts)
	}
}

// TestPatchProverEnvOverrides_FalseValue confirms the false env value disables
// the prover.
func TestPatchProverEnvOverrides_FalseValue(t *testing.T) {
	cfg := Default()
	cfg.Repro.PatchProver = true // start enabled
	if err := applyEnvOverrides(&cfg, []string{
		"BUGBOT_REPRO_PATCH_PROVER=false",
	}); err != nil {
		t.Fatalf("applyEnvOverrides: %v", err)
	}
	if cfg.Repro.PatchProver {
		t.Errorf("env override patch_prover should be false")
	}
}

// TestPatchProverEnvOverrides_InvalidInt confirms an invalid integer is rejected.
func TestPatchProverEnvOverrides_InvalidInt(t *testing.T) {
	cfg := Default()
	if err := applyEnvOverrides(&cfg, []string{
		"BUGBOT_REPRO_PATCH_MAX_ATTEMPTS=notanumber",
	}); err == nil {
		t.Fatal("invalid int env value should return error")
	}
}

// TestReproSuiteCmd covers the yaml parsing and env override of the
// generalized suite command.
func TestReproSuiteCmd(t *testing.T) {
	d := Default()
	if d.Repro.SuiteCmd != nil {
		t.Errorf("default suite_cmd = %v, want nil (detect)", d.Repro.SuiteCmd)
	}

	cfg := Default()
	if err := applyEnvOverrides(&cfg, []string{"BUGBOT_REPRO_SUITE_CMD=cargo, test, --workspace"}); err != nil {
		t.Fatalf("applyEnvOverrides: %v", err)
	}
	want := []string{"cargo", "test", "--workspace"}
	if len(cfg.Repro.SuiteCmd) != 3 || cfg.Repro.SuiteCmd[0] != want[0] || cfg.Repro.SuiteCmd[1] != want[1] || cfg.Repro.SuiteCmd[2] != want[2] {
		t.Errorf("env suite_cmd = %v, want %v", cfg.Repro.SuiteCmd, want)
	}
}
