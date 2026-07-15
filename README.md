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

## Sandbox backend

`sandbox.backend` selects how the isolated environment described above is
built:

| Backend | Values | Platform | Isolation |
|---|---|---|---|
| **container** (default) | `""`, `cli`, `podman`, `docker` | Linux, macOS | podman/docker: baked image, `--network=none`, dropped capabilities, read-only root |
| **bwrap** | `bwrap` | Linux only, unprivileged user namespaces required | `bubblewrap`: tmpfs root + allowlisted read-only host binds, `--unshare-all`, no image to bake |

The container backend is the only option on macOS and remains the default
everywhere: it is what runs a completely hermetic, operator-baked image. The
bwrap backend trades that hermeticity for zero provisioning cost — it runs
directly on whatever toolchains the host already has (the host builds and
tests this repo daily, so it demonstrably has everything a container image
would otherwise need to be baked with). `bugbot doctor` reports which backend
is configured and rejects `backend: bwrap` with an actionable reason
(not Linux, `bwrap` missing from PATH, or unprivileged user namespaces
unavailable) before any run is attempted.

### Allowlist-bind security model (bwrap)

bwrap has no image filesystem to fall back on: the sandbox root starts as an
empty tmpfs, and ONLY the following are ever bound in, always read-only:

- a minimal fixed allowlist: `/usr`, `/lib`, `/lib64`, `/bin`, `/sbin`,
  `/etc/ssl` (plus `/etc/resolv.conf`, but only when `sandbox.network: host`
  is explicitly set — DNS is unreachable and unneeded under the default
  `network=none`);
- the store roots of store-based distros, when present: `/nix/store`,
  `/gnu/store`, and `/etc/static` (NixOS's symlink indirection into the
  store). On NixOS/Guix the allowlist paths above are symlink farms into
  the store — `/bin/sh` → `/nix/store/…-bash/bin/sh` — so without the store
  bind those symlinks dangle inside the sandbox and every `sh`-wrapped run
  fails with `execvp /bin/sh: No such file or directory`. Both stores are
  world-readable by design on their distros, so the read-only bind grants
  the sandboxed code nothing it could not already read unsandboxed as the
  same user; on FHS hosts the paths do not exist and the bind is a no-op;
- host toolchains resolved from `sandbox.host_toolchains` (bare names like
  `node`/`cargo`, resolved via the host's PATH and symlink-closure, or
  explicit absolute directories) — see `internal/sandbox/toolchain.go`;
- the prepared workspace copy, bound read-write at `/workspace` — the only
  writable mount, and the ONLY way the sandboxed process can persist
  anything.

`$HOME`, `/root`, and `/etc` wholesale are NEVER bound: a broader bind can
exfiltrate host secrets through workspace → transcript → LLM even under
`network=none`, since the untrusted command can read anything bound in and
the sandbox's output is fed straight back to a model. Widen the allowlist by
naming a toolchain in `sandbox.host_toolchains`, never by editing the fixed
list — and audit `host_toolchains` (and `sandbox.local_mounts`) with that
exposure in mind, since each entry grants the untrusted run read access to
whatever it resolves to.

Resource limits (`sandbox.cpus`/`memory_mb`/`pids_limit`) have no bwrap flag
equivalent — bwrap has no cgroups of its own. They are enforced via
`systemd-run --user --scope` when a user systemd session is reachable, else a
delegated cgroup v2 subtree; when NEITHER mechanism is available, a run
**fails** with an actionable error rather than silently running uncapped —
set `sandbox.allow_uncapped: true` to opt into that instead.

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
| **C#/NuGet** | root `*.csproj` / `*.sln` / `*.fsproj` | n/a (no vendored detection in v1) | mount `$NUGET_PACKAGES` (default `~/.nuget/packages`) at `/nugetcache` (read-only, `Shared=true`); `NUGET_PACKAGES=/nugetcache` | `dotnet restore [--locked-mode]` into `/nugetcache` (writable) | none — `--network=none` is the enforcement | none |
| **Maven** | root `pom.xml` | n/a (no vendored detection in v1) | mount `~/.m2/repository` at `/m2cache` (read-only, `Shared=true`); `MAVEN_OPTS=-Dmaven.repo.local=/m2cache` | `mvn -B dependency:go-offline` with `MAVEN_OPTS=-Dmaven.repo.local=/m2cache` (writable) | none — `--network=none` is the enforcement | none |
| **Gradle** | root `build.gradle[.kts]` / `settings.gradle[.kts]` | n/a (no vendored detection in v1) | → **off** (Gradle cache is lock-heavy under a read-only mount; see deps.go scope decisions) | `gradle dependencies --no-daemon -q` with `GRADLE_USER_HOME=/gradlecache` (writable) | none — `--network=none` is the enforcement | `mkdir -p /workspace/.bugbot-gradle-home && cp -a /gradlecache/. /workspace/.bugbot-gradle-home` (copy to disk-backed workspace; `GRADLE_USER_HOME=/workspace/.bugbot-gradle-home`) |

