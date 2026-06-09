# Bugbot Architecture

Bugbot is an autonomous, continuously-running agentic harness that finds and
reports bugs in a target codebase using LLMs. It is a CLI-first, single static
Go binary.

## Guiding principles

- **Precision over recall.** Better to surface 3 real bugs than 10 probable
  ones. Everything below is tuned to suppress noise, not maximize hits.
- **Adversarial verification.** Every hypothesis from a finder agent is
  challenged by an independent refuter (verifier) agent before it can be
  reported. Disagreement demotes a finding.
- **Reproduction is the gold standard.** A finding backed by a sandboxed,
  deterministically failing test is worth more than any amount of LLM
  confidence.
- **Suppression memory.** Dismissed findings persist and are never
  re-reported; the system learns what the maintainers consider non-bugs.

## Pipeline

```
Ingest -> Hypothesize -> Triage -> Verify -> Reproduce -> Report
          (parallel              (adversarial  (sandboxed
           finder agents)         refuters)     failing test)
```

1. **Ingest** — build a repo model, fingerprint files/symbols, poll git for new
   commits, and compute the blast radius of a change.
2. **Hypothesize** — parallel finder agents propose candidate bugs.
3. **Triage** — dedup, cluster, and prioritize candidates; drop the obvious
   noise before spending expensive verification budget.
4. **Verify** — adversarial refuter agents try to disprove each candidate.
   Survivors become Tier 2.
5. **Reproduce** — generate a sandboxed failing test. Success promotes the
   finding to Tier 1.
6. **Report** — emit findings to markdown / SARIF and configured sinks.

## Confidence tiers

| Tier | Name       | Meaning                                          | Default |
| ---- | ---------- | ------------------------------------------------ | ------- |
| T1   | Reproduced | A sandboxed failing test demonstrates the bug.   | shown   |
| T2   | Verified   | Survived adversarial verification, no repro yet. | shown   |
| T3   | Suspected  | Hypothesized but not verified.                   | suppressed |

## Planned packages

- **`internal/llm`** — provider-agnostic LLM abstraction with capability
  profiles (context window, tool support, cost). Backends: Anthropic, OpenAI,
  Google, and any OpenAI-compatible endpoint. Roles (finder / verifier /
  reproducer) are mapped to provider+model in config for tiering.
- **`internal/store`** — embedded SQLite state (`modernc.org/sqlite`, pure-Go,
  CGO-free): findings, suppressions, ingest watermarks, and token spend.
- **`internal/ingest`** — repo model, file/symbol fingerprints, git polling,
  and blast-radius computation for incremental scans.
- **`internal/agent`** — tool-loop harness driving an LLM through a bounded
  set of tools, with per-cycle token budgets and recorded transcripts.
- **`internal/funnel`** — the pipeline stages above, wired together with
  backpressure and budget enforcement between stages.
- **`internal/sandbox`** — pluggable `Exec` interface for isolated execution.
  Initial backend is CLI-driven (shells out to podman/docker); network defaults
  to `none`.
- **`internal/report`** — markdown and SARIF emitters plus pluggable sinks.
- **`internal/config`** — typed config, YAML loading, `BUGBOT_*` env overrides.
  Secrets are referenced by env-var NAME, never stored.
- **`internal/cli`** — cobra command tree (`init`, `scan`, `daemon`, `report`).
- **daemon scheduler** — continuous operation: commit-triggered investigations,
  periodic sweeps, per-cycle/per-day token budgets, and idle backoff.

## Current status

Scaffold only. `bugbot init` is implemented; `scan`, `daemon`, and `report`
are wired stubs that load and validate config but do not yet run the pipeline.
See the beads tracker (`bd show bugbot-v2f`) for the build plan.
