# Finder-exec production validation (2026-10-25, round ypei-r1)

This record validates the production `finder-exec` path (finder `run_code`)
through Harbor on the same 5 SWE-bench tasks as the
[2026-10-24 finder-recall loops](../2026-10-24-finder-recall/README.md). It
adds the fetch-free accounting that the 2026-10-24 addendum demanded: a hit
counts as fetch-free only when a finder unit that produced it made zero
network-fetching `run_code` calls (arbiter ruling E2).

**Status: pass 1 of 2.** Control and exec1 are complete and audited below.
exec2 and exec3 are still queued on the quota schedule; every exec2/exec3 cell
reads PENDING and the pass gate stays PENDING until pass 2 fills them.

## Arms

| arm | `finder_exec` | change |
|---|---|---|
| control | off | production scan without finder `run_code` |
| exec1 | on | production scan with finder `run_code` |
| exec2 | on | replicate 2 (PENDING) |
| exec3 | on | replicate 3 (PENDING) |

## Hits per task

Mark legend: **raw** = the verifier's line-tolerance matcher credits the
finding; **genuine** = a manual read against the gold patch and issue confirms
the same defect; **ff** = fetch-free (E2: at least one originating finder unit
with zero fetching `run_code` calls). Full evidence per hit: `hits.json`.

| task | control | exec1 | exec2 | exec3 |
|---|---|---|---|---|
| astropy-14995 | - | - | PENDING | PENDING |
| django-11099 | - | raw, genuine, ff | PENDING | PENDING |
| xarray-4075 | - | - | PENDING | PENDING |
| pylint-4970 | - | - | PENDING | PENDING |
| sklearn-13142 | - | raw, genuine (not ff) | PENDING | PENDING |

- **django-11099:** the hit names the gold defect (`$` anchor accepts a
  trailing newline in both username validators). Its single originating finder
  unit made no fetching calls, but it confirmed the bug by diffing `/testbed`
  against `/tmp/dj30`, a pristine Django-3.0 tree that another unit in the
  same trial pip-downloaded and unpacked (shared-/tmp read: yes).
- **sklearn-13142:** the hit names the gold defect (`fit_predict` labels come
  from the last initialization, not the restored best one) and both
  originating finder units proved it with local numpy probes in `/testbed`.
  Each unit also ran one `pip download sklearn==...` call, and both downloads
  failed (`No matching distribution found`), so no upstream content was
  retrieved — but E2 counts the call, so the hit is not fetch-free.
- Control found nothing (0 of 5), matching the 2026-10-24 control arm.

## Recall and tokens

Run tokens = `in_tok + out_tok`; cached tokens are inside `in_tok` and also
reported alone. Source: `control.json`, `exec1.json` (commands below).

| arm | raw recall | genuine | fetch-free genuine | run tokens | in_tok (cached) | out_tok | cost USD |
|---|---|---|---|---|---|---|---|
| control | 0/5 | 0/5 | 0/5 | 100,396,469 | 98,435,266 (83,477,740) | 1,961,203 | 11.849 |
| exec1 | 2/5 | 2/5 | 1/5 | 94,851,024 | 93,155,692 (79,070,492) | 1,695,332 | 11.004 |
| exec2 | PENDING | PENDING | PENDING | PENDING | PENDING | PENDING | PENDING |
| exec3 | PENDING | PENDING | PENDING | PENDING | PENDING | PENDING | PENDING |

## Pass gate (CP1 + E2)

| leg | threshold | control | exec1 | exec2 | exec3 |
|---|---|---|---|---|---|
| fetch-free genuine recall over exec1-3 | mean >= 2/5 (>= 6 hits / 15 runs) | n/a | 1/5 so far | PENDING | PENDING |
| run tokens per exec arm | <= 1.2x control (<= 120,475,763) | baseline 100,396,469 | PASS (94,851,024) | PENDING | PENDING |
| `rl_units` | 0 in all 4 arms | 0 | 0 | PENDING | PENDING |
| every scorer hit manually read | all hits audited | PASS (0 hits) | PASS (2/2) | PENDING | PENDING |

**Overall: PENDING** (pass 1; the recall leg needs exec2 and exec3).

## Run conditions

- Build `3ecb6f6` in every trial (`agent_info.build` in each trial's
  `result.json`; `drive.log` prints `version=3ecb6f6` per arm).
