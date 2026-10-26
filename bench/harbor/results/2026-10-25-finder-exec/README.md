# Finder-exec production validation (2026-10-25, round ypei-r1)

This record validates the production `finder-exec` path (finder `run_code`)
through Harbor on the same 5 SWE-bench tasks as the
[2026-10-24 finder-recall loops](../2026-10-24-finder-recall/README.md). It
adds the fetch-free accounting that the 2026-10-24 addendum demanded, under
the arbiter V2 rulings of 2026-10-25 (they supersede the earlier E2 rule):

- **Q1:** a fetching call disqualifies only when content was retrieved: the
  fetching command exited 0 with progress/success signals, or its
  destination shows content afterwards. A fetch that fails with nothing
  written is not contamination.
- **Q2 (gating):** a hit is fetch-free only if its originating unit(s)
  neither retrieved content nor read, diffed, imported from or listed any
  path that a retrieving fetch by ANY unit in the same trial wrote (shared
  host `/tmp` counts; "read" covers any `run_code` command or read tool).

**Status: pass 2a of 2.** Control, exec1 and exec2 are complete and audited
below; every exec3 cell reads PENDING until pass 2b fills them (exec3-a2
starts 14:37:41 on 2026-10-26, with a3/a4 if it does not qualify).

## Arms

| arm | `finder_exec` | change |
|---|---|---|
| control | off | production scan without finder `run_code` |
| exec1 | on | production scan with finder `run_code` |
| exec2 | on | replicate 2, qualifying attempt `void-ypei-r1-exec2-a3` |
| exec3 | on | replicate 3 (PENDING) |

## Hits per task

Mark legend: **raw** = the verifier's line-tolerance matcher credits the
finding; **genuine** = a manual read against the gold patch and issue confirms
the same defect; **ff** = fetch-free under the V2 rulings (Q1 + Q2 above).
Full evidence per hit: `hits.json`.

| task | control | exec1 | exec2 | exec3 |
|---|---|---|---|---|
| astropy-14995 | - | - | raw, genuine (not ff) | PENDING |
| django-11099 | - | raw, genuine (not ff) | raw, genuine (not ff) | PENDING |
| xarray-4075 | - | - | - | PENDING |
| pylint-4970 | - | - | - | PENDING |
| sklearn-13142 | - | raw, genuine, ff | raw, genuine (not ff) | PENDING |

- **django-11099 (exec1):** the hit names the gold defect (`$` anchor
  accepts a trailing newline in both username validators). Its single
  originating finder unit made no fetching calls of its own, but it listed,
  read and diffed `/tmp/dj30`, a tree another unit in the same trial
  pip-downloaded and unpacked — a Q2 write-set touch, so the hit is not
  fetch-free even though the mechanism is provable from `/testbed` alone.
- **sklearn-13142 (exec1):** the hit names the gold defect (`fit_predict`
  labels come from the last initialization, not the restored best one) and
  both originating finder units proved it with local numpy probes in
  `/testbed`. Each unit also ran one `pip download sklearn==...` call; both
  failed with `No matching distribution found` and wrote nothing, and the
  trial's fetch write-set is empty, so under Q1+Q2 the hit is fetch-free.
- Control found nothing (0 of 5), matching the 2026-10-24 control arm.
- **exec2 (a3) hits, all genuine and all named for their gold defects:**
  astropy `ndarithmetic.py:527` (mask handler called with `None` when the
  second operand has no mask), django `validators.py:10` (`$` anchor
  trailing newline), sklearn `base.py:263` (labels from the stale E-step).
  None is fetch-free: astropy's five originating units all read or diffed
  fetch-written trees (`/tmp/astropydl/*`, `/tmp/pip-download-*`) and two
  retrieved wheels themselves; django's two originating units each retrieved
  a Django-3.0 wheel; sklearn's four originating units include the unit that
  pip-downloaded scikit-learn 0.21.0/0.20.4 into `/tmp/sk021` and the unit
  that diffed those wheels against the repo.
- Under the superseded E2 rule the exec1 fetch-free mark was on django-11099
  instead; V2 swaps which hit counts and leaves the exec1 tally at 1/5.

