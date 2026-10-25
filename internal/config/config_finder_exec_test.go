package config

import (
	"strings"
	"testing"

	"github.com/dpoage/llmkit/sandbox"
)

func loadFinderExecConfig(t *testing.T, extra string) Config {
	t.Helper()
	cfg, err := Load(writeTemp(t, validYAML+extra))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func TestFinderExecDefaultsWhenUnset(t *testing.T) {
	cfg := loadFinderExecConfig(t, "")
	if cfg.Scan.FinderExec != "auto" {
		t.Errorf("finder_exec = %q, want auto", cfg.Scan.FinderExec)
	}
	if cfg.Scan.FinderExecMaxExecs != 8 {
		t.Errorf("finder_exec_max_execs = %d, want 8", cfg.Scan.FinderExecMaxExecs)
	}
	if cfg.Scan.FinderExecTimeoutSeconds != 120 {
		t.Errorf("finder_exec_timeout_seconds = %d, want 120", cfg.Scan.FinderExecTimeoutSeconds)
	}
	if cfg.Scan.FinderExecOutputBytes != 16384 {
		t.Errorf("finder_exec_output_bytes = %d, want 16384", cfg.Scan.FinderExecOutputBytes)
	}
}

func TestFinderExecValidateRejects(t *testing.T) {
	cases := []struct {
		name  string
		extra string
		want  string
	}{
		{"invalid mode", "scan:\n  finder_exec: sometimes\n", `scan.finder_exec "sometimes" invalid`},
		{"max execs zero", "scan:\n  finder_exec: auto\n  finder_exec_max_execs: 0\n", "scan.finder_exec_max_execs must be >= 1"},
		{"timeout zero", "scan:\n  finder_exec: auto\n  finder_exec_timeout_seconds: 0\n", "scan.finder_exec_timeout_seconds must be >= 1"},
		{"output bytes zero", "scan:\n  finder_exec: auto\n  finder_exec_output_bytes: 0\n", "scan.finder_exec_output_bytes 0 invalid"},
		{"output bytes over 1 MiB", "scan:\n  finder_exec: auto\n  finder_exec_output_bytes: 1048577\n", "scan.finder_exec_output_bytes 1048577 invalid"},
		{"backend typo", "sandbox:\n  backend: unsandbox\n", `sandbox.backend "unsandbox" invalid`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeTemp(t, validYAML+tc.extra))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load() err = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

func TestBackendUnsandboxedAccepted(t *testing.T) {
	cfg := loadFinderExecConfig(t, "sandbox:\n  backend: unsandboxed\n")
	if cfg.Sandbox.Backend != "unsandboxed" {
		t.Fatalf("backend = %q, want unsandboxed", cfg.Sandbox.Backend)
	}
	// The accepted upper bound for the output cap is llmkit's stream cap.
	cfg = loadFinderExecConfig(t, "scan:\n  finder_exec: on\n  finder_exec_output_bytes: 1048576\n")
	if cfg.Scan.FinderExecOutputBytes != sandbox.DefaultMaxOutputBytes {
		t.Errorf("output bytes = %d, want %d", cfg.Scan.FinderExecOutputBytes, sandbox.DefaultMaxOutputBytes)
	}
}

func TestStarterYAMLDocumentsFinderExecKeys(t *testing.T) {
	for _, key := range []string{
		"finder_exec:", "finder_exec_max_execs:", "finder_exec_timeout_seconds:", "finder_exec_output_bytes:",
		"unsandboxed",
	} {
		if !strings.Contains(StarterYAML, key) {
			t.Errorf("StarterYAML does not document %q", key)
		}
	}
}
