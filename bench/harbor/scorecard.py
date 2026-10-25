#!/usr/bin/env python3
"""Per-agent scorecard across Harbor job directories.

Reads every <job>/<trial>/result.json, groups trials by (agent, model), and
pools the verifier's raw counts across tasks (per-task precision averages
are wrong: a task with no findings has undefined precision). Adds tokens,
cost, wall time, crashes, and cost per detected bug. A trial with no reward
(agent or verifier crashed) counts as a miss with no findings.

--json writes a self-contained record (aggregates, per-trial rewards and
tokens, and every finding marked with the ground-truth site it matched) that
outlives the job directories; bench/harbor/results/ keeps these baselines.

Usage: bench/harbor/scorecard.py JOB_DIR [JOB_DIR ...] [--per-task] [--price ...] [--json OUT]
"""

import argparse
import json
import sys
from collections import defaultdict
from datetime import datetime
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent / "template"))
import score  # noqa: E402  (the verifier's own loader and matcher)

COLS = ["agent", "model", "tasks", "crashed", "rl_units", "bug_recall", "site_recall", "precision",
        "fp_per_task", "in_tok", "cache_tok", "out_tok", "cost_usd", "usd_per_bug", "agent_min"]


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
            yield res.parent, r


def rate_limited_units(trial_dir: Path) -> int:
    """Occurrences of the "hit provider rate limit after retries" marker in
    agent/scan.log; a lost bugbot finder unit logs the marker twice, so halve
    the count for lost units (0 for other agents)."""
    log = trial_dir / "agent" / "scan.log"
    if not log.is_file():
        return 0
    return log.read_text(errors="replace").count("hit provider rate limit after retries")


def trial_record(trial_dir: Path, r: dict, price: list[float] | None) -> dict:
    ar = r.get("agent_result") or {}
    n_in = ar.get("n_input_tokens") or 0  # includes cache reads
    n_cache = ar.get("n_cache_tokens") or 0
    n_out = ar.get("n_output_tokens") or 0
    if price is not None:
        cost = ((n_in - n_cache) * price[0] + n_cache * price[1] + n_out * price[2]) / 1e6
    else:
        cost = ar.get("cost_usd")
    rewards = (r.get("verifier_result") or {}).get("rewards")
    exc = (r.get("exception_info") or {}).get("exception_type")

    findings, _ = score.load_findings(str(trial_dir / "agent" / "findings.json"))
    task_path = Path((r.get("config") or {}).get("task", {}).get("path") or "")
    gt_file = task_path / "tests" / "ground_truth.json"
    if gt_file.is_file():
        gt = json.loads(gt_file.read_text())
        sites = [dict(s, file=score.norm_path(s["file"])) for s in gt["sites"]]
        matched = score.pair(findings, sites, int(gt.get("tolerance", 5)))
        for i, f in enumerate(findings):
            f["matched_site"] = matched.get(i)
    mi = r["agent_info"].get("model_info") or {}
    return {
        "agent": r["agent_info"]["name"],
        "model": mi.get("name") or "?",
        "task": r["task_name"],
        "trial": trial_dir.name,
        "started_at": r.get("started_at"),
        "crashed": rewards is None or exc is not None,
        "exception": exc,
        "rewards": rewards or {},
        "n_input_tokens": n_in,
        "n_cache_tokens": n_cache,
        "n_output_tokens": n_out,
        "cost_usd": cost,
        "agent_min": round(seconds(r.get("agent_execution")) / 60, 1),
        "metadata": ar.get("metadata") or {},
        "rate_limited_units": rate_limited_units(trial_dir),
        "findings": findings,
    }


def aggregate(agent: str, model: str, rs: list[dict]) -> dict:
    s = lambda k: sum(t["rewards"].get(k, 0) for t in rs)  # noqa: E731
    tp, fn, fp, det, n = s("site_tp"), s("site_fn"), s("fp"), s("detected"), len(rs)
    cost_known = all(t["cost_usd"] is not None for t in rs)
    cost = sum(t["cost_usd"] for t in rs) if cost_known else None
    return {
        "agent": agent,
        "model": model,
        "tasks": n,
        "crashed": sum(t["crashed"] for t in rs),
        "bugs_detected": det,
        "bug_recall": det / n,
        "site_recall": tp / (tp + fn) if tp + fn else None,
        "precision": tp / (tp + fp) if tp + fp else None,
        "fp_per_task": fp / n,
        "in_tok": sum(t["n_input_tokens"] for t in rs),
        "cache_tok": sum(t["n_cache_tokens"] for t in rs),
        "out_tok": sum(t["n_output_tokens"] for t in rs),
        "cost_usd": cost,
        "usd_per_bug": cost / det if cost is not None and det else None,
        "agent_min": round(sum(t["agent_min"] for t in rs), 1),
        # Nonzero means provider throttling dropped units: the arm's numbers
        # understate it and the run should be repeated at lower concurrency.
        "rl_units": sum(t["rate_limited_units"] for t in rs),
    }


def fmt(v) -> str:
    if v is None:
        return "-"
    if isinstance(v, float):
        return f"{v:.3f}" if v < 100 else f"{v:.1f}"
    return str(v)


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
    ap.add_argument("--json", type=Path, metavar="OUT", help="write the full record as JSON")
    args = ap.parse_args()
    price = [float(x) for x in args.price.split(",")] if args.price else None
    if price is not None and len(price) != 3:
        ap.error("--price needs IN,CACHE_READ,OUT")

    # Latest trial per (agent, model, task) wins, so a rerun job listed
    # alongside the original supersedes its crashed trials instead of
    # double-counting the task.
    latest: dict[tuple[str, str, str], dict] = {}
    for trial_dir, r in trials(args.jobs):
        t = trial_record(trial_dir, r, price)
        key = (t["agent"], t["model"], t["task"])
        if key not in latest or (t["started_at"] or "") > (latest[key]["started_at"] or ""):
            latest[key] = t
    if not latest:
        print("no trials found", file=sys.stderr)
        return 1
    groups: dict[tuple[str, str], list[dict]] = defaultdict(list)
    for key in sorted(latest):
        groups[key[:2]].append(latest[key])
    aggs = [aggregate(a, m, rs) for (a, m), rs in sorted(groups.items())]

    if args.per_task:
        for t in (latest[k] for k in sorted(latest)):
            print(f"  {t['agent']:12} {t['task']:36} {json.dumps(t['rewards'])} exc={t['exception']}")
    rows = [[fmt(a[c]) for c in COLS] for a in aggs]
    widths = [max(len(c), *(len(row[i]) for row in rows)) for i, c in enumerate(COLS)]
    print("  ".join(c.ljust(w) for c, w in zip(COLS, widths)))
    for row in rows:
        print("  ".join(v.ljust(w) for v, w in zip(row, widths)))

    if args.json:
        args.json.write_text(json.dumps({
            "price_usd_per_mtok": dict(zip(("input", "cache_read", "output"), price)) if price else None,
            "jobs": [str(j) for j in args.jobs],
            "aggregates": aggs,
            "trials": [latest[k] for k in sorted(latest)],
        }, indent=2) + "\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