## Recall and tokens

Run tokens = `in_tok + out_tok`; cached tokens are inside `in_tok` and also
reported alone. Lost finder units = `role='finder'` `agent_units` rows whose
detail carries a provider 429, counted over the arm's qualifying attempt
(user ruling 2026-10-26). Source: `control.json`, `exec1.json`, `exec2.json`,
`hits.json` (commands below).

| arm | raw recall | genuine | fetch-free genuine | run tokens | in_tok (cached) | out_tok | cost USD | lost finder units |
|---|---|---|---|---|---|---|---|---|
| control | 0/5 | 0/5 | 0/5 | 100,396,469 | 98,435,266 (83,477,740) | 1,961,203 | 11.849 | 0/152 (0.00%) |
| exec1 | 2/5 | 2/5 | 1/5 | 94,851,024 | 93,155,692 (79,070,492) | 1,695,332 | 11.004 | 0/152 (0.00%) |
| exec2 (a3) | 3/5 | 3/5 | 0/5 | 103,783,009 | 101,551,168 (86,631,104) | 2,231,841 | 12.352 | 5/152 (3.29%) |
| exec3 | PENDING | PENDING | PENDING | PENDING | PENDING | PENDING | PENDING | PENDING |

## Pass gate (CP1 + V2 + user ruling 2026-10-26)

| leg | threshold | control | exec1 | exec2 | exec3 |
|---|---|---|---|---|---|
| fetch-free genuine recall over exec1-3 | mean >= 2/5 (>= 6 hits / 15 runs) | n/a | 1/5 | 0/5 | PENDING |
| run tokens per exec arm | <= 1.2x control (<= 120,475,763) | baseline 100,396,469 | PASS (94,851,024) | PASS (103,783,009) | PENDING |
| lost finder units (user ruling 2026-10-26) | <= 5% of the arm's finder units and every lost unit is the transient "Token Plan rate limit reached" class; first qualifying attempt counts, voided attempts never score | 0/152 YES | 0/152 YES | 5/152 (3.29%), all "rate limit reached" YES | PENDING |
| every scorer hit manually read | all hits audited | PASS (0 hits) | PASS (2/2) | PASS (3/3) | PENDING |

**Recall leg: FAIL, already determined.** exec1 and exec2 together hold 1
fetch-free genuine hit in 10 runs; exec3 can add at most 3, so the ceiling is
4 of 15, below the required 6. Per the round rules this is a reported
finding, not a rerun trigger. Raw recall passes everywhere it is measurable
(5 genuine hits in 10 exec runs so far), so the shortfall is entirely the
fetch-free gate: finder units routinely read upstream releases another unit
downloaded into the shared `/tmp`.

**Overall: PENDING** (the tokens, lost-units and hits-read legs pass on all
audited arms; the recall leg is FAIL by arithmetic ceiling; exec3 pending).

## Run conditions

- Build `3ecb6f6` in every trial (control and exec1 carry it as
  `agent_info.build`, exec2 as `agent_info.version`; `drive.log` prints
  `version=3ecb6f6` per start).
- One Harbor job per arm: `-n 2` concurrent trials, bugbot `--concurrency 4`,
  1 attempt per task, `conda_env testbed`, model `minimax/MiniMax-M3`;
  control `finder_exec=off`, exec arms `finder_exec=on` (job `config.json`).
- Backend unsandboxed: each trial's `agent/scan.log` opens with the
  UNSANDBOXED banner (workspace copy, no container isolation, network=host).
- Schedule (`/tmp/ypei-r1/harbor/drive.log`): control 2026-10-25 01:46-02:39,
  exec1 06:46-07:39. exec2 ran three attempts (11:46 and 16:46 on 2026-10-25,
  then 09:37-10:39 on 2026-10-26); exec3 ran attempt 1 at 21:46 on 2026-10-25
  and its attempt 2 starts 14:37:41 on 2026-10-26.
- `ypei-r1-w3` is a single-task smoke job (astropy only,
  `per_cycle_tokens` 3,000,000, `-n 1`) that ran 01:36-01:46 before control.
  It is not an arm and no number in this record comes from it.
