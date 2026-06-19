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
		name     string
		yaml     string
		wantErr  bool
		checkErr string // substring required in err.Error() when wantErr
		check    func(t *testing.T, c Config)
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
			// budgets contract: 0 (or any negative) on either cap means
			// UNLIMITED for that axis. The validator must accept it instead
			// of erroring out — otherwise the docs and the validator disagree
			// and the user cannot express "unlimited day" or "unlimited
			// cycle" via the YAML. (Inline config; not validYAML+override,
			// because YAML forbids duplicate top-level keys.)
			name: "budgets both zero (unlimited) validates",
			yaml: `
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
  per_cycle_tokens: 0
  per_day_tokens: 0
`,
			wantErr: false,
			check: func(t *testing.T, c Config) {
				if c.Budgets.PerCycleTokens != 0 {
					t.Errorf("per_cycle_tokens = %d, want 0", c.Budgets.PerCycleTokens)
				}
				if c.Budgets.PerDayTokens != 0 {
					t.Errorf("per_day_tokens = %d, want 0", c.Budgets.PerDayTokens)
				}
			},
		},
		{
			// Negative is also unlimited (matches consumer treat-<=0-as-
			// unlimited). Cover the boundary at -1 to make the policy
			// explicit and resist accidental flip to a strict > 0 check.
			name: "budgets negative values validate as unlimited",
			yaml: `
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
  per_cycle_tokens: -1
  per_day_tokens: -100
`,
			wantErr: false,
		},
		{
			// Cross-check interaction: with per_day=0 (unlimited day), a
			// positive per_cycle must NOT be rejected as cycle>day. The
			// cross-check is meaningless when one side is unlimited.
			name: "per_cycle positive with per_day zero (unlimited day) validates",
			yaml: `
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
  per_cycle_tokens: 1000000
  per_day_tokens: 0
`,
			wantErr: false,
		},
		{
			// Symmetric case: per_day positive with per_cycle=0 (unlimited
			// cycle) is also fine.
			name: "per_day positive with per_cycle zero (unlimited cycle) validates",
			yaml: `
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
  per_cycle_tokens: 0
  per_day_tokens: 1000000
`,
			wantErr: false,
		},
		{
			// The pre-existing "per_cycle > per_day" guard must still fire
			// when BOTH caps are finite positive. Inline config (not
			// validYAML+override) so the YAML parse succeeds and the
			// validator is the source of the error.
			name: "per_cycle exceeding per_day still rejected (positive/positive)",
			yaml: `
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
  per_cycle_tokens: 2000000
  per_day_tokens: 1000000
`,
			wantErr:  true,
			checkErr: "must not exceed budgets.per_day_tokens",
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
				if tt.checkErr != "" && !strings.Contains(err.Error(), tt.checkErr) {
					t.Errorf("Load() error = %q, want substring %q", err.Error(), tt.checkErr)
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
			name: "bool override (cartographer)",
			environ: []string{
				"BUGBOT_SCAN_CARTOGRAPHER=true",
			},
			check: func(t *testing.T, c Config) {
				if !c.Scan.Cartographer {
					t.Error("scan.cartographer = false, want true from env override")
				}
			},
		},
		{
			name: "bool override (heat_ordering off)",
			environ: []string{
				"BUGBOT_SCAN_HEAT_ORDERING=false",
			},
			check: func(t *testing.T, c Config) {
				if c.Scan.HeatOrdering {
					t.Error("scan.heat_ordering = true, want false from env override")
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

// ---------------------------------------------------------------------------
// New-field tests: BacklogBatch and ReproBacklogInterval.
// ---------------------------------------------------------------------------

func TestDefault_BacklogFields(t *testing.T) {
	cfg := Default()
	if cfg.Repro.BacklogBatch != 3 {
		t.Errorf("Repro.BacklogBatch default = %d, want 3", cfg.Repro.BacklogBatch)
	}
	if cfg.Daemon.ReproBacklogInterval != time.Hour {
		t.Errorf("Daemon.ReproBacklogInterval default = %s, want 1h", cfg.Daemon.ReproBacklogInterval)
	}
}

func TestLoad_BacklogFieldsFromYAML(t *testing.T) {
	yaml := validYAML + `
repro:
  backlog_batch: 7
daemon:
  repro_backlog_interval: 30m
`
	cfg, err := Load(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Repro.BacklogBatch != 7 {
		t.Errorf("BacklogBatch = %d, want 7", cfg.Repro.BacklogBatch)
	}
	if cfg.Daemon.ReproBacklogInterval != 30*time.Minute {
		t.Errorf("ReproBacklogInterval = %s, want 30m", cfg.Daemon.ReproBacklogInterval)
	}
}

func TestEnvOverride_BacklogBatch(t *testing.T) {
	cfg := Default()
	if err := applyEnvOverrides(&cfg, []string{"BUGBOT_REPRO_BACKLOG_BATCH=5"}); err != nil {
		t.Fatal(err)
	}
	if cfg.Repro.BacklogBatch != 5 {
		t.Errorf("BacklogBatch = %d, want 5", cfg.Repro.BacklogBatch)
	}
}

func TestEnvOverride_TranscriptDir(t *testing.T) {
	cfg := Default()
	if err := applyEnvOverrides(&cfg, []string{"BUGBOT_REPRO_TRANSCRIPT_DIR=/tmp/bugbot-transcripts"}); err != nil {
		t.Fatal(err)
	}
	if cfg.Repro.TranscriptDir != "/tmp/bugbot-transcripts" {
		t.Errorf("TranscriptDir = %q, want %q", cfg.Repro.TranscriptDir, "/tmp/bugbot-transcripts")
	}
}

func TestEnvOverride_ReproBacklogInterval(t *testing.T) {
	cfg := Default()
	if err := applyEnvOverrides(&cfg, []string{"BUGBOT_DAEMON_REPRO_BACKLOG_INTERVAL=2h"}); err != nil {
		t.Fatal(err)
	}
	if cfg.Daemon.ReproBacklogInterval != 2*time.Hour {
		t.Errorf("ReproBacklogInterval = %s, want 2h", cfg.Daemon.ReproBacklogInterval)
	}
}

func TestValidate_BacklogBatchMustBePositive(t *testing.T) {
	cfg, err := Load(writeTemp(t, validYAML))
	if err != nil {
		t.Fatalf("load baseline: %v", err)
	}
	cfg.Repro.BacklogBatch = 0
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "backlog_batch") {
		t.Errorf("backlog_batch=0 should be rejected with backlog_batch in message, got %v", err)
	}
	cfg.Repro.BacklogBatch = -1
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "backlog_batch") {
		t.Errorf("backlog_batch=-1 should be rejected, got %v", err)
	}
	cfg.Repro.BacklogBatch = 1
	if err := cfg.Validate(); err != nil {
		t.Errorf("backlog_batch=1 should be valid, got %v", err)
	}
}

func TestValidate_ReproBacklogIntervalMustBePositive(t *testing.T) {
	cfg, err := Load(writeTemp(t, validYAML))
	if err != nil {
		t.Fatalf("load baseline: %v", err)
	}
	cfg.Daemon.ReproBacklogInterval = 0
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "repro_backlog_interval") {
		t.Errorf("repro_backlog_interval=0 should be rejected, got %v", err)
	}
	cfg.Daemon.ReproBacklogInterval = -1
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "repro_backlog_interval") {
		t.Errorf("repro_backlog_interval=-1 should be rejected, got %v", err)
	}
	cfg.Daemon.ReproBacklogInterval = time.Minute
	if err := cfg.Validate(); err != nil {
		t.Errorf("repro_backlog_interval=1m should be valid, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// New-field tests: VerifyDrainInterval and ImpactSweepInterval (bugbot-vpx.5).
// ---------------------------------------------------------------------------

func TestDefault_DrainIntervals(t *testing.T) {
	cfg := Default()
	if cfg.Daemon.VerifyDrainInterval != time.Hour {
		t.Errorf("Daemon.VerifyDrainInterval default = %s, want 1h", cfg.Daemon.VerifyDrainInterval)
	}
	if cfg.Daemon.ImpactSweepInterval != 6*time.Hour {
		t.Errorf("Daemon.ImpactSweepInterval default = %s, want 6h", cfg.Daemon.ImpactSweepInterval)
	}
}

func TestLoad_DrainIntervalsFromYAML(t *testing.T) {
	yaml := validYAML + `
daemon:
  verify_drain_interval: 45m
  impact_sweep_interval: 12h
`
	cfg, err := Load(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Daemon.VerifyDrainInterval != 45*time.Minute {
		t.Errorf("VerifyDrainInterval = %s, want 45m", cfg.Daemon.VerifyDrainInterval)
	}
	if cfg.Daemon.ImpactSweepInterval != 12*time.Hour {
		t.Errorf("ImpactSweepInterval = %s, want 12h", cfg.Daemon.ImpactSweepInterval)
	}
}

func TestEnvOverride_DrainIntervals(t *testing.T) {
	cfg := Default()
	if err := applyEnvOverrides(&cfg, []string{
		"BUGBOT_DAEMON_VERIFY_DRAIN_INTERVAL=90m",
		"BUGBOT_DAEMON_IMPACT_SWEEP_INTERVAL=3h",
	}); err != nil {
		t.Fatal(err)
	}
	if cfg.Daemon.VerifyDrainInterval != 90*time.Minute {
		t.Errorf("VerifyDrainInterval = %s, want 90m", cfg.Daemon.VerifyDrainInterval)
	}
	if cfg.Daemon.ImpactSweepInterval != 3*time.Hour {
		t.Errorf("ImpactSweepInterval = %s, want 3h", cfg.Daemon.ImpactSweepInterval)
	}
}

func TestValidate_DrainIntervalsMustBePositive(t *testing.T) {
	cfg, err := Load(writeTemp(t, validYAML))
	if err != nil {
		t.Fatalf("load baseline: %v", err)
	}
	cfg.Daemon.VerifyDrainInterval = 0
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "verify_drain_interval") {
		t.Errorf("verify_drain_interval=0 should be rejected, got %v", err)
	}
	cfg.Daemon.VerifyDrainInterval = time.Minute
	cfg.Daemon.ImpactSweepInterval = -1
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "impact_sweep_interval") {
		t.Errorf("impact_sweep_interval=-1 should be rejected, got %v", err)
	}
	cfg.Daemon.ImpactSweepInterval = time.Hour
	if err := cfg.Validate(); err != nil {
		t.Errorf("positive drain intervals should be valid, got %v", err)
	}
}

func TestDefault_FinderHistoryTokensIsZero(t *testing.T) {
	// Config default is zero so the funnel applies its own
	// DefaultFinderHistoryTokens (compaction ON). A non-zero config default here
	// would override that funnel default and be harder to reason about.
	if got := Default().Budgets.FinderHistoryTokens; got != 0 {
		t.Errorf("default FinderHistoryTokens = %d, want 0 (defer to funnel default)", got)
	}
}

func TestEnvOverride_FinderHistoryTokens(t *testing.T) {
	cfg := Default()
	if err := applyEnvOverrides(&cfg, []string{"BUGBOT_BUDGETS_FINDER_HISTORY_TOKENS=42000"}); err != nil {
		t.Fatal(err)
	}
	if cfg.Budgets.FinderHistoryTokens != 42000 {
		t.Errorf("env override = %d, want 42000", cfg.Budgets.FinderHistoryTokens)
	}
}

func TestEnvOverride_FinderHistoryTokensDisable(t *testing.T) {
	cfg := Default()
	if err := applyEnvOverrides(&cfg, []string{"BUGBOT_BUDGETS_FINDER_HISTORY_TOKENS=-1"}); err != nil {
		t.Fatal(err)
	}
	if cfg.Budgets.FinderHistoryTokens != -1 {
		t.Errorf("env override = %d, want -1 (disable sentinel)", cfg.Budgets.FinderHistoryTokens)
	}
}

func TestDefault_FinderReadCapsAreZero(t *testing.T) {
	// Zero config defers to the funnel finder defaults (the cache-safe lever).
	d := Default().Budgets
	if d.FinderReadLines != 0 || d.FinderReadBytes != 0 {
		t.Errorf("default finder read caps = {%d %d}, want {0 0} (defer to funnel)", d.FinderReadLines, d.FinderReadBytes)
	}
}

func TestEnvOverride_FinderReadCaps(t *testing.T) {
	cfg := Default()
	if err := applyEnvOverrides(&cfg, []string{
		"BUGBOT_BUDGETS_FINDER_READ_LINES=500",
		"BUGBOT_BUDGETS_FINDER_READ_BYTES=65536",
	}); err != nil {
		t.Fatal(err)
	}
	if cfg.Budgets.FinderReadLines != 500 {
		t.Errorf("FinderReadLines = %d, want 500", cfg.Budgets.FinderReadLines)
	}
	if cfg.Budgets.FinderReadBytes != 65536 {
		t.Errorf("FinderReadBytes = %d, want 65536", cfg.Budgets.FinderReadBytes)
	}
}

func TestEnvOverride_TokenClaims(t *testing.T) {
	cfg := Default()
	if err := applyEnvOverrides(&cfg, []string{
		"BUGBOT_BUDGETS_FINDER_TOKEN_CLAIM=2000000",
		"BUGBOT_BUDGETS_VERIFIER_TOKEN_CLAIM=500000",
	}); err != nil {
		t.Fatal(err)
	}
	if cfg.Budgets.FinderTokenClaim != 2_000_000 {
		t.Errorf("FinderTokenClaim = %d, want 2000000", cfg.Budgets.FinderTokenClaim)
	}
	if cfg.Budgets.VerifierTokenClaim != 500_000 {
		t.Errorf("VerifierTokenClaim = %d, want 500000", cfg.Budgets.VerifierTokenClaim)
	}
}

func TestDefault_DepStrategyIsOff(t *testing.T) {
	if got := Default().Sandbox.DepStrategy; got != "off" {
		t.Errorf("default sandbox.dep_strategy = %q, want off", got)
	}
}

func TestEnvOverride_DepStrategy(t *testing.T) {
	cfg := Default()
	if err := applyEnvOverrides(&cfg, []string{"BUGBOT_SANDBOX_DEP_STRATEGY=host"}); err != nil {
		t.Fatal(err)
	}
	if cfg.Sandbox.DepStrategy != "host" {
		t.Errorf("dep_strategy override = %q, want host", cfg.Sandbox.DepStrategy)
	}
}

func TestValidate_DepStrategyEnum(t *testing.T) {
	cfg, err := Load(writeTemp(t, validYAML))
	if err != nil {
		t.Fatalf("load baseline: %v", err)
	}
	for _, ok := range []string{"", "off", "host", "fetch"} {
		cfg.Sandbox.DepStrategy = ok
		if err := cfg.Validate(); err != nil {
			t.Errorf("dep_strategy=%q should be valid, got %v", ok, err)
		}
	}
	cfg.Sandbox.DepStrategy = "bogus"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "dep_strategy") {
		t.Errorf("dep_strategy=bogus should be rejected with dep_strategy in message, got %v", err)
	}
}

// TestValidate_CartographerRoleOptional pins the optional cartographer role:
// unset is valid (falls back to finder); a partially-specified or
// unknown-provider mapping is rejected; a complete one is accepted.
func TestValidate_CartographerRoleOptional(t *testing.T) {
	base := func(t *testing.T) Config {
		t.Helper()
		cfg, err := Load(writeTemp(t, validYAML))
		if err != nil {
			t.Fatalf("load baseline: %v", err)
		}
		return cfg
	}
	cfg := base(t)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("baseline (no cartographer) should be valid: %v", err)
	}
	cfg = base(t)
	cfg.Roles.Cartographer = cfg.Roles.Finder
	if err := cfg.Validate(); err != nil {
		t.Errorf("complete cartographer role should be valid: %v", err)
	}
	cfg = base(t)
	cfg.Roles.Cartographer = RoleModel{Provider: cfg.Roles.Finder.Provider}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "cartographer") {
		t.Errorf("partial cartographer role should be rejected, got %v", err)
	}
	cfg = base(t)
	cfg.Roles.Cartographer = RoleModel{Provider: "does-not-exist", Model: "m"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Errorf("cartographer with unknown provider should be rejected, got %v", err)
	}
}