- One Harbor job per arm: `-n 2` concurrent trials, bugbot `--concurrency 4`,
  1 attempt per task, `conda_env testbed`, model `minimax/MiniMax-M3`;
  control `finder_exec=off`, exec arms `finder_exec=on` (job `config.json`).
- Backend unsandboxed: each trial's `agent/scan.log` opens with the
  UNSANDBOXED banner (workspace copy, no container isolation, network=host).
- Schedule (`/tmp/ypei-r1/harbor/drive.log`): control 01:46-02:39,
  exec1 06:46-07:39, both `attempt 1` with `rl_lines=0`. exec2 waits until
  11:46 and exec3 until 16:46 (quota windows). No arm was voided this round;
  there are no `void-*` job directories.
- `ypei-r1-w3` is a single-task smoke job (astropy only,
  `per_cycle_tokens` 3,000,000, `-n 1`) that ran 01:36-01:46 before control.
  It is not an arm and no number in this record comes from it.
- Finder `run_code` budget: at most 8 calls per finder; the tool refused
  further calls with "run_code budget exhausted" (22 refusals in exec1).

## run_code usage

| arm | run_code calls | executed / refused (budget) | exit codes (executed) | fetching calls |
|---|---|---|---|---|
| control | 0 | 0 / 0 | - | 0 |
| exec1 | 526 | 504 / 22 | 0:442, 1:30, 2:10, 127:13, 128:9 | 34 |
| exec2 | PENDING | PENDING | PENDING | PENDING |
| exec3 | PENDING | PENDING | PENDING | PENDING |

Fetching calls per exec1 trial: astropy 3, django 9, xarray 14, pylint 6,
sklearn 2. What they did: `pip download django==3.0` wheels into `/tmp/dl`
then unpack into `/tmp/dj30*` (django), one `git clone` of django upstream
(refused by the call budget before running), `git clone` of xarray upstream
plus `urllib` fetches of `raw.githubusercontent.com` files (xarray), and the
two failed `pip download sklearn` calls (sklearn).

Sklearn import failures (E3 pattern, if seen): 2 run_code results in exec1's
sklearn trial show
`ModuleNotFoundError: No module named 'sklearn.__check_build._check_build'`,
both from units that ran `python` inside a source tree that shadowed the
installed package. Control shows no run_code usage at all.

## Accepted differences from production

- The backend is unsandboxed: finder probes run on the host with network
  access and the OS user's privileges, not in the sandbox container.
- All units in a trial share the host `/tmp`, and `/tmp` persists across
  trials, so a finder can read what another unit (or an earlier trial)
  downloaded — the django hit's confirmation used such a tree (`/tmp/dj30`).
- The scan target is a copy of the repository's tracked files
  (`git -C /testbed ls-files`), recorded per trial in `agent/tracked_files`.

## Files

- `control.json`, `exec1.json`: scorecards (exec2/exec3 land in pass 2).
- `hits.json`: one record per scorer hit with the genuine read, the
  originating finder units, their fetching calls, the fetch-free verdict, and
  the shared-/tmp read (pass 1 covers control and exec1).

Regenerate a scorecard while the job directories still exist (job directories
live in the feature worktree):

```bash
python3 bench/harbor/scorecard.py \
    /home/dpoage/code/bugbot-wt/ypei-r1-feat/bench/harbor/jobs/ypei-r1-control \
    --price 0.3,0.06,1.2 --json bench/harbor/results/2026-10-25-finder-exec/control.json
python3 bench/harbor/scorecard.py \
    /home/dpoage/code/bugbot-wt/ypei-r1-feat/bench/harbor/jobs/ypei-r1-exec1 \
    --price 0.3,0.06,1.2 --json bench/harbor/results/2026-10-25-finder-exec/exec1.json
```

Re-derive the run_code and fetching counts, and the per-unit candidate lists
behind `hits.json`, with the audit script kept for pass 2:

```bash
J=/home/dpoage/code/bugbot-wt/ypei-r1-feat/bench/harbor/jobs
python3 /tmp/ypei-r1/s6/audit.py \
    control:$J/ypei-r1-control/<trial-dir> ... exec1:$J/ypei-r1-exec1/<trial-dir> ...
```

(one `ARM:<trial-dir>` pair per trial, all ten trials of the two arms; the
script writes `/tmp/ypei-r1/s6/audit-<arm>.json`).