- Finder `run_code` budget: at most 8 calls per finder; the tool refused
  further calls with "run_code budget exhausted" (22 refusals in exec1, 24 in
  exec2).

### Attempts and validity (user ruling 2026-10-26)

Validity rule (replaces `rl_units = 0` for this round): an arm is valid if
its lost finder units (`agent_units` rows with `role='finder'` whose detail
carries a provider 429) are at most 5% of the arm's finder units across all
5 trials, and every lost unit is the transient "Token Plan rate limit
reached" class. The first qualifying attempt per arm counts; later attempts
are not considered; voided attempts are never scored. Measured table
(verbatim from `/tmp/ypei-r1/harbor/attempt-table.txt`, its definition
included):

```
Lost finder unit = role='finder' agent_units row whose detail carries a provider 429 ('(status 429)'); class from the 429 message.
| attempt | finder units | lost | lost % | classes of lost | qualifies (<=5%, all 'rate limit reached') |
|---|---|---|---|---|---|
| ypei-r1-control (5 trials) | 152 | 0 | 0.00% | - | YES |
| ypei-r1-exec1 (5 trials) | 152 | 0 | 0.00% | - | YES |
| void-ypei-r1-exec2-a1 (5 trials) | 152 | 11 | 7.24% | {'rate limit reached': 11} | NO |
| void-ypei-r1-exec2-a2 (5 trials) | 152 | 18 | 11.84% | {'rate limit reached': 18} | NO |
| void-ypei-r1-exec2-a3 (5 trials) | 152 | 5 | 3.29% | {'rate limit reached': 5} | YES |
| void-ypei-r1-exec3-a1 (5 trials) | 152 | 9 | 5.92% | {'rate limit reached': 9} | NO |
```

- **Why the scored exec2 directory is named `void-*`:** the driver marked
  every attempt with rate-limit lines as void and moved its directory to
  `void-ypei-r1-exec2-a*`. Under the 2026-10-26 ruling the third attempt
  (`void-ypei-r1-exec2-a3`, started 2026-10-26 09:37) is the first one under
  the 5% threshold (5/152 = 3.29%, all transient "rate limit reached"), so it
  is the attempt this record scores; the "void" prefix is a leftover of the
  old rule, not a disqualification.
- 429 classification and burst timing (all 38 lost units across the voided
  attempts are the transient "Token Plan rate limit reached" class, in short
  bursts with later units succeeding):
  `/tmp/ypei-r1/harbor/void-classification.md`.

## run_code usage

| arm | run_code calls | executed / refused (budget) | exit codes (executed) | fetching calls |
|---|---|---|---|---|
| control | 0 | 0 / 0 | - | 0 |
| exec1 | 526 | 504 / 22 | 0:442, 1:30, 2:10, 127:13, 128:9 | 34 |
| exec2 (a3) | 552 | 528 / 24 | 0:462, 1:33, 2:9, 127:17, 128:6, -1:1 | 54 |
| exec3 | PENDING | PENDING | PENDING | PENDING |

Fetching calls per exec1 trial: astropy 3, django 9, xarray 14, pylint 6,
sklearn 2. Of the 34 calls, 23 retrieved content (Q1), 5 failed with an
error and nothing written, 4 were refused by the call budget before running,
and 2 show no retrieval evidence in the transcript (quiet `pip -q` with a
shell-variable destination). What the retrieving calls did: `pip download
django==3.0` wheels into `/tmp/dl` then unpack into `/tmp/dj30*` (django),
`git clone` of xarray upstream plus `curl`/`urllib` fetches of
`raw.githubusercontent.com` files (xarray), `pip download` of astropy,
xarray and pylint wheels/sdists into `/tmp/ast`, `/tmp/x*`, `/tmp/pl*`,
`/tmp/dl`. The two sklearn `pip download sklearn` calls both failed. One
`git clone` of django upstream was refused by the call budget before it ran.
Exit status and destination of every fetching call: `hits.json`
(fetching_calls_by_arm). Fetching calls per exec2 trial: astropy 8, django
15, xarray 19, pylint 9, sklearn 3. Of the 54 calls, 36 retrieved content
(Q1), 11 failed with an error and nothing written, 3 were refused by the
call budget before running, and 4 show no retrieval evidence in the
transcript. The retrieving calls follow the exec1 pattern: `pip download` of
astropy, django, xarray, pylint and (this time successfully) scikit-learn
wheels into `/tmp/astropydl`, `/tmp/dj30`, `/tmp/dl*`, `/tmp/xa*`, `/tmp/pl*`
and `/tmp/sk021`, then diffs of those trees against `/testbed`.

