# Finder-recall A/B loops (2026-10-23 to 2026-10-24)

These runs test why bugbot finds fewer of the gold bugs than claude-code on the
[2026-10-22 baseline](../2026-10-22-minimax-m3/README.md). Every arm uses the
same 5 tasks and the same model (MiniMax-M3). The arms differ only in
`BUGBOT_EXPERIMENTS` switches (`internal/funnel/experiment.go`, branch
`explore/finder-recall`).

**Result: finder execution is the lever.** If finders can run code, bugbot
finds 2 to 3 of 5 gold bugs per run, at the same token cost. That matches
claude-code's 3 of 5. The lens, prompt and chunking changes stay inside the
run-to-run noise of the control arm.

## Arms

| arm | switch | change |
|---|---|---|
| control | none | production behavior |
| logic-lens | `logic-lens` | adds a `logic/wrong-result` lens |
| soft-empty | `soft-empty` | replaces the "an empty list is expected" paragraph of the finder prompt |
| size-chunks | `size-chunks` | cuts chunks at 2,500 lines instead of 8 files |
| finder-exec | `finder-exec` | gives every finder `run_code`: a shell command in a scratch copy of the repository, with the task's conda env active, at most 8 runs for each finder |

## Genuine gold hits

A hit counts only after a manual read of the matched finding. The line-tolerance
matcher credits `pylint/checkers/similar.py:388` (a Unicode decode bug)
as a hit for pylint-4970, but that is a different bug from the gold one (see
bugbot-sds6). This table excludes those false hits.

| run | astropy | django | xarray | pylint | sklearn | bugs found | tokens in (cached) |
|---|---|---|---|---|---|---|---|
| baseline 2026-10-22 | - | - | - | - | hit | 1/5 | 95M (81M) |
| control | - | - | - | - | - | 0/5 | 101M (87M) |
| logic-lens | - | - | - | - | hit | 1/5 | 101M (85M) |
| soft-empty | - | - | - | - | hit | 1/5 | 101M (86M) |
| size-chunks (3 tasks) | | - | - | - | | 0/3 | 201M (172M) |
| finder-exec | hit | hit | - | - | hit | 3/5 | 103M (90M) |
| finder-exec, replicate 2 | hit | - | - | - | hit | 2/5 | 95M (81M) |
| finder-exec, replicate 3 | hit | hit | - | - | hit | 3/5 | 100M (86M) |
| claude-code, baseline | hit | - | hit | - | hit | 3/5 | 119M (113M) |

- **astropy-14995:** finder-exec finds the bug in 3 of 3 runs: `handle_mask`
  gets `None` when only one operand has a mask (`ndarithmetic.py:523-527`).
  The 5 runs without execution find 0. In those runs several finders read
  `_arithmetic_mask`, but none reports the bug.
- **django-11099:** finder-exec finds the bug in 2 of 3 runs: the `$` anchor in
  the username validators accepts a trailing newline. No earlier run of
  either agent found it.
- **sklearn-13142:** without execution, bugbot finds it in 3 of 4 runs. With
  execution, it finds it in 3 of 3.
- **xarray-4075 and pylint-4970:** no bugbot arm finds them.
- **Cost:** finder-exec uses the same tokens as control. Size-chunks uses twice
  the tokens and gains nothing.

## Run conditions

- Each arm ran as one Harbor job: `-n 2`, bugbot concurrency 4, and 1 attempt
  per task.
- The MiniMax Token Plan quota decides the schedule. It has a 5-hour window
  and a weekly window. One 5-task arm uses about 100M tokens, and one window
  holds about 2 arms. So the arms ran one after the other, through
  `/tmp/hb/drive_arms.sh`.
- Before each arm, the driver waits for the quota to return. If any finder
  unit failed on throttling (`rl_units > 0`), the driver voids the arm and runs
  it again.
- Every arm in the tables has `rl_units = 0`. The voided attempts are under
  `bench/harbor/jobs/void-*` and are local only.
- The first two loop attempts ran 4 arms at the same time. The quota ran out
  partway through, and about half of the finder units failed. Both attempts are
  void.
- In those void attempts, bugbot still exited 0 and printed "Scan complete".
  It also told the user to lower `--concurrency`, which is the wrong advice
  for an exhausted quota (bugbot-l569).
- Bugbot builds: control, logic-lens, soft-empty and size-chunks used
  `50f922d`. Finder-exec used `d7c2121`. Experiments that are not switched on
  run production behavior.

## Files

- `loop2.json`: scorecard for the control, logic-lens, soft-empty,
  size-chunks and finder-exec (run 1) arms.
- `loop3-exec-r2.json`, `loop3-exec-r3.json`: the finder-exec replicates.

Regenerate a file while the job directories still exist:

```bash
python3 bench/harbor/scorecard.py bench/harbor/jobs/loop2-control bench/harbor/jobs/loop2-logic \
    bench/harbor/jobs/loop2-soft bench/harbor/jobs/loop2-size bench/harbor/jobs/loop2x-exec \
    --price 0.3,0.06,1.2 --json bench/harbor/results/2026-10-24-finder-recall/loop2.json
```

---

**2026-10-24 addendum (ported to production finder exec).** A later audit of
the exported transcripts found network fetches of upstream code in the exec
arms: 420 of 1,729 `run_code` calls fetched from the network, and the django
hits trace to finder units that diffed the scanned code against downloaded
upstream releases. The ypei-r1 validation record
([`../2026-10-25-finder-exec/`](../2026-10-25-finder-exec/README.md)) reports the raw and fetch-free
numbers.

---

**2026-10-25 note (regeneration).** The Regenerate command above produced
`loop2.json` with the explore-branch scorecard, which labels each arm as its
own agent row (`bugbot[finder-exec]`, …). The production `scorecard.py` on
this branch drops that labeling: rerunning the command now yields 1 aggregate,
not the 5 committed in `loop2.json`. The committed JSONs stay as recorded;
regenerate the arm split with the explore-branch scorecard (2181109) while
the job directories exist.
