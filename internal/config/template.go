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
  #   # Opt in to native grammar-constrained JSON output (provider response_format
  #   # / forced-tool schema) for endpoints that support it (e.g. MiniMax-M3).
  #   # Defaults to off for openai-compatible since arbitrary endpoints vary;
  #   # first-party anthropic/openai/google enable it automatically.
  #   structured_output: true
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
# per_day bounds total daemon spend across a 24h window. Set either to 0
# (or any negative value) for UNLIMITED spend on that axis — matching the
# consumer contract (funnel Options.TokenBudget, daemon day-spend check).
# When both are positive, per_cycle_tokens must not exceed per_day_tokens.
# ---------------------------------------------------------------------------
budgets:
  per_cycle_tokens: 200000
  per_day_tokens: 5000000
  # cache reads bill at a fraction of full price; weight them ~0.1 against
  # the token budgets so a cache-heavy run isn't throttled by cheap tokens.
  cache_read_weight: 0.1
  # fraction of per_cycle_tokens the finder stage may spend; the rest is
  # RESERVED for verification so finders can't drain the pool and leave every
  # candidate unverified. 0 uses the built-in default (0.7); 1.0 disables it.
  finder_budget_share: 0.7
  # per-task token claims for the claimant budget system: each finder/verifier
  # run is capped at this many tokens, so one breadth-heavy run can't be granted
  # a whole stage's reserve. Only tokens actually spent are charged, so a run
  # that finishes under its claim leaves the remainder for its siblings. 0 uses
  # the built-in default (1000000); a negative value removes the per-task cap.
  finder_token_claim: 1000000
  verifier_token_claim: 1000000

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
  # cartographer: summarize each package once (cached by content fingerprint)
  # and inject the relevant summaries into finder tasks so agents begin with
  # repo context instead of rediscovering it per turn. On by default; set to
  # false to disable. Optionally add a [roles.cartographer] block above to run
  # the summary pass on a cheaper model than the finder.
  cartographer: true

# ---------------------------------------------------------------------------
# sandbox: isolated execution environment for verification and reproduction.
# backend is currently "cli" (shells out to a container runtime).
# network defaults to "none" — reproduction runs offline.
#
# IMPORTANT: the reproduce/verify stages run the target repo's OWN test/build
# toolchain INSIDE this image under network=none. A toolchain-less image (such
# as the debian:stable-slim default below) makes every repro/verify exit with
# environment_error, so findings silently stay unreproduced. Set image to one
# that carries the target language's toolchain (Go -> golang:<ver>-alpine,
# Python -> python:3-slim, Node -> node:22-slim, Rust -> rust:1-slim,
# C/C++ -> gcc:14). Run "bugbot prime" in the target repo for a tailored pick.
#
# dep_strategy controls how a NON-vendored repo resolves its external
# dependencies under network=none. It applies per detected ecosystem (Go
# go.mod, Python requirements.txt, Rust Cargo.toml, JS package.json); already-
# vendored repos (Go vendor/modules.txt, Rust vendor/ + .cargo/config, committed
# node_modules/) build offline regardless of this setting.
#   off   (default) no dependency mounts; only vendored repos build offline.
#   host  mount the host's package cache read-only into the sandbox. Exposes
#         PUBLIC package source (never put secrets in your caches). Go and Rust
#         only; Python and JS fall back to off (their HTTP caches cannot install
#         offline) — use fetch for those.
#   fetch run one online prefetch (go mod download / pip download / cargo fetch /
#         npm ci) in a hardened container to warm a bugbot-managed cache, then
#         mount it read-only; the build/test run that follows is still
#         network=none. The network is touched ONCE.
# See the README "Sandbox dependency strategies" matrix for per-ecosystem detail.
# ---------------------------------------------------------------------------
sandbox:
  backend: cli
  runtime: podman              # podman | docker
  image: docker.io/library/debian:stable-slim   # see IMPORTANT note above; set a toolchain image for repro/verify
  cpus: 2
  memory_mb: 2048
  timeout_seconds: 600         # HARD ceiling for one sandbox run
  idle_timeout_seconds: 120    # kill a run only after this long with NO progress
                               # (output or workspace writes); 0 disables. The
                               # ceiling above still applies. Lets a slow-but-
                               # progressing build finish while killing hangs fast.
  network: none
  dep_strategy: off            # off | host | fetch
  # setup_cmds: pre-run commands (argv lists) executed BEFORE the main command
  # and BEFORE per-ecosystem offline installs, so system libs are present when
  # ecosystem tools run. Commands share the same network-none run — anything
  # needing the network must be baked into the image or handled by fetch.
  # Each entry is a non-empty argv; leave commented out (empty default).
  # setup_cmds:
  #   - ["apt-get", "install", "-y", "--no-install-recommends", "libpq-dev"]
  #   - ["protoc", "--version"]
  # local_mounts: read-only bind-mounts for on-disk deps (monorepo siblings,
  # locally-checked-out path deps). Orthogonal to dep_strategy — both may be
  # active at once. Paths are ONLY from this config (trusted boundary); paths
  # from in-repo manifests are a deliberate fast-follow (security gating).
  # Mounts are read-only with no SELinux :Z relabel (host-owned shared dirs).
  # local_mounts:
  #   - host: /absolute/path/to/sibling     # must exist on the host
  #     container: /sibling                  # absolute container path; unique

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
# llm: shared LLM client wrapper settings applied to every role's client.
# request_timeout is the per-attempt wall-clock deadline for one provider
# request (bounds a stalled HTTP round-trip). 0 / omitted uses the LLM
# package default (5m). Negative is rejected at load time.
# ---------------------------------------------------------------------------
llm:
  request_timeout: 5m

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
