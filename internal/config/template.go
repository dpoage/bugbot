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
# ---------------------------------------------------------------------------
sandbox:
  backend: cli
  runtime: podman              # podman | docker
  image: docker.io/library/debian:stable-slim
  cpus: 2
  memory_mb: 2048
  timeout_seconds: 600
  network: none

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
