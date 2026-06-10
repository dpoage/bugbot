// Package config defines Bugbot's typed configuration, loaded from a YAML file
// with BUGBOT_* environment-variable overrides applied on top.
//
// Secrets are never stored in the config file. Provider API keys are referenced
// by the NAME of an environment variable (api_key_env); the value is read from
// the process environment at use time.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ProviderType enumerates the supported LLM provider backends.
type ProviderType string

const (
	ProviderAnthropic        ProviderType = "anthropic"
	ProviderOpenAI           ProviderType = "openai"
	ProviderGoogle           ProviderType = "google"
	ProviderOpenAICompatible ProviderType = "openai-compatible"
)

// validProviderTypes is the set of accepted provider type values.
var validProviderTypes = map[ProviderType]bool{
	ProviderAnthropic:        true,
	ProviderOpenAI:           true,
	ProviderGoogle:           true,
	ProviderOpenAICompatible: true,
}

// Provider describes a single LLM provider endpoint. The API key itself is
// never stored here: APIKeyEnv names the environment variable that holds it.
type Provider struct {
	Type      ProviderType `yaml:"type"`
	BaseURL   string       `yaml:"base_url,omitempty"`
	APIKeyEnv string       `yaml:"api_key_env"`
}

// RoleModel binds a pipeline role to a provider and model.
type RoleModel struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
}

// Roles maps the three pipeline roles to their provider/model selection.
type Roles struct {
	Finder     RoleModel `yaml:"finder"`
	Verifier   RoleModel `yaml:"verifier"`
	Reproducer RoleModel `yaml:"reproducer"`
}

// Budgets caps token spend per investigation cycle and per day.
type Budgets struct {
	PerCycleTokens int64 `yaml:"per_cycle_tokens"`
	PerDayTokens   int64 `yaml:"per_day_tokens"`
}

// Scan controls which files are considered during ingest/scan.
type Scan struct {
	Include []string `yaml:"include"`
	Exclude []string `yaml:"exclude"`
}