func TestValidate_SandboxIdleTimeout(t *testing.T) {
	load := func(t *testing.T) Config {
		t.Helper()
		cfg, err := Load(writeTemp(t, validYAML))
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		return cfg
	}

	// On by default.
	if got := Default().Sandbox.IdleTimeoutSeconds; got != 120 {
		t.Errorf("Default idle_timeout_seconds = %d, want 120", got)
	}

	// 0 disables the watchdog — valid.
	cfg := load(t)
	cfg.Sandbox.IdleTimeoutSeconds = 0
	if err := cfg.Validate(); err != nil {
		t.Errorf("idle_timeout_seconds=0 should be valid (disabled), got %v", err)
	}

	// Positive — valid.
	cfg = load(t)
	cfg.Sandbox.IdleTimeoutSeconds = 60
	if err := cfg.Validate(); err != nil {
		t.Errorf("idle_timeout_seconds=60 should be valid, got %v", err)
	}

	// Negative — rejected with a clear message.
	cfg = load(t)
	cfg.Sandbox.IdleTimeoutSeconds = -1
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "idle_timeout_seconds") {
		t.Errorf("idle_timeout_seconds=-1 should be rejected with idle_timeout_seconds in message, got %v", err)
	}
}

func TestDefault_LLMRequestTimeoutIsZero(t *testing.T) {
	// Zero config defers to the LLM package default (llm.DefaultRequestTimeout).
	if got := Default().LLM.RequestTimeout; got != 0 {
		t.Errorf("Default LLM.RequestTimeout = %s, want 0 (defer to LLM package default)", got)
	}
}

