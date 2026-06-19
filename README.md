# Bugbot

An autonomous, continuously-running agentic harness that finds and reports bugs
in a target codebase — with a bias for precision over recall. Better three real
bugs than ten probable ones.

Bugbot runs a multi-stage detection funnel where LLM agents hunt for bugs and
*other* LLM agents try to tear those findings apart. Only candidates that
survive adversarial verification get reported, and the best findings ship with
a runnable failing test produced and executed in a locked-down container
sandbox.

> **Status: early.** The pipeline, daemon, sandbox, and eval harness are built
> and tested, but Bugbot has not yet been battle-tested against large real-world
> codebases. Expect rough edges.

## How it works

```
Ingest → Hypothesize → Triage → Verify → Reproduce → Report
```

- **Hypothesize** — finder agents scan in parallel, each through a different
  lens: nil-safety/error-handling, concurrency, resource leaks, boundary
  conditions, API-contract misuse, injection.
- **Triage** — dedup, drop low-confidence candidates, and filter anything a
  human previously dismissed (suppression memory).
- **Verify** — each surviving candidate faces three adversarial refuter agents
  whose only goal is to prove it wrong. Majority-refuted candidates die.
- **Reproduce** — for verified findings, an agent writes a minimal failing test
  and runs it in a sandboxed container (podman/docker, no network, all
  capabilities dropped). A demonstrated failure promotes the finding.
- **Report** — markdown + SARIF 2.1, with the full verification reasoning and a
  link to the repro artifact.

Findings carry a confidence tier:

| Tier | Meaning |
|---|---|
| **T1 Reproduced** | A failing test was generated and actually executed — the report includes the runnable artifact |
| **T2 Verified** | Survived adversarial review with a concrete reasoning trace |
| **T3 Suspected** | Suppressed by default |

## Sandbox dependency strategies

The sandbox runs untrusted, model-generated code with `--network=none`, all
capabilities dropped, and a read-only root. That isolation means a build or test
can only resolve external packages if their source is already inside the
container. The `sandbox.dep_strategy` setting controls how that happens; each
ecosystem implements the same four strategies independently and the results are
merged when a repo spans multiple ecosystems (e.g. a Go service with a JS
frontend).

### Strategy overview

| Strategy | Meaning |
|---|---|
| **vendored** (auto-detected) | dependencies are already present in the repo tree; no mount or network needed |
| **off** (default) | no dependency mounts; builds that need external packages will fail offline |
| **host** | mount a slice of the host package cache read-only; cache miss → hard error |
| **fetch** | run ONE online prefetch in a hardened container, then mount the result read-only for all subsequent network-none runs |

### Per-ecosystem matrix

| Ecosystem | Detected by | Vendored means | `host` behavior | `fetch` prefetch command | Offline enforcement env | In-sandbox setup step |
|---|---|---|---|---|---|---|
| **Go** | `go.mod` | `vendor/modules.txt` exists → `GOFLAGS=-mod=vendor` | mount `$GOMODCACHE` at `/modcache` (read-only, `Shared=true`) | `go mod download all` into `/modcache` (writable) | `GOPROXY=off` | none |
| **Python** | `requirements.txt` | n/a (no vendored detection) | → **off** (pip HTTP cache does not materialize packages) | `pip download -r requirements.txt -d /depcache` into `/depcache` (writable) | `PIP_NO_INDEX=1` | `pip install --user --no-index --find-links=/depcache -r requirements.txt` |
| **Rust** | `Cargo.toml` | `vendor/` + `.cargo/config{.toml}` with `replace-with` stanza → `CARGO_NET_OFFLINE=true` | mount `$CARGO_HOME/registry` at `/cargo/registry` (read-only, `Shared=true`); `CARGO_HOME=/cargo` | `cargo fetch [--locked]` with `CARGO_HOME=/cargo` (writable); populates `/cargo/registry` | `CARGO_NET_OFFLINE=true` | none |
| **JS/npm** | `package.json` | `node_modules/` exists → no mounts needed | → **off** (npm HTTP cache does not materialize `node_modules`) | `npm ci --ignore-scripts --cache /npmcache` into `/npmcache` (writable) | `npm_config_offline=true` | `cp -a /npmcache /tmp/npmcache && npm ci --cache /tmp/npmcache` |

### Container mount paths (globally unique)

Each ecosystem owns a distinct container path so multi-ecosystem repos never
have mount collisions:

