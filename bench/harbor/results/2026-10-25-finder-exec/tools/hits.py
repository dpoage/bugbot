#!/usr/bin/env python3
"""Per scorer hit: findings at the gold site, their originating units, and the Q2 counterfactual.

Usage: hits.py [--jobs ROOT] [--audit DIR] [--json OUT] ARM...   (DIR = audit.py --out)
The counterfactual drops contaminated units, keeps findings with a clean unit,
and re-runs the verifier's matcher (bench/harbor/template/score.py) on them.
"""
import argparse
import json
import sqlite3
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[3] / "template"))
import score  # noqa: E402  (the verifier's own loader and matcher)

DEFAULT_JOBS = "/home/dpoage/code/bugbot-harbor-jobs/ypei-r1"
ARM_DIRS = {"control": "ypei-r1-control", "exec1": "ypei-r1-exec1",
            "exec2": "void-ypei-r1-exec2-a3"}


def lenses(row: dict) -> list:
    return [row.get("Lens")] + list(row.get("CorroboratingLenses") or [])


def originating(finding: dict, finders: list, tol: int) -> list:
    out = []
    for u in finders:
        if u["lens"] not in lenses(finding):
            continue
        for c in u["candidates"]:
            try:
                line = int(c.get("line"))
            except (TypeError, ValueError):
                continue
            if (score.norm_path(c.get("file", "")) == score.norm_path(finding["File"])
                    and abs(line - int(finding["Line"])) <= tol):
                out.append(u)
                break
    return out


def unit_view(u: dict) -> dict:
    via = sorted({t["via"] for t in u["write_set_touches"]})
    return {
        "unit_id": u["unit_id"], "transcript": u["transcript"], "lens": u["lens"],
        "run_code_calls": u["n_run_code"], "fetching_calls": u["n_fetching"],
        "retrieving_calls": u["n_retrieving"],
        "touches_fetch_write_set": u["touches_write_set"], "touch_via": via,
        "clean": u["n_retrieving"] == 0 and not u["touches_write_set"],
    }


def trial_hits(trial: Path, audit_trial: dict) -> list:
    task_dir = Path(json.loads((trial / "config.json").read_text())["task"]["path"])
    gt = json.loads((task_dir / "tests" / "ground_truth.json").read_text())
    sites = [dict(s, file=score.norm_path(s["file"])) for s in gt["sites"]]
    tol = int(gt.get("tolerance", 5))
    rows = json.loads((trial / "agent" / "findings.json").read_text()) or []
    scored, _ = score.load_findings(trial / "agent" / "findings.json")
    credited = score.pair(scored, sites, tol)
    if not credited:
        return []
    finders = [u for u in audit_trial["units"] if u["role"] == "finder"]
    at_site = [i for i, f in enumerate(scored)
               if any(f["file"] == s["file"] and score.distance(s, f["line"], tol) is not None
                      for s in sites)]
    findings = []
    survivors = []
    for i in at_site:
        units = [unit_view(u) for u in originating(rows[i], finders, tol)]
        alive = any(u["clean"] for u in units)
        if alive:
            survivors.append(scored[i])
        findings.append({
            "index": i, "file_line": f"{rows[i]['File']}:{rows[i]['Line']}",
            "title": rows[i]["Title"], "lenses": lenses(rows[i]),
            "scorer_credited": i in credited, "originating_units": units,
            "survives_counterfactual": alive,
        })
    leads = sqlite3.connect(trial / "agent" / "state.db").execute(
        "select count(*) from leads").fetchone()[0]
    cf = score.match(survivors, sites, tol)["detected"] == 1
    credited_units = [u for f in findings if f["scorer_credited"]
                      for u in f["originating_units"]]
    return [{
        "task": trial.name.split("__")[0] + "__" + trial.name.split("__")[1],
        "trial": trial.name, "gold_sites": gt["sites"], "tolerance": tol,
        "findings_at_gold_site": findings, "leads_rows": leads,
        "fetch_free_clarified": cf,
        "all_units_clean": bool(credited_units) and all(u["clean"] for u in credited_units),
    }]


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--jobs", default=DEFAULT_JOBS)
    ap.add_argument("--audit", default="audit-out", help="audit.py --out dir")
    ap.add_argument("--json", help="write the derived records here")
    ap.add_argument("arms", nargs="+")
    args = ap.parse_args()
    out = {}
    for arm in args.arms:
        audit = {t["trial"]: t for t in
                 json.loads((Path(args.audit) / f"audit-{arm}.json").read_text())}
        job = Path(args.jobs) / ARM_DIRS[arm]
        hits = []
        for trial in sorted(p for p in job.iterdir() if (p / "agent").is_dir()):
            hits += trial_hits(trial, audit[trial.name])
        out[arm] = hits
        ff = sum(h["fetch_free_clarified"] for h in hits)
        allc = sum(h["all_units_clean"] for h in hits)
        print(f"{arm}: scorer hits {len(hits)}, fetch-free (clarified) {ff}, "
              f"ALL-units clean {allc}")
        for h in hits:
            print(f"  {h['task']}: ff={h['fetch_free_clarified']} "
                  f"all_units_clean={h['all_units_clean']} leads_rows={h['leads_rows']}")
            for f in h["findings_at_gold_site"]:
                tag = "CREDITED" if f["scorer_credited"] else "uncredited"
                print(f"    findings.json[{f['index']}] {f['file_line']} {tag} "
                      f"lenses={f['lenses']} survives={f['survives_counterfactual']}")
                for u in f["originating_units"]:
                    print(f"      unit {u['unit_id']} {u['lens']}: rc={u['run_code_calls']} "
                          f"fetch={u['fetching_calls']} retrieved={u['retrieving_calls']} "
                          f"touch={u['touch_via'] or '-'} clean={u['clean']}")
    if args.json:
        Path(args.json).write_text(json.dumps(out, indent=1) + "\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
