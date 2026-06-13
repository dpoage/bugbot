package config

// DefaultFileName is the conventional name of the Bugbot config file.
const DefaultFileName = "bugbot.yaml"

// StarterYAML is a commented starter configuration written by `bugbot init`.
// It documents every option and ships with conservative, precision-first
// defaults. API keys are referenced by environment-variable NAME only; the
// secret values are never written here.
const StarterYAML = `# Bugbot configuration.
#
# Secrets are NEVER stored in this file. Each provider names an environment
# variable (api_key_env) that holds its API key; Bugbot reads the value from the
# process environment at run time.

# ---------------------------------------------------------------------------
# providers: named LLM backends. type is one of:
#   anthropic | openai | google | openai-compatible
# base_url is optional except for openai-compatible (required).
# ---------------------------------------------------------------------------
providers:
  anthropic:
    type: anthropic
    api_key_env: ANTHROPIC_API_KEY
  openai:
    type: openai
    api_key_env: OPENAI_API_KEY
  # Example self-hosted / gateway endpoint:
  # local:
  #   type: openai-compatible
  #   base_url: http://localhost:11434/v1
  #   api_key_env: LOCAL_API_KEY
  #
  # OAuth bearer-token mode (for Claude Code subscription users without a
  # pay-per-token ANTHROPIC_API_KEY). Run "claude setup-token" once to populate
  # the env var, then restart bugbot. The token is long-lived but can be
  # refreshed at any time with the same command.
  #
  # anthropic-oauth:
  #   type: anthropic
  #   auth: oauth-token
  #   auth_token_env: CLAUDE_CODE_OAUTH_TOKEN

# ---------------------------------------------------------------------------
# roles: map each pipeline role to a provider+model. Tier strong models to
# verification, cheaper models to broad finding.
# ---------------------------------------------------------------------------
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

# ---------------------------------------------------------------------------
# budgets: token spend caps. per_cycle bounds a single investigation;
# per_day bounds total daemon spend across a 24h window.
# ---------------------------------------------------------------------------
budgets:
  per_cycle_tokens: 200000
  per_day_tokens: 5000000
  # cache reads bill at a fraction of full price; weight them ~0.1 against
  # the token budgets so a cache-heavy run isn't throttled by cheap tokens.
  cache_read_weight: 0.1
  # max_output_tokens caps each finder/verifier completion's VISIBLE output
  # (max_tokens). Reasoning models route their chain-of-thought through visible
  # output tokens, so the cap must cover the thinking PLUS the JSON answer; set
  # too low, a heavy reasoner exhausts its budget inside the <think> block and
  # emits no JSON ("empty model output"). It is an upper cap (no cost penalty for
  # raising it; smaller-window models are bounded by their own cap). Omit to use
  # the built-in default.
  # max_output_tokens: 32768

# ---------------------------------------------------------------------------
# scan: path globs (relative to the target repo root) selecting files to
# consider. exclude wins over include.
# ---------------------------------------------------------------------------
scan:
  include:
    - "**/*"
  exclude:
    - ".git/**"
    - "vendor/**"
    - "node_modules/**"
    - "**/*_test.go"

# ---------------------------------------------------------------------------
# sandbox: isolated execution environment for verification and reproduction.
# backend is currently "cli" (shells out to a container runtime).
# network defaults to "none" — reproduction runs offline.
#
# dep_strategy controls how a NON-vendored Go module resolves its external
# dependencies under network=none. Vendored repos (vendor/modules.txt) always
# build offline regardless of this setting.
#   off   (default) no dependency mounts; only vendored repos build offline.
#   host  mount the host's Go module cache read-only into the sandbox. Exposes
#         PUBLIC module source (never put secrets in your module cache).
#   fetch run one online "go mod download" in a hardened container to warm a
#         bugbot-managed cache, then mount it read-only; the test/build run that
#         follows is still network=none. The network is touched ONCE.
# ---------------------------------------------------------------------------
sandbox:
  backend: cli
  runtime: podman              # podman | docker
  image: docker.io/library/debian:stable-slim
  cpus: 2
  memory_mb: 2048
  timeout_seconds: 600
  network: none
  dep_strategy: off            # off | host | fetch

# ---------------------------------------------------------------------------
# verify: configuration for the LLM-assisted patch-verification stage.
# The verifier re-runs the sandbox (network=none) against the patched repo and
# confirms the reproduction test passes. dep_strategy must match the setting
# above (or be stricter) because the verify run is also network-none.
# ---------------------------------------------------------------------------
# verify:
#   enabled: true
#   timeout_seconds: 300       # per-run wall-clock cap for the verify sandbox

# ---------------------------------------------------------------------------
# repro: configuration for the reproduction-test generation stage.
# The reproducer writes a self-contained test, commits it to a workspace copy,
# and runs it inside the sandbox (network=none). dep_strategy in the sandbox
# section above applies here too — set host or fetch if the repo's modules are
# not vendored and the repro test imports them.
# ---------------------------------------------------------------------------
# repro:
#   enabled: true
#   max_attempts: 3            # how many patch-and-retry cycles before giving up
#   timeout_seconds: 600       # per-attempt wall-clock cap

# ---------------------------------------------------------------------------
# report: where findings are emitted and through which sinks.
# sinks: fs (writes report.md + report.sarif to dir) | stdout (more to come).
# ---------------------------------------------------------------------------
report:
  dir: .bugbot/reports
  sinks:
    - fs

# ---------------------------------------------------------------------------
# daemon: continuous-run scheduler timing (Go duration strings).
# ---------------------------------------------------------------------------
daemon:
  poll_interval: 60s           # how often to check for new commits
  sweep_interval: 6h           # full re-scan cadence
  idle_backoff: 5m             # wait after an idle cycle

# ---------------------------------------------------------------------------
# storage: embedded SQLite state (findings, suppressions, watermarks, spend).
# ---------------------------------------------------------------------------
storage:
  path: .bugbot/state.db
`