func TestLoad_LLMRequestTimeoutFromYAML(t *testing.T) {
	yaml := validYAML + `
llm:
  request_timeout: 90s
`
	cfg, err := Load(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.LLM.RequestTimeout != 90*time.Second {
		t.Errorf("LLM.RequestTimeout = %s, want 90s", cfg.LLM.RequestTimeout)
	}
}

func TestEnvOverride_LLMRequestTimeout(t *testing.T) {
	cfg := Default()
	if err := applyEnvOverrides(&cfg, []string{"BUGBOT_LLM_REQUEST_TIMEOUT=2m"}); err != nil {
		t.Fatal(err)
	}
	if cfg.LLM.RequestTimeout != 2*time.Minute {
		t.Errorf("env override = %s, want 2m", cfg.LLM.RequestTimeout)
	}
}

func TestValidate_LLMRequestTimeoutRejectsNegative(t *testing.T) {
	cfg, err := Load(writeTemp(t, validYAML))
	if err != nil {
		t.Fatalf("load baseline: %v", err)
	}
	cfg.LLM.RequestTimeout = -1 * time.Second
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "request_timeout") {
		t.Errorf("request_timeout=-1s should be rejected, got %v", err)
	}
	cfg.LLM.RequestTimeout = 0
	if err := cfg.Validate(); err != nil {
		t.Errorf("request_timeout=0 should be valid (use LLM package default), got %v", err)
	}
	cfg.LLM.RequestTimeout = 30 * time.Second
	if err := cfg.Validate(); err != nil {
		t.Errorf("request_timeout=30s should be valid, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// sandbox.setup_cmds and sandbox.local_mounts validation tests.
// ---------------------------------------------------------------------------

func TestValidate_SetupCmds(t *testing.T) {
	load := func(t *testing.T) Config {
		t.Helper()
		cfg, err := Load(writeTemp(t, validYAML))
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		return cfg
	}

	t.Run("empty setup_cmds is valid", func(t *testing.T) {
		cfg := load(t)
		cfg.Sandbox.SetupCmds = nil
		if err := cfg.Validate(); err != nil {
			t.Errorf("nil SetupCmds should be valid, got %v", err)
		}
	})

	t.Run("non-empty argv entries are valid", func(t *testing.T) {
		cfg := load(t)
		cfg.Sandbox.SetupCmds = [][]string{
			{"apt-get", "install", "-y", "libpq-dev"},
			{"protoc", "--version"},
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("valid SetupCmds should pass, got %v", err)
		}
	})

	t.Run("empty argv entry is invalid", func(t *testing.T) {
		cfg := load(t)
		cfg.Sandbox.SetupCmds = [][]string{
			{"apt-get", "install"},
			{}, // empty argv
		}
		err := cfg.Validate()
		if err == nil {
			t.Fatal("empty argv entry should be rejected")
		}
		if !strings.Contains(err.Error(), "setup_cmds[1]") {
			t.Errorf("error should mention setup_cmds[1], got %q", err.Error())
		}
	})

	t.Run("parse from yaml", func(t *testing.T) {
		yaml := validYAML + `
sandbox:
  setup_cmds:
    - ["apt-get", "install", "-y", "libpq-dev"]
    - ["sh", "-c", "echo hi"]
`
		cfg, err := Load(writeTemp(t, yaml))
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if len(cfg.Sandbox.SetupCmds) != 2 {
			t.Fatalf("want 2 setup_cmds, got %d", len(cfg.Sandbox.SetupCmds))
		}
		if cfg.Sandbox.SetupCmds[0][0] != "apt-get" {
			t.Errorf("setup_cmds[0][0] = %q, want apt-get", cfg.Sandbox.SetupCmds[0][0])
		}
	})
}

func TestValidate_LocalMounts(t *testing.T) {
	load := func(t *testing.T) Config {
		t.Helper()
		cfg, err := Load(writeTemp(t, validYAML))
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		return cfg
	}

	t.Run("empty local_mounts is valid", func(t *testing.T) {
		cfg := load(t)
		cfg.Sandbox.LocalMounts = nil
		if err := cfg.Validate(); err != nil {
			t.Errorf("nil LocalMounts should be valid, got %v", err)
		}
	})

	t.Run("valid mount passes", func(t *testing.T) {
		hostDir := t.TempDir()
		cfg := load(t)
		cfg.Sandbox.LocalMounts = []LocalMount{
			{Host: hostDir, Container: "/sibling"},
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("valid mount should pass, got %v", err)
		}
	})

	t.Run("relative host path is invalid", func(t *testing.T) {
		cfg := load(t)
		cfg.Sandbox.LocalMounts = []LocalMount{
			{Host: "relative/path", Container: "/sibling"},
		}
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "host") {
			t.Errorf("relative host path should be rejected with 'host' in message, got %v", err)
		}
	})

	t.Run("relative container path is invalid", func(t *testing.T) {
		hostDir := t.TempDir()
		cfg := load(t)
		cfg.Sandbox.LocalMounts = []LocalMount{
			{Host: hostDir, Container: "relative/ctr"},
		}
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "container") {
			t.Errorf("relative container path should be rejected with 'container' in message, got %v", err)
		}
	})

	t.Run("duplicate container path is invalid", func(t *testing.T) {
		hostDir1 := t.TempDir()
		hostDir2 := t.TempDir()
		cfg := load(t)
		cfg.Sandbox.LocalMounts = []LocalMount{
			{Host: hostDir1, Container: "/shared"},
			{Host: hostDir2, Container: "/shared"},
		}
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "duplicated") {
			t.Errorf("duplicate container path should be rejected, got %v", err)
		}
	})

	t.Run("missing host dir is invalid", func(t *testing.T) {
		cfg := load(t)
		cfg.Sandbox.LocalMounts = []LocalMount{
			{Host: "/this/path/does/not/exist/at/all", Container: "/sibling"},
		}
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "existing directory") {
			t.Errorf("missing host dir should be rejected with 'existing directory' in message, got %v", err)
		}
	})

	t.Run("parse from yaml", func(t *testing.T) {
		hostDir := t.TempDir()
		yaml := validYAML + "sandbox:\n  local_mounts:\n    - host: " + hostDir + "\n      container: /sibling\n"
		cfg, err := Load(writeTemp(t, yaml))
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if len(cfg.Sandbox.LocalMounts) != 1 {
			t.Fatalf("want 1 local_mount, got %d", len(cfg.Sandbox.LocalMounts))
		}
		if cfg.Sandbox.LocalMounts[0].Host != hostDir {
			t.Errorf("host = %q, want %q", cfg.Sandbox.LocalMounts[0].Host, hostDir)
		}
		if cfg.Sandbox.LocalMounts[0].Container != "/sibling" {
			t.Errorf("container = %q, want /sibling", cfg.Sandbox.LocalMounts[0].Container)
		}
	})
}

