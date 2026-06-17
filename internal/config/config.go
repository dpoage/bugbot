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

// Provider describes a single LLM provider endpoint.
//
// Secrets are never stored here. In api_key mode (the default), APIKeyEnv names
// the environment variable that holds the API key. In oauth-token mode,
// AuthTokenEnv names the environment variable that holds the OAuth bearer token
// — the credential Claude Code uses. Exactly one of the two credential fields
// must be set, matching the mode.
type Provider struct {
	Type      ProviderType `yaml:"type"`
	BaseURL   string       `yaml:"base_url,omitempty"`
	APIKeyEnv string       `yaml:"api_key_env,omitempty"`

	// Auth selects the credential mode. The empty string and "api_key" both
	// select API-key mode. "oauth-token" selects OAuth bearer-token mode, which
	// is only valid for type=anthropic. The Anthropic API rejects requests that
	// carry both an x-api-key and a Bearer token, so the two credential fields
	// are mutually exclusive per mode.
	Auth string `yaml:"auth,omitempty"`

	// AuthTokenEnv names the environment variable that holds the OAuth bearer
	// token. Required (and api_key_env must be empty) when auth=oauth-token.
	// Populate via `claude setup-token` (long-lived) or
	// `ant auth print-credentials --access-token` (short-lived).
	AuthTokenEnv string `yaml:"auth_token_env,omitempty"`

	// StructuredOutput overrides the provider's default StructuredOutput
	// capability. A pointer is used so unset (nil) is distinguishable from
	// explicit false: unset = use the adapter's built-in default (true for
	// first-party OpenAI / Anthropic / Google; false for arbitrary
	// openai-compatible endpoints). Set to true to opt an openai-compatible
	// endpoint (e.g. MiniMax) into schema-constrained output; set to false
	// to force-disable it on a provider that would otherwise default on.
	StructuredOutput *bool `yaml:"structured_output,omitempty"`
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

// Budgets caps token spend per investigation cycle and per day. Each cap is
// independent: a value of 0 (or any negative) means UNLIMITED for that knob,
// matching the consuming layers (funnel Options.TokenBudget, daemon day-spend
// tracking). When both caps are positive, per_cycle_tokens must not exceed
// per_day_tokens.
type Budgets struct {
	PerCycleTokens int64 `yaml:"per_cycle_tokens"`
	PerDayTokens   int64 `yaml:"per_day_tokens"`
	// CacheReadWeight discounts cache-read input tokens against the token
	// budgets (0..1). Cache reads bill at a fraction of full price; counting
	// them at full weight exhausts the budget far faster than real cost. Zero
	// uses the funnel default (~0.1). Set to 1.0 for raw-token accounting.
	CacheReadWeight float64 `yaml:"cache_read_weight"`
	// FinderHistoryTokens opts a finder into threshold-triggered history
	// compaction: once its growing message history exceeds this many (estimated)
	// tokens, older tool-result content is pruned to short stubs once per crossing.
	// It is OFF by default (0 and negative both disable it) because under a strong
	// prompt cache it raises cache-weighted cost; it is intended for weak-/no-cache
	// providers where the raw-token reduction is the real-dollar reduction. See
	// funnel.DefaultFinderHistoryTokens for the full rationale. The cache-safe
	// default lever is FinderReadLines/FinderReadBytes instead.
	FinderHistoryTokens int64 `yaml:"finder_history_tokens"`
	// FinderReadLines / FinderReadBytes tighten the per-read_file caps for finder
	// agents — the primary, cache-safe lever for finder token burn (bugbot-3nf).
	// Shrinking each read result at the source slows the growth of the re-sent
	// conversation history without mutating any earlier message, so the prompt
	// cache is preserved. Zero uses the funnel finder defaults
	// (funnel.DefaultFinderReadLines / DefaultFinderReadBytes); a negative value
	// restores the looser agent-package read defaults for the finder.
	FinderReadLines int `yaml:"finder_read_lines"`
	FinderReadBytes int `yaml:"finder_read_bytes"`
	// FinderBudgetShare is the fraction of per_cycle_tokens (0..1) the finder
	// stage may consume; the remainder is RESERVED for downstream verification
	// so the breadth-heavy finder stage cannot drain the whole per-cycle pool and
	// orphan every candidate before it is verified (bugbot-8mj). Zero defers to
	// the funnel default (funnel.DefaultFinderBudgetShare); 1.0 disables the
	// reservation (legacy: finders may use the whole budget).
	FinderBudgetShare float64 `yaml:"finder_budget_share"`
	// FinderTokenClaim / VerifierTokenClaim are the per-task token claims for the
	// claimant budget system: each finder or verifier agent run is capped at this
	// many tokens (its per-run budget), so a single breadth-heavy finder cannot
	// be granted the whole finder reserve in one run (bugbot-8mj). The reserved
	// sub-pool is charged only for tokens ACTUALLY spent, so a run that finishes
	// under its claim leaves the remainder available to its siblings — the claim
	// is "returned to the pool" by never being removed. Zero defers to the funnel
	// default (funnel.DefaultTokenClaim = 1_000_000). A negative value disables
	// the per-task cap for that role (each run may use its sub-pool remainder).
	FinderTokenClaim   int64 `yaml:"finder_token_claim"`
	VerifierTokenClaim int64 `yaml:"verifier_token_claim"`
}

// Scan controls which files are considered during ingest/scan.
type Scan struct {
	Include []string `yaml:"include"`
	Exclude []string `yaml:"exclude"`
	// Cartographer enables the per-package summary pass: a cheap one-shot LLM
	// summary per package, cached by content fingerprint and injected into
	// finder task messages so agents start with repo context instead of
	// rediscovering it via tool calls every turn (bugbot-mi5.7). Off by
	// default; the injection is append-only to the finder task and never
	// mutates the cached system-prompt prefix.
	Cartographer bool `yaml:"cartographer"`
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
	// DepStrategy selects how external module dependencies are made available to
	// the network-none sandbox for Go repos that are not vendored. Vendored repos
	// (vendor/modules.txt) are always detected and need no strategy. Values:
	//   off   (default) no dependency mounts; only vendored repos build offline.
	//   host  mount the host Go module cache read-only (exposes public module
	//         source — never put secrets in the module cache).
	//   fetch run one online `go mod download` in a hardened container to warm a
	//         bugbot-managed cache, then mount it read-only; everything after is
	//         network-none.
	DepStrategy string `yaml:"dep_strategy"`
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
	// SuiteCmd is the full-suite test command (argv) the patch-prover runs to
	// prove a fix keeps the whole suite green. Empty (the default) detects the
	// command from repo marker files (go.mod, Cargo.toml, package.json,
	// pyproject.toml/setup.py); when neither is available the patch-prover
	// skips rather than guessing.
	SuiteCmd []string `yaml:"suite_cmd"`
	// BacklogBatch is the maximum number of backlog findings the daemon submits
	// to the reproduce stage per backlog-timer firing. Must be >= 1. Default 3.
	// The batch cap prevents a large backlog from exhausting the per-day budget
	// in a single firing; the backlog drains gradually across multiple firings.
	BacklogBatch int `yaml:"backlog_batch"`
}

// Daemon configures the continuous-run scheduler.
type Daemon struct {
	PollInterval  time.Duration `yaml:"poll_interval"`
	SweepInterval time.Duration `yaml:"sweep_interval"`
	IdleBackoff   time.Duration `yaml:"idle_backoff"`
	// ReproBacklogInterval is the cadence at which the daemon drains the
	// reproduction backlog: open findings with no repro attempt (Tier 2 or 3,
	// ReproPath empty, NeedsHuman false). Must be > 0 when repro is enabled.
	// Default 1h. Only consulted when repro is enabled; otherwise ignored.
	ReproBacklogInterval time.Duration `yaml:"repro_backlog_interval"`
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
			PerCycleTokens:  200_000,
			PerDayTokens:    5_000_000,
			CacheReadWeight: 0.1,
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
			DepStrategy:    "off",
		},
		Verify: Verify{
			SandboxExec:        false,
			SandboxMinSeverity: "high",
			SandboxMaxExecs:    3,
		},
		Repro: Repro{
			PatchProver:      false,
			PatchMaxAttempts: 3,
			BacklogBatch:     3,
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
			PollInterval:         60 * time.Second,
			SweepInterval:        6 * time.Hour,
			IdleBackoff:          5 * time.Minute,
			ReproBacklogInterval: 1 * time.Hour,
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
//	BUGBOT_BUDGETS_CACHE_READ_WEIGHT
//	BUGBOT_BUDGETS_PER_DAY_TOKENS
//	BUGBOT_SANDBOX_RUNTIME
//	BUGBOT_SANDBOX_IMAGE
//	BUGBOT_SANDBOX_NETWORK
//	BUGBOT_SANDBOX_DEP_STRATEGY      (off|host|fetch)
//	BUGBOT_SANDBOX_CPUS
//	BUGBOT_SANDBOX_MEMORY_MB
//	BUGBOT_SANDBOX_TIMEOUT_SECONDS
//	BUGBOT_DAEMON_POLL_INTERVAL
//	BUGBOT_DAEMON_SWEEP_INTERVAL
//	BUGBOT_DAEMON_IDLE_BACKOFF
//	BUGBOT_DAEMON_REPRO_BACKLOG_INTERVAL
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
//	BUGBOT_REPRO_BACKLOG_BATCH        (integer >= 1)
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
	setFloat64 := func(key string, dst *float64) error {
		if v, ok := env[key]; ok {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			*dst = f
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
	setStr("BUGBOT_SANDBOX_DEP_STRATEGY", &cfg.Sandbox.DepStrategy)
	setStr("BUGBOT_REVIEW_FAIL_ON", &cfg.Review.FailOn)
	if v, ok := env["BUGBOT_REPRO_SUITE_CMD"]; ok {
		var cmd []string
		for _, part := range strings.Split(v, ",") {
			if p := strings.TrimSpace(part); p != "" {
				cmd = append(cmd, p)
			}
		}
		cfg.Repro.SuiteCmd = cmd
	}
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
		setFloat64("BUGBOT_BUDGETS_CACHE_READ_WEIGHT", &cfg.Budgets.CacheReadWeight),
		setFloat64("BUGBOT_BUDGETS_FINDER_BUDGET_SHARE", &cfg.Budgets.FinderBudgetShare),
		setInt64("BUGBOT_BUDGETS_FINDER_TOKEN_CLAIM", &cfg.Budgets.FinderTokenClaim),
		setInt64("BUGBOT_BUDGETS_VERIFIER_TOKEN_CLAIM", &cfg.Budgets.VerifierTokenClaim),
		setInt64("BUGBOT_BUDGETS_FINDER_HISTORY_TOKENS", &cfg.Budgets.FinderHistoryTokens),
		setInt("BUGBOT_BUDGETS_FINDER_READ_LINES", &cfg.Budgets.FinderReadLines),
		setInt("BUGBOT_BUDGETS_FINDER_READ_BYTES", &cfg.Budgets.FinderReadBytes),
		setInt("BUGBOT_SANDBOX_CPUS", &cfg.Sandbox.CPUs),
		setInt("BUGBOT_SANDBOX_MEMORY_MB", &cfg.Sandbox.MemoryMB),
		setInt("BUGBOT_SANDBOX_TIMEOUT_SECONDS", &cfg.Sandbox.TimeoutSeconds),
		setInt("BUGBOT_VERIFY_SANDBOX_MAX_EXECS", &cfg.Verify.SandboxMaxExecs),
		setInt("BUGBOT_PUBLISH_TIER_MIN", &cfg.Publish.TierMin),
		setInt("BUGBOT_REPRO_PATCH_MAX_ATTEMPTS", &cfg.Repro.PatchMaxAttempts),
		setInt("BUGBOT_REPRO_BACKLOG_BATCH", &cfg.Repro.BacklogBatch),
		setDur("BUGBOT_DAEMON_POLL_INTERVAL", &cfg.Daemon.PollInterval),
		setDur("BUGBOT_DAEMON_SWEEP_INTERVAL", &cfg.Daemon.SweepInterval),
		setDur("BUGBOT_DAEMON_IDLE_BACKOFF", &cfg.Daemon.IdleBackoff),
		setDur("BUGBOT_DAEMON_REPRO_BACKLOG_INTERVAL", &cfg.Daemon.ReproBacklogInterval),
		setBool("BUGBOT_VERIFY_SANDBOX_EXEC", &cfg.Verify.SandboxExec),
		setBool("BUGBOT_PUBLISH_ENABLED", &cfg.Publish.Enabled),
		setBool("BUGBOT_PUBLISH_CLOSE_ON_FIXED", &cfg.Publish.CloseOnFixed),
		setBool("BUGBOT_REPRO_PATCH_PROVER", &cfg.Repro.PatchProver),
		setBool("BUGBOT_SCAN_CARTOGRAPHER", &cfg.Scan.Cartographer),
	} {
		if err != nil {
			return err
		}
	}

	return nil
}

// validateProviderAuth enforces the credential-field constraints for a single
// provider. The rules are:
//
//   - auth="" or "api_key": api_key_env required; auth_token_env must be empty.
//   - auth="oauth-token":   type must be anthropic; auth_token_env required;
//     api_key_env must be empty (the Anthropic API rejects requests that carry
//     both an x-api-key and a Bearer token simultaneously).
//   - any other auth value: rejected with the list of valid values.
func validateProviderAuth(name string, p Provider) error {
	switch p.Auth {
	case "", "api_key":
		// API-key mode: api_key_env required; auth_token_env must be absent to
		// catch early confusion between the two credential fields.
		if p.APIKeyEnv == "" {
			return fmt.Errorf("config: provider %q must set `api_key_env` (the NAME of the env var holding the key)", name)
		}
		if p.AuthTokenEnv != "" {
			return fmt.Errorf("config: provider %q sets `auth_token_env` but auth mode is %q — remove `auth_token_env` or set `auth: oauth-token`", name, p.Auth)
		}
	case "oauth-token":
		// OAuth mode: anthropic only; auth_token_env required; api_key_env must
		// be empty — the Anthropic API rejects requests carrying both credentials.
		if p.Type != ProviderAnthropic {
			return fmt.Errorf("config: provider %q has auth=oauth-token but type=%q; oauth-token is only supported for type=anthropic", name, p.Type)
		}
		if p.AuthTokenEnv == "" {
			return fmt.Errorf("config: provider %q with auth=oauth-token must set `auth_token_env` (the NAME of the env var holding the bearer token)", name)
		}
		if p.APIKeyEnv != "" {
			return fmt.Errorf("config: provider %q sets both `api_key_env` and `auth_token_env`; the Anthropic API rejects requests carrying both credentials — remove `api_key_env` when using auth=oauth-token", name)
		}
	default:
		return fmt.Errorf("config: provider %q has invalid auth %q (valid values: api_key, oauth-token)", name, p.Auth)
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
		if err := validateProviderAuth(name, p); err != nil {
			return err
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

	// budgets.per_cycle_tokens and budgets.per_day_tokens are independent caps.
	// Each treats 0 (or any negative value) as UNLIMITED, matching the
	// consuming layers (funnel Options.TokenBudget, daemon.DaemonConfig.
	// {PerCycleTokens, PerDayTokens}, store day-spend tracking) — the user who
	// reads any consumer doc and sets 0 must not be told their config is
	// invalid. The cross-check below only applies when BOTH values are finite
	// positive; a zero on either side makes the comparison meaningless (the
	// unlimited side cannot be exceeded by construction).
	if c.Budgets.PerCycleTokens > 0 && c.Budgets.PerDayTokens > 0 && c.Budgets.PerCycleTokens > c.Budgets.PerDayTokens {
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
	switch c.Sandbox.DepStrategy {
	case "", "off", "host", "fetch":
	default:
		return fmt.Errorf("config: sandbox.dep_strategy %q invalid (want off, host, or fetch)", c.Sandbox.DepStrategy)
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

	if c.Budgets.CacheReadWeight < 0 || c.Budgets.CacheReadWeight > 1 {
		return fmt.Errorf("config: budgets.cache_read_weight %.3f invalid (want 0..1)", c.Budgets.CacheReadWeight)
	}
	// finder_budget_share is a fraction: 0 defers to the funnel default, 1.0
	// disables the reservation, and any value in between partitions the budget.
	// Negative or >1 is meaningless.
	if c.Budgets.FinderBudgetShare < 0 || c.Budgets.FinderBudgetShare > 1 {
		return fmt.Errorf("config: budgets.finder_budget_share %.3f invalid (want 0..1)", c.Budgets.FinderBudgetShare)
	}
	if c.Publish.TierMin < 0 || c.Publish.TierMin > 3 {
		return fmt.Errorf("config: publish.tier_min %d invalid (want 0..3)", c.Publish.TierMin)
	}
	if c.Repro.PatchMaxAttempts < 1 {
		return fmt.Errorf("config: repro.patch_max_attempts must be >= 1 (got %d)", c.Repro.PatchMaxAttempts)
	}
	if c.Repro.BacklogBatch < 1 {
		return fmt.Errorf("config: repro.backlog_batch must be >= 1 (got %d)", c.Repro.BacklogBatch)
	}
	// ReproBacklogInterval is only consulted when repro is enabled, but we
	// validate it unconditionally so a misconfiguration is caught at startup
	// rather than silently ignored.
	if c.Daemon.ReproBacklogInterval <= 0 {
		return fmt.Errorf("config: daemon.repro_backlog_interval must be > 0")
	}

	return nil
}

// APIKey resolves the API key for the named provider by reading the environment
// variable named by its api_key_env field. It returns an error if the provider
// is unknown or the environment variable is unset/empty.
//
// Callers that must work across both api_key and oauth-token providers should
// use Credential instead.
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

// Credential resolves the active credential for the named provider according to
// its configured auth mode:
//
//   - api_key mode (default): reads the env var named by api_key_env and returns
//     the API key string.
//   - oauth-token mode: reads the env var named by auth_token_env and returns the
//     OAuth bearer token. If the env var is unset or empty the error mentions the
//     env var name and instructs the user to mint a fresh token with
//     `claude setup-token` (the token may also be expired).
//
// The secret value is returned to the caller and must not be logged.
func (c *Config) Credential(provider string) (string, error) {
	p, ok := c.Providers[provider]
	if !ok {
		return "", fmt.Errorf("config: unknown provider %q", provider)
	}
	if p.Auth == "oauth-token" {
		token := os.Getenv(p.AuthTokenEnv)
		if token == "" {
			return "", fmt.Errorf("config: provider %q oauth token env var %q is not set or is empty (token may also be expired — run `claude setup-token` to mint a fresh one)", provider, p.AuthTokenEnv)
		}
		return token, nil
	}
	// api_key mode (auth="" or "api_key"): delegate to the existing resolver.
	return c.APIKey(provider)
}
