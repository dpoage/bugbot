#!/usr/bin/env python3
"""Attempt table, first loss, loss spans, peak concurrency and window overlap per ypei-r1 job.

Usage: finalpass.py [--jobs ROOT] [JOB_NAME ...]   (default: every ypei-r1 job under ROOT)
The first 12 output lines reproduce run-logs/attempt-table.txt.
"""
import argparse
import json
import sqlite3
from collections import Counter
from datetime import datetime, timezone, timedelta
from pathlib import Path

DEFAULT_JOBS = "/home/dpoage/code/bugbot-harbor-jobs/ypei-r1"
MDT = timezone(timedelta(hours=-6))  # Harbor result.json stamps are naive local MDT
DEFINITION = ("Lost finder unit = role='finder' agent_units row whose detail carries a "
              "provider 429 ('(status 429)'); class from the 429 message.")


def unit_ts(s):
    """state.db stamps: UTC, 'Z', nanosecond fraction; '' when never run."""
    if not s:
        return None
    return datetime.fromisoformat(s.strip().replace(" ", "T").replace("Z", "+00:00"))


def job_ts(s):
    t = datetime.fromisoformat(s)
    return t if t.tzinfo else t.replace(tzinfo=MDT)


def klass(detail: str) -> str:
    if "rate limit reached" in detail:
        return "rate limit reached"
    if "usage limit" in detail:
        return "usage limit"
    return detail[:40]


def measure(job: Path) -> dict:
    res = json.loads((job / "result.json").read_text())
    start, finish = job_ts(res["started_at"]), job_ts(res["finished_at"])
    intervals, lost, classes, n_finder, trials = [], [], Counter(), 0, 0
    bursts = []
    for db in sorted(job.glob("*/agent/state.db")):
        trials += 1
        rows = sqlite3.connect(db).execute(
            "select role, detail, started_at, finished_at from agent_units").fetchall()
        trial_lost = []
        for role, detail, s, f in rows:
            s, f = unit_ts(s), unit_ts(f)
            if role == "finder":
                n_finder += 1
                if "(status 429)" in (detail or ""):
                    lost.append(f)
                    trial_lost.append(f)
                    classes[klass(detail)] += 1
            if s and f:
                intervals.append((s, f))
        stamps = [f for f in trial_lost if f]
        if stamps:
            last = max(stamps)
            after = sum(1 for _, _, s, f in rows if unit_ts(f) and unit_ts(f) > last)
            bursts.append((db.parent.parent.name.split("__")[0], len(trial_lost),
                           max(stamps) - min(stamps), after))
    events = sorted([(s, 1) for s, _ in intervals] + [(f, -1) for _, f in intervals],
                    key=lambda x: (x[0], x[1]))
    cur = peak = 0
    for _, d in events:
        cur += d
        peak = max(peak, cur)
    first = min((f for f in lost if f), default=None)
    pct = 100 * len(lost) / n_finder if n_finder else 0.0
    qualifies = n_finder > 0 and pct <= 5 and set(classes) <= {"rate limit reached"}
    return {"name": job.name, "trials": trials, "finder_units": n_finder,
            "lost": len(lost), "pct": pct, "classes": dict(classes),
            "qualifies": qualifies, "start": start, "finish": finish,
            "first_loss": (first - start) if first else None,
            "peak": peak, "bursts": bursts}


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--jobs", default=DEFAULT_JOBS)
    ap.add_argument("names", nargs="*", help="job directory names (default: all)")
    args = ap.parse_args()
    root = Path(args.jobs)
    names = args.names or sorted(
        (p.name for p in root.iterdir()
         if p.is_dir() and (p / "result.json").is_file()
         and (p.name.startswith("ypei-r1-") or p.name.startswith("void-ypei-r1-"))),
        key=lambda n: n.removeprefix("void-"))
    rows = [measure(root / n) for n in names]

    print(DEFINITION)
    print("| attempt | finder units | lost | lost % | classes of lost "
          "| qualifies (<=5%, all 'rate limit reached') |")
    print("|---|---|---|---|---|---|")
    for r in rows:
        cls = r["classes"] if r["classes"] else "-"
        print(f"| {r['name']} ({r['trials']} trials) | {r['finder_units']} | {r['lost']} "
              f"| {r['pct']:.2f}% | {cls} | {'YES' if r['qualifies'] else 'NO'} |")
    print()
    for r in rows:
        fl = f"+{r['first_loss']}" if r["first_loss"] is not None else "-"
        print(f"{r['name']}: window {r['start'].isoformat()} .. {r['finish'].isoformat()}"
              f"  first loss {fl}  peak concurrent agent units {r['peak']}")
        for task, n, span, after in r["bursts"]:
            print(f"   {task}: {n} lost, loss span {span}, "
                  f"agent units finishing after the last loss {after}")
    overlaps = [(a["name"], b["name"]) for i, a in enumerate(rows) for b in rows[i + 1:]
                if a["start"] < b["finish"] and b["start"] < a["finish"]]
    print(f"\n{len(rows)} job windows; overlapping pairs: {overlaps or 'none'}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
