# Harbor bug-finding benchmark (spike)

Runs bugbot and general coding agents on the same find-the-bug tasks with
[Harbor](https://harborframework.com), scored against real historical bugs.
It complements `internal/eval` and does not replace it: `internal/eval` is the
offline, deterministic CI gate (scripted/replayed LLMs, funnel-stage
attribution); this benchmark makes live model calls, runs each trial in its
own container, and compares bugbot against other agents.

## Tasks

`adapter.py` converts Harbor's `swebench-verified@1.0` tasks:

- **Environment:** the instance's prebuilt SWE-bench image (repo at the
  buggy base commit). Git history is squashed to a single commit, so the fix
  can't be recovered from later commits.
- **Instruction:** audit a scope of the repo and write
  `/logs/agent/findings.json`. The issue text is withheld. The scope is the
  gold file's directory (`dir/*.py`, not recursive) or, with
  `--scope file`, the gold file alone. It is also written to
  `/task/scope.txt`.
- **Ground truth:** one site per gold-patch hunk, covering the old-side line
  span that the hunk changes. It lives in `tests/`, which Harbor uploads only
  after the agent finishes.
- **Verifier:** `template/score.py` applies the `internal/eval` matching
  rule: same file, line within the site span ± tolerance (default 5), and
  greedy 1:1 matching so duplicate findings can't inflate true positives.
  `reward.json` holds the raw counts `detected`, `site_tp`, `site_fn`, `fp`,
  `findings` and `valid_output`. Bugbot rows also get `verified_*` counts,
  restricted to tier ≤ 2.

Precision is a **lower bound**: a finding outside the gold patch can still
be a real bug that nobody labeled.

## Agents

- `bugbot_agent:BugbotAgent` uploads a static bugbot binary and points every
  role at `-m provider/model` (known provider: `minimax`; override with
  `--ak base_url=… --ak api_key_env=…`). It scans `/testbed` restricted to
  the scope and exports `report list --json`. The Reproduce stage is off.
  Agent kwargs: `per_cycle_tokens` (default 15M), `concurrency`,
  `scan_timeout_sec` (default 6900, under the 7200 s agent timeout, so a slow
  scan still exports its findings and spend), `structured_output`, `binary`,
  `finder_exec` (default `on`; writes `scan.finder_exec`, the arm under
  test), `sandbox_backend` (default `unsandboxed`; writes
  `sandbox.backend`), `conda_env` (default empty; SWE-bench tasks set
  `testbed`).
- Baselines: any Harbor agent. `baselines:ClaudeCodeReadable` is
  `claude-code` with its logs made world-readable after the run. Under
  rootless podman, the host-side Harbor process otherwise cannot read the
  session files, which loses token usage and the trajectory and skips secret
  scrubbing. Pointing it at MiniMax's Anthropic-compatible endpoint runs the
  same model as bugbot, so the comparison isolates the harness.

### Finder exec bench mode

Each task container runs under rootless podman and holds no nested podman or
bubblewrap, so bugbot cannot isolate finder probes inside it. The agent
therefore writes `sandbox.backend: unsandboxed` (the `sandbox_backend`
default) plus `scan.finder_exec` (the `finder_exec` kwarg) into bugbot.yaml:
`run_code` probes run directly in the task container, on the host network,
with no isolation. bugbot never auto-selects this backend; the scan log
prints an `UNSANDBOXED` banner naming it. Set `finder_exec: off` for the
control arm of an A/B pair. With `conda_env: testbed`, the run script
activates `/opt/miniconda3/envs/testbed` before the scan, so probes import
the project's dependencies.

After the scan, the agent copies `state.db` and tars `.bugbot/transcripts`
to `/logs/agent/` (both best-effort), so a run can be autopsied after the
container is gone. `scorecard.py` counts `rl_units` per arm: finder units
lost to provider throttling, read from `agent/scan.log`.

## Run

```bash
uv tool install harbor                # 0.24.0 at the time of writing
bench/harbor/build.sh                 # -> bench/harbor/bin/bugbot
harbor download swebench-verified@1.0 -o /tmp/hb
python3 bench/harbor/adapter.py --source /tmp/hb/swebench-verified \
    --out bench/harbor/out/bugfind-swebench --instances bench/harbor/instances.txt

set -a; . ~/.config/bugbot/env; set +a
# Rootless podman as `docker` needs the compose v2 plugin in
# ~/.docker/cli-plugins. Silence its banner: it otherwise pollutes the
# stdout of exec calls inside the container.
export PODMAN_COMPOSE_WARNING_LOGS=false

PYTHONPATH=bench/harbor harbor run -y -p bench/harbor/out/bugfind-swebench \
    -a bugbot_agent:BugbotAgent -m minimax/MiniMax-M3 -n 2 \
    -o bench/harbor/jobs --job-name bugbot-minimax

ANTHROPIC_BASE_URL=https://api.minimax.io/anthropic ANTHROPIC_API_KEY=$MINIMAX_API_KEY \
PYTHONPATH=bench/harbor harbor run -y -p bench/harbor/out/bugfind-swebench \
    -a baselines:ClaudeCodeReadable -m MiniMax-M3 -n 2 \
    -o bench/harbor/jobs --job-name cc-minimax

python3 bench/harbor/scorecard.py bench/harbor/jobs/bugbot-minimax bench/harbor/jobs/cc-minimax \
    --price 0.3,0.06,1.2   # MiniMax-M3 list USD/Mtok: input, cache read, output
```

`scorecard.py` pools the counts across tasks and adds tokens, cost, agent
wall time and crashes for each agent and model. Harbor 0.24 ignores a
`metric.py` in a local dataset directory, so the pooling happens here.
Pass `--price` to compare costs: Claude Code reports a `cost_usd` based on
Anthropic rates even when it is pointed at another vendor.

**Don't run two jobs on the same task at the same time.** Harbor names each
task's image `hb__<task hash>`, so every job shares it, and trial teardown
runs `compose down --rmi local`. Under podman, one job's teardown removed the
image out from under the other job's running trial, and both trials died
(exit 137 / "no container with ID"). Run agents one after the other, or pass
`--no-delete` and clean up afterwards. To rerun only the crashed tasks, use
`-i <task>` in a new job and give `scorecard.py` both job dirs: for each
agent, model and task, the latest trial wins.

## Baselines

`results/<date>-<label>/` keeps each recorded baseline: `scorecard.json`
(the `--json` output) and a `README.md` that covers the setup, the results
and any deviations. Job directories stay local and gitignored. Compare new
runs against the latest baseline:
[2026-10-22, MiniMax-M3](results/2026-10-22-minimax-m3/README.md).

To record a baseline:

1. Build with `build.sh`. It writes the bugbot commit to `bin/bugbot.version`,
   and Harbor reports that commit as the agent version.
2. Run each agent as one job, one agent at a time.
3. Write `scorecard.py --price … --json results/<date>-<label>/scorecard.json`
   from the job directories.
4. Write the baseline `README.md` with the setup and any reruns or
   timeouts.
