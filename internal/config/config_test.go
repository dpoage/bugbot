package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// validYAML is a minimal-but-complete config used as a baseline in tests.
const validYAML = `
providers:
  anthropic:
    type: anthropic
    api_key_env: ANTHROPIC_API_KEY
roles:
  finder:
    provider: anthropic
    model: claude-haiku-4-5
  verifier:
    provider: anthropic
    model: claude-opus-4-8
  reproducer:
    provider: anthropic
    model: claude-sonnet-4-5
budgets:
  per_cycle_tokens: 100000
  per_day_tokens: 1000000
`

func writeTemp(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "bugbot.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr bool
		check   func(t *testing.T, c Config)
	}{
		{
			name: "valid minimal config fills defaults",
			yaml: validYAML,
			check: func(t *testing.T, c Config) {
				if c.Storage.Path != ".bugbot/state.db" {
					t.Errorf("storage.path = %q, want default .bugbot/state.db", c.Storage.Path)
				}
				if c.Sandbox.Network != "none" {
					t.Errorf("sandbox.network = %q, want default none", c.Sandbox.Network)
				}
				if c.Sandbox.Runtime != "podman" {
					t.Errorf("sandbox.runtime = %q, want default podman", c.Sandbox.Runtime)
				}
				if c.Daemon.PollInterval != 60*time.Second {
					t.Errorf("daemon.poll_interval = %s, want 60s", c.Daemon.PollInterval)
				}
				if c.Roles.Verifier.Model != "claude-opus-4-8" {
					t.Errorf("verifier model = %q", c.Roles.Verifier.Model)
				}
			},
		},
		{
			name: "explicit values override defaults",
			yaml: validYAML + `
sandbox:
  runtime: docker
  network: bridge
  cpus: 8
  memory_mb: 4096
  timeout_seconds: 120
daemon:
  poll_interval: 30s
  sweep_interval: 12h
  idle_backoff: 2m
storage:
  path: /tmp/state.db
`,
			check: func(t *testing.T, c Config) {
				if c.Sandbox.Runtime != "docker" {
					t.Errorf("runtime = %q, want docker", c.Sandbox.Runtime)
				}
				if c.Sandbox.Network != "bridge" {
					t.Errorf("network = %q, want bridge", c.Sandbox.Network)
				}
				if c.Daemon.PollInterval != 30*time.Second {
					t.Errorf("poll = %s, want 30s", c.Daemon.PollInterval)
				}
				if c.Storage.Path != "/tmp/state.db" {
					t.Errorf("storage = %q", c.Storage.Path)
				}
			},
		},
		{
			name:    "missing providers fails",
			yaml:    "roles:\n  finder:\n    provider: x\n    model: y\n",
			wantErr: true,
		},
		{
			name: "unknown provider type fails",
			yaml: `
providers:
  bad:
    type: bogus
    api_key_env: X
roles:
  finder: {provider: bad, model: m}
  verifier: {provider: bad, model: m}
  reproducer: {provider: bad, model: m}
`,
			wantErr: true,
		},
		{
			name: "role references unknown provider fails",
			yaml: `
providers:
  anthropic: {type: anthropic, api_key_env: K}
roles:
  finder: {provider: nope, model: m}
  verifier: {provider: anthropic, model: m}
  reproducer: {provider: anthropic, model: m}
budgets: {per_cycle_tokens: 1, per_day_tokens: 2}
`,
			wantErr: true,
		},
		{
			name: "openai-compatible without base_url fails",
			yaml: `
providers:
  local: {type: openai-compatible, api_key_env: K}
roles:
  finder: {provider: local, model: m}
  verifier: {provider: local, model: m}
  reproducer: {provider: local, model: m}
budgets: {per_cycle_tokens: 1, per_day_tokens: 2}
`,
			wantErr: true,
		},
		{
			name: "per_cycle exceeding per_day fails",
			yaml: validYAML + `
budgets:
  per_cycle_tokens: 2000000
  per_day_tokens: 1000000
`,
			wantErr: true,
		},
		{
			name:    "malformed yaml fails",
			yaml:    "providers: [this is: not valid",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTemp(t, tt.yaml)
			cfg, err := Load(path)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Load() = nil error, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if tt.check != nil {
				tt.check(t, cfg)
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml")); err == nil {
		t.Fatal("Load() of missing file = nil error, want error")
	}
}

func TestApplyEnvOverrides(t *testing.T) {
	tests := []struct {
		name    string
		environ []string
		wantErr bool
		check   func(t *testing.T, c Config)
	}{
		{
			name: "string and numeric overrides",
			environ: []string{
				"BUGBOT_STORAGE_PATH=/custom/state.db",
				"BUGBOT_SANDBOX_RUNTIME=docker",
				"BUGBOT_SANDBOX_CPUS=16",
				"BUGBOT_BUDGETS_PER_DAY_TOKENS=9000000",
				"UNRELATED=ignored",
			},
			check: func(t *testing.T, c Config) {
				if c.Storage.Path != "/custom/state.db" {
					t.Errorf("storage = %q", c.Storage.Path)
				}
				if c.Sandbox.Runtime != "docker" {
					t.Errorf("runtime = %q", c.Sandbox.Runtime)
				}
				if c.Sandbox.CPUs != 16 {
					t.Errorf("cpus = %d", c.Sandbox.CPUs)
				}
				if c.Budgets.PerDayTokens != 9_000_000 {
					t.Errorf("per_day = %d", c.Budgets.PerDayTokens)
				}
			},
		},
		{
			name: "duration override",
			environ: []string{
				"BUGBOT_DAEMON_POLL_INTERVAL=15s",
			},
			check: func(t *testing.T, c Config) {
				if c.Daemon.PollInterval != 15*time.Second {
					t.Errorf("poll = %s, want 15s", c.Daemon.PollInterval)
				}
			},
		},
		{
			name:    "invalid int override fails",
			environ: []string{"BUGBOT_SANDBOX_CPUS=notanumber"},
			wantErr: true,
		},
		{
			name:    "invalid duration override fails",
			environ: []string{"BUGBOT_DAEMON_IDLE_BACKOFF=5flips"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			err := applyEnvOverrides(&cfg, tt.environ)
			if tt.wantErr {
				if err == nil {
					t.Fatal("applyEnvOverrides() = nil error, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("applyEnvOverrides() error = %v", err)
			}
			if tt.check != nil {
				tt.check(t, cfg)
			}
		})
	}
}

func TestLoadEnvOverrideIntegration(t *testing.T) {
	t.Setenv("BUGBOT_STORAGE_PATH", "/env/override.db")
	path := writeTemp(t, validYAML+"storage:\n  path: /file/value.db\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Storage.Path != "/env/override.db" {
		t.Errorf("env override lost: storage.path = %q, want /env/override.db", cfg.Storage.Path)
	}
}

func TestAPIKey(t *testing.T) {
	cfg := Default()
	cfg.Providers["anthropic"] = Provider{Type: ProviderAnthropic, APIKeyEnv: "TEST_BUGBOT_KEY"}

	if _, err := cfg.APIKey("unknown"); err == nil {
		t.Error("APIKey(unknown) = nil error, want error")
	}
	if _, err := cfg.APIKey("anthropic"); err == nil {
		t.Error("APIKey with unset env = nil error, want error")
	}

	t.Setenv("TEST_BUGBOT_KEY", "secret-value")
	got, err := cfg.APIKey("anthropic")
	if err != nil {
		t.Fatalf("APIKey() error = %v", err)
	}
	if got != "secret-value" {
		t.Errorf("APIKey() = %q, want secret-value", got)
	}
}

func TestStarterYAMLIsValid(t *testing.T) {
	// The starter config shipped by `bugbot init` must itself load and validate
	// cleanly (so a fresh user can run scan immediately after setting keys).
	path := writeTemp(t, StarterYAML)
	if _, err := Load(path); err != nil {
		t.Fatalf("StarterYAML failed to load/validate: %v", err)
	}
}

func TestValidate_CacheReadWeightBounds(t *testing.T) {
	load := func(t *testing.T, w float64) *Config {
		t.Helper()
		cfg, err := Load(writeTemp(t, validYAML))
		if err != nil {
			t.Fatalf("load baseline: %v", err)
		}
		cfg.Budgets.CacheReadWeight = w
		return &cfg
	}
	for _, w := range []float64{-0.1, 1.1, 2.0} {
		if err := load(t, w).Validate(); err == nil || !strings.Contains(err.Error(), "cache_read_weight") {
			t.Errorf("cache_read_weight=%v should be rejected, got %v", w, err)
		}
	}
	for _, w := range []float64{0, 0.1, 0.5, 1.0} {
		if err := load(t, w).Validate(); err != nil {
			t.Errorf("cache_read_weight=%v should be valid, got %v", w, err)
		}
	}
}

func TestEnvOverride_CacheReadWeight(t *testing.T) {
	cfg := Default()
	if err := applyEnvOverrides(&cfg, []string{"BUGBOT_BUDGETS_CACHE_READ_WEIGHT=0.25"}); err != nil {
		t.Fatal(err)
	}
	if cfg.Budgets.CacheReadWeight != 0.25 {
		t.Errorf("env override = %v, want 0.25", cfg.Budgets.CacheReadWeight)
	}
}