Sklearn import failures (E3 pattern, if seen): 2 run_code results in exec1's
sklearn trial and 4 in exec2's show
`ModuleNotFoundError: No module named 'sklearn.__check_build._check_build'`,
all from units that ran `python` inside a source tree that shadowed the
installed package. Control shows no run_code usage at all.

## Accepted differences from production

- The backend is unsandboxed: finder probes run on the host with network
  access and the OS user's privileges, not in the sandbox container.
- All units in a trial share the host `/tmp`, and `/tmp` persists across
  trials, so a finder can read what another unit (or an earlier trial)
  downloaded — the django hit's confirmation used such a tree (`/tmp/dj30`).
  The unsandboxed backend thus opens a shared-/tmp cross-unit channel that
  the Q2 gate has to police; on the podman and bwrap backends each exec gets
  its own `/tmp`, so the channel does not exist there. This is an
  observation, not a gate.
- The scan target is a copy of the repository's tracked files
  (`git -C /testbed ls-files`), recorded per trial in `agent/tracked_files`.

## Files

- `control.json`, `exec1.json`, `exec2.json`: scorecards (`exec2.json` scores
  the qualifying attempt `void-ypei-r1-exec2-a3`; exec3 lands in pass 2b).
- `hits.json`: one record per scorer hit with the genuine read, the
  originating finder units, their fetching calls, the V2 fetch-free verdict
  with its write-set touches, `lost_finder_units` per arm, and
  `fetching_calls_by_arm` — the exit status, destination and retrieved
  verdict of every fetching call in the audited arms (control, exec1, exec2).

Regenerate a scorecard while the job directories still exist (job directories
live in the feature worktree):

```bash
python3 bench/harbor/scorecard.py \
    /home/dpoage/code/bugbot-wt/ypei-r1-feat/bench/harbor/jobs/ypei-r1-control \
    --price 0.3,0.06,1.2 --json bench/harbor/results/2026-10-25-finder-exec/control.json
python3 bench/harbor/scorecard.py \
    /home/dpoage/code/bugbot-wt/ypei-r1-feat/bench/harbor/jobs/ypei-r1-exec1 \
    --price 0.3,0.06,1.2 --json bench/harbor/results/2026-10-25-finder-exec/exec1.json
python3 bench/harbor/scorecard.py \
    /home/dpoage/code/bugbot-wt/ypei-r1-feat/bench/harbor/jobs/void-ypei-r1-exec2-a3 \
    --price 0.3,0.06,1.2 --json bench/harbor/results/2026-10-25-finder-exec/exec2.json
```

Re-derive the run_code and fetching counts, the Q1 retrieval verdicts, the
Q2 write-sets and touches, and the per-unit candidate lists behind
`hits.json`, with the audit script kept for pass 2 (it applies the V2 rules
mechanically, so pass 2 needs no reclassification by hand):

```bash
J=/home/dpoage/code/bugbot-wt/ypei-r1-feat/bench/harbor/jobs
python3 /tmp/ypei-r1/s6/audit.py \
    control:$J/ypei-r1-control/<trial-dir> ... exec1:$J/ypei-r1-exec1/<trial-dir> ...
```

(one `ARM:<trial-dir>` pair per trial, all fifteen trials of the three arms;
the script writes `/tmp/ypei-r1/s6/audit-<arm>.json`). The lost-finder-units
counts come from the same job directories:
`sqlite3 <trial>/agent/state.db "select detail from agent_units where
role='finder'"` and counting the rows whose detail carries a 429.
