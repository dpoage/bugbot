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

## Quickstart

Requires Go 1.26+ to build, and podman or docker for the repro stage
(optional — everything else works without a container runtime).

```sh
make build                     # static binary at bin/bugbot

cd /path/to/target/repo
bugbot init                    # writes a commented bugbot.yaml
export ANTHROPIC_API_KEY=...   # or whichever provider(s) you configure
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
