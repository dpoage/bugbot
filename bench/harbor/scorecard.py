#!/usr/bin/env python3
"""Per-agent scorecard across Harbor job directories.

Reads every <job>/<trial>/result.json, groups trials by (agent, model), and
pools the verifier's raw counts across tasks (per-task precision averages
are wrong: a task with no findings has undefined precision). Adds tokens,
cost, wall time, crashes, and cost per detected bug. A trial with no reward
(agent or verifier crashed) counts as a miss with no findings.

Usage: bench/harbor/scorecard.py bench/harbor/jobs/<job> [<job> ...] [--per-task]
"""

import argparse
import json
import sys
from collections import defaultdict
from datetime import datetime
from pathlib import Path


def seconds(t: dict | None) -> float:
    if not t or not t.get("started_at") or not t.get("finished_at"):
        return 0.0
    a = datetime.fromisoformat(t["started_at"])
    b = datetime.fromisoformat(t["finished_at"])
    return (b - a).total_seconds()


def trials(job_dirs: list[Path]):
    for job in job_dirs:
        for res in sorted(job.glob("*/result.json")):
            r = json.loads(res.read_text())
            if "agent_info" not in r:
                continue  # job-level result.json
            yield r


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("jobs", nargs="+", type=Path)
    ap.add_argument("--per-task", action="store_true")
    ap.add_argument(
        "--price", metavar="IN,CACHE_READ,OUT",
        help="USD per Mtok; recompute every trial's cost from its tokens so agents are "
             "priced alike (agent-reported cost may assume another vendor's rates). "
             "MiniMax-M3 list price (<=512K ctx): 0.3,0.06,1.2",
    )
    args = ap.parse_args()
    price = [float(x) for x in args.price.split(",")] if args.price else None
    if price is not None and len(price) != 3:
        ap.error("--price needs IN,CACHE_READ,OUT")

    # Latest trial per (agent, model, task) wins, so a rerun job listed
    # alongside the original supersedes its crashed trials instead of
    # double-counting the task.
    latest: dict[tuple[str, str, str], dict] = {}
    for r in trials(args.jobs):
        mi = r["agent_info"].get("model_info") or {}
        key = (r["agent_info"]["name"], mi.get("name") or "?", r["task_name"])
        if key not in latest or (r.get("started_at") or "") > (latest[key].get("started_at") or ""):
            latest[key] = r
    groups: dict[tuple[str, str], list[dict]] = defaultdict(list)
    for (agent, model, _), r in sorted(latest.items()):
        groups[(agent, model)].append(r)
    if not groups:
        print("no trials found", file=sys.stderr)
        return 1

    cols = ["agent", "model", "tasks", "crashed", "bug_recall", "site_recall", "precision",
            "fp/task", "in_tok", "cache_tok", "out_tok", "cost_usd", "usd/bug", "agent_min"]
    rows = []
    for (agent, model), rs in sorted(groups.items()):
        tp = fn = fp = det = crashed = 0
        tin = tcache = tout = 0
        cost = 0.0
        cost_known = True
        mins = 0.0
        for r in rs:
            rw = (r.get("verifier_result") or {}).get("rewards")
            if rw is None or r.get("exception_info"):
                crashed += 1
            rw = rw or {}
            tp += rw.get("site_tp", 0)
            fn += rw.get("site_fn", 0)
            fp += rw.get("fp", 0)
            det += rw.get("detected", 0)
            ar = r.get("agent_result") or {}
            n_in = ar.get("n_input_tokens") or 0  # includes cache reads
            n_cache = ar.get("n_cache_tokens") or 0
            n_out = ar.get("n_output_tokens") or 0
            tin, tcache, tout = tin + n_in, tcache + n_cache, tout + n_out
            if price is not None:
                cost += ((n_in - n_cache) * price[0] + n_cache * price[1] + n_out * price[2]) / 1e6
            elif ar.get("cost_usd") is None:
                cost_known = False
            else:
                cost += ar["cost_usd"]
            mins += seconds(r.get("agent_execution")) / 60
            if args.per_task:
                print(f"  {agent:12} {r['task_name']:36} {json.dumps(rw)} "
                      f"exc={(r.get('exception_info') or {}).get('exception_type')}")
        n = len(rs)
        rows.append([
            agent, model, n, crashed,
            f"{det / n:.2f}",
            f"{tp / (tp + fn):.2f}" if tp + fn else "-",
            f"{tp / (tp + fp):.2f}" if tp + fp else "-",
            f"{fp / n:.1f}",
            tin, tcache, tout,
            f"{cost:.3f}" if cost_known else "-",
            f"{cost / det:.3f}" if cost_known and det else "-",
            f"{mins:.1f}",
        ])

    widths = [max(len(str(c)), *(len(str(row[i])) for row in rows)) for i, c in enumerate(cols)]
    print("  ".join(c.ljust(w) for c, w in zip(cols, widths)))
    for row in rows:
        print("  ".join(str(v).ljust(w) for v, w in zip(row, widths)))
    return 0


if __name__ == "__main__":
    sys.exit(main())