> **Bazel monorepos** use a custom image instead of dependency mounts — see
> [Offline Bazel sandbox image](#offline-bazel-sandbox-image) below.

### Container mount paths (globally unique)

Each ecosystem owns a distinct container path so multi-ecosystem repos never
have mount collisions:

| Ecosystem | Container path | Purpose |
|---|---|---|
| Go | `/modcache` | Go module cache (`GOMODCACHE`) |
| Python | `/depcache` | pip wheelhouse |
| Rust | `/cargo/registry` | Cargo registry index + crate sources (`CARGO_HOME=/cargo`) |
| JS | `/npmcache` | npm HTTP cache (`--cache /npmcache`) |
| C#/NuGet | `/nugetcache` | NuGet global packages folder (`NUGET_PACKAGES`) |
| Maven | `/m2cache` | Maven local repository (`-Dmaven.repo.local`) |
| Gradle | `/gradlecache` | Gradle user home (`GRADLE_USER_HOME`); copied to `/workspace/.bugbot-gradle-home` (disk-backed) at run time |

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
- **Maven `fetch` prefetch**: `mvn -B dependency:go-offline` instantiates POM plugins and any `.mvn/extensions.xml` build extensions at project-model load time. This executes repo-controlled Java code in an **online** (network-enabled) container. There is no Maven analog to npm's `--ignore-scripts`; the POM lifecycle is always evaluated. Accepted under the bugbot-gu0o posture (same as pip, dotnet). Mitigated by container hardening (cap-drop ALL, no-new-privileges, read-only root) and the absence of secret-bearing mounts during the prefetch.
- **Gradle `fetch` prefetch**: `gradle dependencies` evaluates `settings.gradle` and `build.gradle` (Groovy/Kotlin DSL) at configuration time. This executes repo-controlled code in an **online** container. There is no Gradle analog to `--ignore-scripts` — configuration code always runs and cannot be skipped without fundamentally changing how Gradle loads the project. Accepted under the bugbot-gu0o posture. Same mitigations as Maven above.
- Read-only mounts are never writable; the writable workspace copy remains the
  only writable surface for the untrusted network-none run.

## Offline Bazel sandbox image

Bazel monorepos do not fit the per-ecosystem dependency strategies above. Bugbot
runs `bazel test --build_tests_only //...` inside the sandbox under
`--network=none`, and Bazel needs three things present on disk to do that
offline: the **vendored external deps** (`bazel vendor` tree), a **prefetched,
content-addressed repository cache**, and a **warm disk cache**. `bugbot sandbox
build` generates a purpose-built image that carries all three, generalizing a
hand-built recipe into bugbot tooling.

### Two-phase build

1. **Base layer** — bake the `bazel vendor` tree and the repository cache into
   the image. They are *baked*, not bind-mounted, because Bazel's vendor mode
   writes a `bazel-external` symlink at run time and a read-only mount rejects
   that write; an image layer is writable through the container overlay.
2. **Warm layer** — run `bazel test --build_tests_only //...` once under
   `--network=none` to (a) prove the image builds and tests fully offline and
   (b) populate the disk cache, then `commit` the container as the final image.

### Why warm-cache-as-layer

Bugbot mounts everything read-only, and Bazel *disables* a read-only disk
cache — so a cache mounted in would be ignored and every run would recompile
from cold. Baking the warmed disk cache as an image layer sidesteps that: the
layer is writable through the per-run container overlay, so each Bugbot run
starts warm and its writes land in the throwaway overlay.

### Workflow

```sh
cd /path/to/bazel/repo
bugbot sandbox build            # scaffold bugbot-sandbox/{Dockerfile,build.sh}; print next steps
bazel vendor --vendor_dir=$HOME/.cache/<repo>-bugbot/vendor //...   # vendor deps (online, once)
./bugbot-sandbox/build.sh       # build base + warm offline + commit final image

# or do it all in one shot:
bugbot sandbox build --run      # vendor -> build -> warm run (network=none) -> commit
```

Either path commits `localhost/<repo>-bugbot-sandbox:latest` and refreshes
`sandbox.image` in `bugbot.yaml` to that tag (other keys preserved). Flags
override the output dir (`--out`), image tag (`--image`), Bazel version
(`--bazel-version`, defaults to the repo's `.bazelversion`), and vendor dir
(`--vendor-dir`). Without `--run` the command only scaffolds and prints — it
never shells out.

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

The state database is single-writer: each writing command (`scan`, `daemon`,
`review`, `repro`, `publish`, and `report dismiss`) takes an exclusive
cross-process lock on it, so a second writer refuses rather than racing —
concurrent writers were the cause of on-disk page corruption. Read-only
commands (`report list`/`show`/`emit`/`units`, plus `leads`, `metrics`,
`export`, `status`) run fine alongside a writer. The daemon runs a `PRAGMA
quick_check` at the start of every cycle; `bugbot doctor` reports integrity, and
`bugbot doctor --repair` (run with no writer active) backs up a corrupt database
and rebuilds it, salvaging readable rows.

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