// TestScanHeatOrdering covers the three acceptance criteria for the
// HeatOrdering config knob:
//  1. Default is ON (true) when the field is absent from YAML.
//  2. Explicit false in YAML loads correctly.
//  3. Env override BUGBOT_SCAN_HEAT_ORDERING toggles it.
func TestScanHeatOrdering(t *testing.T) {
	t.Run("default ON when absent from YAML", func(t *testing.T) {
		cfg, err := Load(writeTemp(t, validYAML))
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if !cfg.Scan.HeatOrdering {
			t.Error("scan.heat_ordering = false, want true (default ON)")
		}
	})

	t.Run("false loads from YAML", func(t *testing.T) {
		yaml := validYAML + "scan:\n  heat_ordering: false\n"
		cfg, err := Load(writeTemp(t, yaml))
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.Scan.HeatOrdering {
			t.Error("scan.heat_ordering = true, want false from YAML")
		}
	})

	t.Run("env override false disables", func(t *testing.T) {
		t.Setenv("BUGBOT_SCAN_HEAT_ORDERING", "false")
		cfg, err := Load(writeTemp(t, validYAML))
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.Scan.HeatOrdering {
			t.Error("scan.heat_ordering = true after env BUGBOT_SCAN_HEAT_ORDERING=false, want false")
		}
	})

	t.Run("env override true enables", func(t *testing.T) {
		yaml := validYAML + "scan:\n  heat_ordering: false\n"
		t.Setenv("BUGBOT_SCAN_HEAT_ORDERING", "true")
		cfg, err := Load(writeTemp(t, yaml))
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if !cfg.Scan.HeatOrdering {
			t.Error("scan.heat_ordering = false after env BUGBOT_SCAN_HEAT_ORDERING=true, want true")
		}
	})
}
