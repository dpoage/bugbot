# Baseline: bugbot vs claude-code on MiniMax-M3 (2026-10-22)

This is the first recorded baseline of the Harbor bug-finding benchmark. Both
agents use the same model on the same 5 tasks. Claude Code finds 3 of 5 gold
bugs and bugbot finds 1 of 5. With 1 attempt on each of 5 tasks, treat the gap as
a signal, not a measurement. `scorecard.json` holds the full record: aggregates,
per-trial rewards and tokens, and every finding marked with the gold site it
matched.

## Results

| agent | bugs found | site recall | precision (floor) | false positives per task | input tokens (cache reads) | output tokens | cost at list price | cost per bug found | agent time |
|---|---|---|---|---|---|---|---|---|---|
| bugbot | 1/5 | 0.14 | 0.03 | 5.8 | 94.9M (80.6M) | 1.66M | $11.13 | $11.13 | 237 min |
| claude-code | 3/5 | 0.43 | 0.07 | 7.6 | 118.7M (113.4M) | 1.60M | $10.33 | $3.44 | 252 min |

| task | gold site | bugbot | claude-code |
|---|---|---|---|
| astropy-14995 | `astropy/nddata/mixins/ndarithmetic.py:523-527` | miss (4 findings, 22 min) | **hit** at :523 (3 findings, 24 min) |
| django-11099 | `django/contrib/auth/validators.py:10-11, 20-21` | miss (0 findings, 39 min) | miss (0 findings, 86 min) |
| xarray-4075 | `xarray/core/weighted.py:145-146` | miss (12 findings, 100 min) | **hit** at :145 (18 findings, 57 min) |
| pylint-4970 | `pylint/checkers/similar.py:392-393` | miss (8 findings, 64 min) | miss (13 findings, 54 min) |
| sklearn-13142 | `sklearn/mixture/base.py:260-264, 275-276` | **hit** at :263 (6 findings, 12 min) | **hit** at :263 (7 findings, 32 min) |

## How to read the numbers

- **Bugs found** is the headline: a task counts if one finding matches any of
  its gold sites.
- **Site recall** counts each gold-patch hunk as its own site. Two tasks have 2
  hunks for 1 bug, so site recall is lower than bug recall for both agents.
- **Precision is a floor.** A finding outside the gold patch counts as a false
  positive, but many look like real bugs that nobody labeled. For example, both
  agents flagged `pylint/checkers/base.py:1318` (keyword-only dangerous
  defaults are skipped).
- **Cost** uses MiniMax-M3 list prices for context ≤ 512K: $0.30 input,
  $0.06 cache read and $1.20 output per million tokens. Claude Code's own
  `cost_usd` uses Anthropic prices, so the scorecard ignores it.

## Setup

| item | value |
|---|---|
| Harbor | 0.24.0, local Docker environment on rootless podman 5.8.7 |
| tasks | `swebench-verified@1.0` through `adapter.py`, `bench/harbor/instances.txt` at the time of the run |
| scope | the gold file's directory, `dir/*.py`, not recursive |
| line tolerance | 5 |
| model | MiniMax-M3 (`api.minimax.io`) |
| bugbot | built from `2737630`; `bugbot_agent:BugbotAgent -m minimax/MiniMax-M3`; `per_cycle_tokens` 15M, concurrency 4, Reproduce stage off |
| claude-code | 2.1.309; `baselines:ClaudeCodeReadable -m MiniMax-M3` through `https://api.minimax.io/anthropic` |
| attempts | 1 per task and agent |

## Deviations from a clean run

- In the first jobs, both agents ran astropy and django at the same time. One
  trial's teardown removed the shared image, which killed the other trial (see
  the README for the benchmark). The scorecard excludes those 4 crashed trials.
  It uses the reruns, because the latest trial for each agent and task wins.
- The agent timeout changed from 3600 s to 7200 s during the run. The first
  bugbot xarray trial timed out at 3600 s with no output, and the scorecard
  uses its 7200 s rerun instead. The claude-code django rerun also timed out
  at 3600 s; its 7200 s rerun took 86 min.
- Five trials ran under the 3600 s limit: bugbot pylint and sklearn, and
  claude-code xarray, pylint and sklearn. All five completed without a
  timeout, so the lower limit did not change their results. Harbor reports
  64 min of agent time for bugbot pylint, although that trial did not time
  out.

## Reproduce

The job directories are not committed: Claude Code session logs make them
large (about 110 MB). If the job directories are still present, regenerate
`scorecard.json` from them:

```bash
J=bench/harbor/jobs
python3 bench/harbor/scorecard.py $J/spike-bugbot-minimax $J/spike-bugbot-minimax-rerun \
    $J/spike-bugbot-minimax-rerun-xarray $J/spike-cc-minimax $J/spike-cc-minimax-rerun \
    $J/spike-cc-minimax-rerun-django --price 0.3,0.06,1.2 \
    --json bench/harbor/results/2026-10-22-minimax-m3/scorecard.json
```

To run the benchmark again, follow the Run section of the
[benchmark README](../../README.md). Model sampling is not deterministic, so
expect different findings on each run.