// Sandbox configures the isolated execution environment used for verification
// and reproduction.
type Sandbox struct {
	Backend        string `yaml:"backend"`
	Runtime        string `yaml:"runtime"`
	Image          string `yaml:"image"`
	CPUs           int    `yaml:"cpus"`
	MemoryMB       int    `yaml:"memory_mb"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
	Network        string `yaml:"network"`
}

// Report configures where findings are written and which sinks receive them.
type Report struct {
	Dir   string   `yaml:"dir"`
	Sinks []string `yaml:"sinks"`
}

// validSeverities is the set of accepted severity values.
var validSeverities = map[string]bool{
	"critical": true,
	"high":     true,
	"medium":   true,
	"low":      true,
}

// Verify configures the empirical sandbox-execution capability offered to
// refuter agents in the adversarial verification stage.
type Verify struct {
	// SandboxExec enables the sandbox_exec tool for refuter agents. Default off.
	SandboxExec bool `yaml:"sandbox_exec"`
	// SandboxMinSeverity is the minimum candidate severity that qualifies for
	// the sandbox_exec tool. Only candidates at or above this severity receive the
	// tool; others rely on rhetorical reasoning alone. Valid values:
	// critical|high|medium|low. Default "high".
	SandboxMinSeverity string `yaml:"sandbox_min_severity"`
	// SandboxMaxExecs is the per-candidate execution budget: a refuter panel for
	// one candidate may issue at most this many sandbox_exec calls in total.
	// Must be >= 1. Default 3.
	SandboxMaxExecs int `yaml:"sandbox_max_execs"`
}

// Review configures the `bugbot review --pr N` PR-review mode.
type Review struct {
	// FailOn controls the CI exit gate. "verified" (default) makes the command
	// exit nonzero when the run surfaces a NEW Tier<=2 finding not already posted
	// on the PR. "none" never fails the gate on findings.
	FailOn string `yaml:"fail_on"`
	// Suspected controls how Tier-3 (suspected) findings are surfaced. "summary"
	// (default) lists them in the summary comment. "withhold" omits them entirely.
	Suspected string `yaml:"suspected"`
}

// Repro configures the Reproduce stage and the follow-on Patch-prover stage.
type Repro struct {
	// PatchProver enables the patch-prover stage.  When true, a successful
	// repro triggers a follow-on agent that attempts to produce a minimal fix
	// and proves it with a sandboxed suite run.  Default false.
	PatchProver bool `yaml:"patch_prover"`
	// PatchMaxAttempts is the maximum number of fix plans tried per finding
	// before giving up and flagging the finding needs-human.  Must be >= 1.
	// Default 3.
	PatchMaxAttempts int `yaml:"patch_max_attempts"`
}

// Daemon configures the continuous-run scheduler.
type Daemon struct {
	PollInterval  time.Duration `yaml:"poll_interval"`
	SweepInterval time.Duration `yaml:"sweep_interval"`
	IdleBackoff   time.Duration `yaml:"idle_backoff"`
}

// Storage configures the embedded SQLite state store.
type Storage struct {
	Path string `yaml:"path"`
}

// Publish configures the `bugbot publish` command and daemon post-cycle hook
// that files open findings as GitHub issues via the gh CLI.
//
// enabled gates the daemon hook only — the manual `bugbot publish` command
// always works regardless of this flag.
//
// tier_min is the maximum tier to publish (inclusive): findings with
// Tier <= tier_min are filed. Default 2 publishes T1 and T2 but not T3.
// Tier 0 is strongest (reproduced), 3 is weakest (suspected). Valid range 0..3.
//
// labels is the set of labels applied to every filed issue. Default ["bugbot"].
//
// close_on_fixed controls whether fixed/dismissed findings have their GitHub
// issue closed by the reconciler. Default true.
//
// Env overrides:
//
//	BUGBOT_PUBLISH_ENABLED       ("true" or "false")
//	BUGBOT_PUBLISH_TIER_MIN      (integer 0..3)
//	BUGBOT_PUBLISH_LABELS        (comma-separated label names)
//	BUGBOT_PUBLISH_CLOSE_ON_FIXED ("true" or "false")
type Publish struct {
	Enabled      bool     `yaml:"enabled"`
	TierMin      int      `yaml:"tier_min"`
	Labels       []string `yaml:"labels"`
	CloseOnFixed bool     `yaml:"close_on_fixed"`
}

// Config is the root configuration object.
type Config struct {
	Providers map[string]Provider `yaml:"providers"`
	Roles     Roles               `yaml:"roles"`
	Budgets   Budgets             `yaml:"budgets"`
	Scan      Scan                `yaml:"scan"`
	Sandbox   Sandbox             `yaml:"sandbox"`
	Verify    Verify              `yaml:"verify"`
	Repro     Repro               `yaml:"repro"`
	Report    Report              `yaml:"report"`
	Review    Review              `yaml:"review"`
	Publish   Publish             `yaml:"publish"`
	Daemon    Daemon              `yaml:"daemon"`
	Storage   Storage             `yaml:"storage"`
}

// Default returns a Config populated with sane defaults. Callers typically
// overlay a loaded YAML file and then env-var overrides on top.
func Default() Config {
	return Config{
		Providers: map[string]Provider{},
		Budgets: Budgets{
			PerCycleTokens: 200_000,
			PerDayTokens:   5_000_000,
		},
		Scan: Scan{
			Include: []string{"**/*"},
			Exclude: []string{
				".git/**",
				"vendor/**",
				"node_modules/**",
				"**/*_test.go",
			},
		},
		Sandbox: Sandbox{
			Backend:        "cli",
			Runtime:        "podman",
			Image:          "docker.io/library/debian:stable-slim",
			CPUs:           2,
			MemoryMB:       2048,
			TimeoutSeconds: 600,
			Network:        "none",
		},
		Verify: Verify{
			SandboxExec:        false,
			SandboxMinSeverity: "high",
			SandboxMaxExecs:    3,
		},
		Repro: Repro{
			PatchProver:      false,
			PatchMaxAttempts: 3,
		},
		Report: Report{
			Dir:   ".bugbot/reports",
			Sinks: []string{"fs"},
		},
		Review: Review{
			FailOn:    "verified",
			Suspected: "summary",
		},
		Publish: Publish{
			Enabled:      false,
			TierMin:      2,
			Labels:       []string{"bugbot"},
			CloseOnFixed: true,
		},
		Daemon: Daemon{
			PollInterval:  60 * time.Second,
			SweepInterval: 6 * time.Hour,
			IdleBackoff:   5 * time.Minute,
		},
		Storage: Storage{
			Path: ".bugbot/state.db",
		},
	}
}

// Load reads the config file at path, overlays it on the defaults, applies
// BUGBOT_* environment-variable overrides, and validates the result.
func Load(path string) (Config, error) {
	cfg := Default()

	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}

	// Unmarshal directly onto the defaults so unspecified fields are retained.
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %q: %w", path, err)
	}

	if err := applyEnvOverrides(&cfg, os.Environ()); err != nil {
		return Config{}, fmt.Errorf("apply env overrides: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// applyEnvOverrides applies BUGBOT_* overrides from the supplied environment
// (in "KEY=VALUE" form, as returned by os.Environ). Recognized keys:
//
//	BUGBOT_STORAGE_PATH
//	BUGBOT_REPORT_DIR
//	BUGBOT_BUDGETS_PER_CYCLE_TOKENS
//	BUGBOT_BUDGETS_PER_DAY_TOKENS
//	BUGBOT_SANDBOX_RUNTIME
//	BUGBOT_SANDBOX_IMAGE
//	BUGBOT_SANDBOX_NETWORK
//	BUGBOT_SANDBOX_CPUS
//	BUGBOT_SANDBOX_MEMORY_MB
//	BUGBOT_SANDBOX_TIMEOUT_SECONDS
//	BUGBOT_DAEMON_POLL_INTERVAL
//	BUGBOT_DAEMON_SWEEP_INTERVAL
//	BUGBOT_DAEMON_IDLE_BACKOFF
//	BUGBOT_REVIEW_FAIL_ON
//	BUGBOT_REVIEW_SUSPECTED
//	BUGBOT_VERIFY_SANDBOX_EXEC        ("true" or "false")
//	BUGBOT_VERIFY_SANDBOX_MIN_SEVERITY (critical|high|medium|low)
//	BUGBOT_VERIFY_SANDBOX_MAX_EXECS   (integer >= 1)
//	BUGBOT_PUBLISH_ENABLED            ("true" or "false")
//	BUGBOT_PUBLISH_TIER_MIN           (integer 0..3)
//	BUGBOT_PUBLISH_LABELS             (comma-separated label names)
//	BUGBOT_PUBLISH_CLOSE_ON_FIXED     ("true" or "false")
//	BUGBOT_REPRO_PATCH_PROVER         ("true" or "false")
//	BUGBOT_REPRO_PATCH_MAX_ATTEMPTS   (integer >= 1)
//	BUGBOT_REPRO_SUITE_CMD            (comma-separated argv)
func applyEnvOverrides(cfg *Config, environ []string) error {
	env := make(map[string]string, len(environ))
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if ok && strings.HasPrefix(k, "BUGBOT_") {
			env[k] = v
		}
	}

	setStr := func(key string, dst *string) {
		if v, ok := env[key]; ok {
			*dst = v
		}
	}
	setBool := func(key string, dst *bool) error {
		if v, ok := env[key]; ok {
			switch strings.ToLower(v) {
			case "true", "1", "yes":
				*dst = true
			case "false", "0", "no":
				*dst = false
			default:
				return fmt.Errorf("%s: invalid boolean value %q (want true or false)", key, v)
			}
		}
		return nil
	}
	setInt := func(key string, dst *int) error {
		if v, ok := env[key]; ok {
			n, err := strconv.Atoi(v)
			if err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			*dst = n
		}
		return nil
	}
	setInt64 := func(key string, dst *int64) error {
		if v, ok := env[key]; ok {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			*dst = n
		}
		return nil
	}
	setDur := func(key string, dst *time.Duration) error {
		if v, ok := env[key]; ok {
			d, err := time.ParseDuration(v)
			if err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			*dst = d
		}
		return nil
	}

	setStr("BUGBOT_STORAGE_PATH", &cfg.Storage.Path)
	setStr("BUGBOT_REPORT_DIR", &cfg.Report.Dir)
	setStr("BUGBOT_SANDBOX_RUNTIME", &cfg.Sandbox.Runtime)
	setStr("BUGBOT_SANDBOX_IMAGE", &cfg.Sandbox.Image)
	setStr("BUGBOT_SANDBOX_NETWORK", &cfg.Sandbox.Network)
	setStr("BUGBOT_REVIEW_FAIL_ON", &cfg.Review.FailOn)
	setStr("BUGBOT_REVIEW_SUSPECTED", &cfg.Review.Suspected)
	setStr("BUGBOT_VERIFY_SANDBOX_MIN_SEVERITY", &cfg.Verify.SandboxMinSeverity)

	// BUGBOT_PUBLISH_LABELS is comma-separated; an explicit env var replaces the
	// whole slice rather than appending, matching the override pattern elsewhere.
	if v, ok := env["BUGBOT_PUBLISH_LABELS"]; ok {
		var labels []string
		for _, l := range strings.Split(v, ",") {
			if trimmed := strings.TrimSpace(l); trimmed != "" {
				labels = append(labels, trimmed)
			}
		}
		cfg.Publish.Labels = labels
	}

	for _, err := range []error{
		setInt64("BUGBOT_BUDGETS_PER_CYCLE_TOKENS", &cfg.Budgets.PerCycleTokens),
		setInt64("BUGBOT_BUDGETS_PER_DAY_TOKENS", &cfg.Budgets.PerDayTokens),
		setInt("BUGBOT_SANDBOX_CPUS", &cfg.Sandbox.CPUs),
		setInt("BUGBOT_SANDBOX_MEMORY_MB", &cfg.Sandbox.MemoryMB),
		setInt("BUGBOT_SANDBOX_TIMEOUT_SECONDS", &cfg.Sandbox.TimeoutSeconds),
		setInt("BUGBOT_VERIFY_SANDBOX_MAX_EXECS", &cfg.Verify.SandboxMaxExecs),
		setInt("BUGBOT_PUBLISH_TIER_MIN", &cfg.Publish.TierMin),
		setInt("BUGBOT_REPRO_PATCH_MAX_ATTEMPTS", &cfg.Repro.PatchMaxAttempts),
		setDur("BUGBOT_DAEMON_POLL_INTERVAL", &cfg.Daemon.PollInterval),
		setDur("BUGBOT_DAEMON_SWEEP_INTERVAL", &cfg.Daemon.SweepInterval),
		setDur("BUGBOT_DAEMON_IDLE_BACKOFF", &cfg.Daemon.IdleBackoff),
		setBool("BUGBOT_VERIFY_SANDBOX_EXEC", &cfg.Verify.SandboxExec),
		setBool("BUGBOT_PUBLISH_ENABLED", &cfg.Publish.Enabled),
		setBool("BUGBOT_PUBLISH_CLOSE_ON_FIXED", &cfg.Publish.CloseOnFixed),
		setBool("BUGBOT_REPRO_PATCH_PROVER", &cfg.Repro.PatchProver),
	} {
		if err != nil {
			return err
		}
	}

	return nil
}

// Validate checks the config for internal consistency and returns a helpful
// error describing the first problem found.
func (c *Config) Validate() error {
	if len(c.Providers) == 0 {
		return fmt.Errorf("config: at least one provider must be defined under `providers`")
	}

	for name, p := range c.Providers {
		if !validProviderTypes[p.Type] {
			return fmt.Errorf("config: provider %q has invalid type %q (want one of anthropic, openai, google, openai-compatible)", name, p.Type)
		}
		if p.APIKeyEnv == "" {
			return fmt.Errorf("config: provider %q must set `api_key_env` (the NAME of the env var holding the key)", name)
		}
		if p.Type == ProviderOpenAICompatible && p.BaseURL == "" {
			return fmt.Errorf("config: provider %q has type openai-compatible but no `base_url`", name)
		}
	}

	roles := map[string]RoleModel{
		"finder":     c.Roles.Finder,
		"verifier":   c.Roles.Verifier,
		"reproducer": c.Roles.Reproducer,
	}
	for role, rm := range roles {
		if rm.Provider == "" {
			return fmt.Errorf("config: role %q must set `provider`", role)
		}
		if rm.Model == "" {
			return fmt.Errorf("config: role %q must set `model`", role)
		}
		if _, ok := c.Providers[rm.Provider]; !ok {
			return fmt.Errorf("config: role %q references unknown provider %q", role, rm.Provider)
		}
	}

	if c.Budgets.PerCycleTokens <= 0 {
		return fmt.Errorf("config: budgets.per_cycle_tokens must be > 0")
	}
	if c.Budgets.PerDayTokens <= 0 {
		return fmt.Errorf("config: budgets.per_day_tokens must be > 0")
	}
	if c.Budgets.PerCycleTokens > c.Budgets.PerDayTokens {
		return fmt.Errorf("config: budgets.per_cycle_tokens (%d) must not exceed budgets.per_day_tokens (%d)", c.Budgets.PerCycleTokens, c.Budgets.PerDayTokens)
	}

	switch c.Sandbox.Runtime {
	case "podman", "docker":
	default:
		return fmt.Errorf("config: sandbox.runtime %q invalid (want podman or docker)", c.Sandbox.Runtime)
	}
	if c.Sandbox.CPUs <= 0 {
		return fmt.Errorf("config: sandbox.cpus must be > 0")
	}
	if c.Sandbox.MemoryMB <= 0 {
		return fmt.Errorf("config: sandbox.memory_mb must be > 0")
	}
	if c.Sandbox.TimeoutSeconds <= 0 {
		return fmt.Errorf("config: sandbox.timeout_seconds must be > 0")
	}

	if c.Storage.Path == "" {
		return fmt.Errorf("config: storage.path must not be empty")
	}

	switch c.Review.FailOn {
	case "", "verified", "none":
	default:
		return fmt.Errorf("config: review.fail_on %q invalid (want verified or none)", c.Review.FailOn)
	}
	switch c.Review.Suspected {
	case "", "summary", "withhold":
	default:
		return fmt.Errorf("config: review.suspected %q invalid (want summary or withhold)", c.Review.Suspected)
	}

	if c.Verify.SandboxMinSeverity != "" && !validSeverities[c.Verify.SandboxMinSeverity] {
		return fmt.Errorf("config: verify.sandbox_min_severity %q invalid (want critical, high, medium, or low)", c.Verify.SandboxMinSeverity)
	}
	if c.Verify.SandboxMaxExecs < 1 {
		return fmt.Errorf("config: verify.sandbox_max_execs must be >= 1 (got %d)", c.Verify.SandboxMaxExecs)
	}

	if c.Publish.TierMin < 0 || c.Publish.TierMin > 3 {
		return fmt.Errorf("config: publish.tier_min %d invalid (want 0..3)", c.Publish.TierMin)
	}
	if c.Repro.PatchMaxAttempts < 1 {
		return fmt.Errorf("config: repro.patch_max_attempts must be >= 1 (got %d)", c.Repro.PatchMaxAttempts)
	}

	return nil
}

// APIKey resolves the API key for the named provider by reading the environment
// variable named by its api_key_env field. It returns an error if the provider
// is unknown or the environment variable is unset/empty.
func (c *Config) APIKey(provider string) (string, error) {
	p, ok := c.Providers[provider]
	if !ok {
		return "", fmt.Errorf("config: unknown provider %q", provider)
	}
	key := os.Getenv(p.APIKeyEnv)
	if key == "" {
		return "", fmt.Errorf("config: provider %q api key env var %q is not set", provider, p.APIKeyEnv)
	}
	return key, nil
}