| Ecosystem | Container path | Purpose |
|---|---|---|
| Go | `/modcache` | Go module cache (`GOMODCACHE`) |
| Python | `/depcache` | pip wheelhouse |
| Rust | `/cargo/registry` | Cargo registry index + crate sources (`CARGO_HOME=/cargo`) |
| JS | `/npmcache` | npm HTTP cache (`--cache /npmcache`) |

### Security notes

- **Rust `host` strategy**: only `$CARGO_HOME/registry` is mounted — never all
  of `~/.cargo`, which contains `credentials.toml` and `bin/`. This is enforced
  in the resolver and asserted in unit tests.
- **JS `fetch` prefetch**: `--ignore-scripts` is **mandatory** in the online
  prefetch step. npm lifecycle scripts are arbitrary code; during the prefetch
  the container has network access, so executing them could exfiltrate data or
  contact external services. Scripts may run during the offline `npm ci` in the
  setup step, where arbitrary code execution is already the sandbox's threat
  model (network is none at that point).
- **Vendored detection runs in every mode** (it is free and safe). Vendored
  detection for Rust additionally requires a `.cargo/config{.toml}` source-
  replacement stanza — a bare `vendor/` directory without the config is ignored
  by cargo and falls through to the requested strategy.
- Read-only mounts are never writable; the writable workspace copy remains the
  only writable surface for the untrusted network-none run.

## Install

```sh
go install github.com/dpoage/bugbot@latest
```

That installs the latest `bugbot` into `$(go env GOPATH)/bin`. It needs only a
Go 1.25+ toolchain — `modernc.org/sqlite` is pure Go, so there is no C compiler
or CGO requirement, and the tree-sitter code-nav grammars are embedded in the
binary. `go install` builds the **full** binary: every grammar embedded, no
build tags required. podman or docker is needed only for the repro stage
(optional — everything else works without a container runtime).

For a ~21MB-smaller binary that embeds only the grammars the code-nav fallback
actually uses, build from a checkout instead (see [Development](#development)):

```sh
make build                     # static, CGO-free binary at bin/bugbot
```

## Quickstart

```sh
cd /path/to/target/repo
bugbot prime                   # repo-aware guidance for filling in bugbot.yaml
bugbot init                    # writes a commented bugbot.yaml
export ANTHROPIC_API_KEY=...   # or whichever provider(s) you configure
bugbot scan --estimate         # project token spend + wall time before running (no LLM calls)
bugbot scan                    # one-shot funnel run
bugbot scan --repro            # also attempt sandboxed reproductions
bugbot report list             # inspect findings
bugbot report dismiss <id> --reason "false positive: guarded by caller"
bugbot daemon                  # run continuously: poll commits, sweep, re-verify
```

Dismissing a finding records its fingerprint in a persistent suppression
memory — Bugbot will never re-report it.

## Provider-agnostic by design

Bugbot speaks to Anthropic, OpenAI, Google, and any OpenAI-compatible endpoint
(Ollama, vLLM, Groq, OpenRouter, ...). Pipeline roles map to models
independently, so you can put a cheap fast model on finding and your strongest
model on verification:

```yaml
providers:
  anthropic: { type: anthropic, api_key_env: ANTHROPIC_API_KEY }
  local:     { type: openai-compatible, base_url: http://localhost:11434/v1 }
roles:
  finder:     { provider: local,     model: qwen3-coder }
  verifier:   { provider: anthropic, model: claude-fable-5 }
  reproducer: { provider: anthropic, model: claude-fable-5 }
```

Token budgets (`per_cycle_tokens`, `per_day_tokens`) are first-class: the
daemon degrades gracefully as budget runs low and stops spending entirely when
it's gone — never silently.

## Continuous operation

`bugbot daemon` interleaves slow whole-repo sweeps with targeted
investigations triggered by new commits (scoped to the change's blast radius).
Open findings whose implicated code changes are automatically re-verified, and
fixed bugs are auto-closed. State lives in an embedded SQLite database — no
external services.

## Development

```sh
make test       # full suite (no network, no API keys, no containers needed)
make lint
go test -tags integration ./internal/sandbox/... ./internal/repro/...  # real containers
go test ./internal/eval/ -run TestBenchmarkSuite -v                    # precision/recall gate
```

See [ARCHITECTURE.md](ARCHITECTURE.md) for the component map. Issue tracking
uses [beads](https://github.com/steveyegge/beads) (`.beads/`).

## License

[AGPL-3.0](LICENSE). If you run a modified Bugbot as a network service, you
must make your modifications available to its users.
