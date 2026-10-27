#!/usr/bin/env python3
"""Audit finder transcripts: run_code usage, Q1 fetch retrievals, Q2 write-set touches.

Usage: audit.py [--jobs ROOT] [--out DIR] [--fresh] ARM... (control|exec1|exec2|ARM:JOB_DIR)
Writes DIR/audit-<arm>.json and prints the counts README.md quotes; Q1/Q2 as
defined in hits.json (definitions.retrieving_fetch, definitions.write_set_touch).
"""
import argparse
import itertools
import json
import re
import sqlite3
import shutil
import tarfile
from collections import Counter
from datetime import datetime, timedelta
from pathlib import Path

DEFAULT_JOBS = "/home/dpoage/code/bugbot-harbor-jobs/ypei-r1"
ARM_DIRS = {"control": "ypei-r1-control", "exec1": "ypei-r1-exec1",
            "exec2": "void-ypei-r1-exec2-a3"}

FETCH_RE = re.compile(
    r"(\bpip3?\s+(install|download|index)\b|\buv\s+\S*\s*(install|download|sync)\b"
    r"|\bcurl\b|\bwget\b|\bgit\s+(clone|fetch|pull)\b|https?://\S+"
    r"|\bconda\s+(install|create)\b|\bapt(-get)?\s+install\b|\burllib\.request\b)",
    re.IGNORECASE,
)

STRONG_OK = re.compile(
    r"(\bSaved \S+|Successfully downloaded \S+|File was already downloaded \S+)",
    re.IGNORECASE)
WEAK_OK = re.compile(
    r"(\bDownloading \(|Collecting \S|Using cached|Cloning into|Receiving objects|"
    r"Resolving deltas|^got \d|^ok \d+|^-[rw-]{9}\s+\d+\s+\S+\s+\S+\s+\d+)",
    re.IGNORECASE | re.MULTILINE)
BAD_SIG = re.compile(
    r"(No matching distribution found|Could not find a version that satisfies|"
    r"ResolutionImpossible|Could not resolve host|Connection (timed out|refused)|"
    r"fatal:|curl: \(\d+\)|ERROR 404|404: Not Found|UnicodeEncodeError|"
    r"run_code budget exhausted|URLError|HTTP Error|timed_out=true)", re.IGNORECASE)
REFUSED = "ERROR: run_code budget exhausted"
SKLEARN_BUILD_ERR = "No module named 'sklearn.__check_build._check_build'"
BARE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._+-]*$")
DIFF_OP = re.compile(r"^\d+(,\d+)?[acd]\d+(,\d+)?$")


def is_pip_local(cmd: str) -> bool:
    """pip install --no-index ... never touches a network index."""
    return bool(re.search(r"\bpip3?\s+install\b[^|;&]*--no-index", cmd))


def parse_exit(result) -> object:
    if not isinstance(result, str):
        return None
    if result.lstrip().startswith(REFUSED):
        return "budget_exhausted"
    m = re.match(r"\s*exit_code=(-?\d+) ", result)
    return int(m.group(1)) if m else None


def parse_duration_ms(result) -> int:
    if not isinstance(result, str):
        return 0
    m = re.search(r"duration=(\d+)ms", result)
    return int(m.group(1)) if m else 0


def ts(time: str) -> datetime:
    """Transcript event stamp (UTC, 'Z', nanosecond fraction) as datetime."""
    return datetime.fromisoformat(time.replace("Z", "+00:00"))


def call_start(time: str, dur_ms: int) -> datetime:
    """Event stamps are call ENDs; replay the duration to get the start."""
    return ts(time) - timedelta(milliseconds=dur_ms)


def _cwd_at(cmd: str, pos: int) -> str:
    """Working directory at `pos`: replay the preceding `cd X` steps."""
    cwd = "(cwd)"
    for m in re.finditer(r"(?:^|[;&|(]\s*)cd\s+(\S+)", cmd[:pos]):
        d = m.group(1).rstrip(";,)")
        if d.startswith("/"):
            cwd = d
        elif cwd != "(cwd)":
            cwd = f"{cwd}/{d}"
    return cwd


def _resolve(path: str, cwd: str) -> str:
    path = path.strip("\"'")
    if path.startswith("/"):
        return path
    if cwd.startswith("/"):
        return f"{cwd}/{path}"
    return f"(cwd)/{path}"


def loop_vars(cmd: str) -> dict[str, list[str]]:
    """Shell variables bound by `for VAR in w1 w2 ...; do` in the command."""
    out = {}
    for m in re.finditer(r"for\s+(\w+)\s+in\s+([^;]*);", cmd):
        words = [w.strip("\"'") for w in m.group(2).split()]
        if words:
            out[m.group(1)] = words
    return out


def expand_vars(text: str, varmap: dict[str, list[str]]) -> list[str]:
    """Expand $VAR / ${VAR} over every loop binding (cartesian product)."""
    if "$" not in text:
        return [text]
    names = [v for v in varmap if re.search(r"\$\{?" + v + r"\}?", text)]
    if not names:
        return [text]
    out = []
    for combo in itertools.product(*[varmap[v] for v in names]):
        t = text
        for v, w in zip(names, combo):
            t = re.sub(r"\$\{?" + v + r"\}?", w, t)
        if t not in out:
            out.append(t)
    return out


def destinations(cmd: str) -> list:
    """/tmp paths a fetching command writes; `$VAR` destinations expand over
    the command's own `for` loops."""
    out = []
    pipish = bool(re.search(r"\bpip3?\s+(download|install)\b|\buv\b", cmd))
    cw = bool(re.search(r"\b(curl|wget)\b", cmd))
    varmap = loop_vars(cmd)
    if pipish:
        for m in re.finditer(r"(?:\s|=)(-d|--dest|--target|--directory)\s+(\S+)", cmd):
            d = m.group(2).rstrip(";,)")
            out += [_resolve(x, _cwd_at(cmd, m.start())) for x in expand_vars(d, varmap)]
    if cw:
        for m in re.finditer(r"(?:\s)(-o|--output)\s+(\S+)", cmd):
            d = m.group(2).rstrip(";,)")
            out += [_resolve(x, _cwd_at(cmd, m.start())) for x in expand_vars(d, varmap)]
        if re.search(r"\s-O\b", cmd):
            out.append("(cwd)/(url-basename)")
    m = re.search(r"\bgit\s+clone\b[^\n]*?\s(\S+)\s*$", cmd.strip())
    if m and not m.group(1).startswith("-"):
        out.append(_resolve(m.group(1), _cwd_at(cmd, m.end())))
    for pat in (r"open\('([^']+)'[^)]*'[wa]b?'?\)", r"os\.makedirs\('([^']+)'\)",
                r"extractall\('([^']+)'\)"):
        for m in re.finditer(pat, cmd):
            out += expand_vars(m.group(1), varmap)
    if pipish and not re.search(r"-d\b|--dest\b|--target\b", cmd):
        out.append(_cwd_at(cmd, len(cmd)))
    keep = lambda p: bool(re.fullmatch(r"(\(cwd\)|/tmp/[A-Za-z0-9_./$-]+)", p))  # noqa: E731
    cleaned = []
    for p in out:
        p = re.sub(r"/\.?$", "", p).rstrip("/;")
        if keep(p):
            cleaned.append(p)
    return sorted(set(cleaned))


def observed_paths(result: str, cmd: str) -> list:
    """/tmp paths this command's output shows to exist (absolute paths, `Saved`
    targets, and single-entry lines of an `ls` of a /tmp directory)."""
    res = result or ""
    out = set(p for p in re.findall(r"/tmp/[A-Za-z0-9_./-]+", res)
              if "/llmkit-sandbox" not in p)
    cwd = _cwd_at(cmd, len(cmd))
    for m in re.finditer(r"\bSaved (\./[^ \n]+)", res):
        if cwd.startswith("/"):
            out.add(f"{cwd}/{m.group(1)[2:]}")
    bases = []
    for m in re.finditer(r"(?:^|[;&|(]\s*|&&\s*)\bls\b\s*([^;|&]*)", cmd):
        args = [a for a in m.group(1).split() if not a.startswith("2>")]
        if args and args[0].startswith("/tmp"):
            bases.append(args[0].rstrip("/"))
        elif not args and cwd.startswith("/tmp"):
            bases.append(cwd.rstrip("/"))
    if bases:
        # one bare token with a digit or dot; prose like "Successfully downloaded astropy" must not map
        for ln in res.splitlines():
            toks = ln.split()
            if (len(toks) == 1 and BARE.match(toks[0])
                    and re.search(r"[\d.]", toks[0])
                    and not DIFF_OP.match(toks[0])):
                for b in set(bases):
                    out.add(f"{b}/{toks[0]}")
    return sorted(p.rstrip("/;,)'\"") for p in out)


def candidates_from_final_text(text: str) -> list:
    """Finder final_text may be plain JSON or <think>...</think> followed by
    JSON (possibly with trailing prose). Try every 'candidates' object."""
    out = []
    for m in re.finditer(r'\{\s*"candidates"', text):
        depth = 0
        chunk = text[m.start():]
        for i, ch in enumerate(chunk, start=1):
            if ch == "{":
                depth += 1
            elif ch == "}":
                depth -= 1
                if depth == 0:
                    try:
                        j = json.loads(chunk[:i])
                    except Exception:
                        continue
                    if isinstance(j.get("candidates"), list):
                        out.extend(j["candidates"])
                    break
    seen, uniq = set(), []
    for c in out:
        key = (c.get("file"), c.get("line"), c.get("title"))
        if key not in seen:
            seen.add(key)
            uniq.append(c)
    return uniq


def unit_id_of(filename: str) -> str:
    for part in filename.rsplit(".", 1)[0].split("-"):
        if len(part) == 32 and part.startswith("000001"):
            return part
    return ""


def audit_trial(trial: Path, extract_to: Path, fresh: bool = False) -> dict:
    tgz = trial / "agent" / "transcripts.tgz"
    tdir = extract_to / trial.name
    if fresh and tdir.exists():
        shutil.rmtree(tdir)
    if not (tdir / "transcripts").is_dir():
        tdir.mkdir(parents=True, exist_ok=True)
        with tarfile.open(tgz) as tf:
            tf.extractall(tdir, filter="data")
    db = sqlite3.connect(trial / "agent" / "state.db")
    db.row_factory = sqlite3.Row
    units_db = {r["id"]: dict(r) for r in db.execute("select * from agent_units")}

    units, events = {}, []
    for jf in sorted((tdir / "transcripts").glob("*.jsonl")):
        uid = unit_id_of(jf.name)
        row = units_db.get(uid, {})
        rec = {
            "transcript": jf.name, "unit_id": uid,
            "role": row.get("role", "(not in agent_units)"),
            "lens": row.get("lens", ""), "strategy": row.get("strategy", ""),
            "status": row.get("status", ""), "candidates": [],
            "run_code": [], "n_run_code": 0, "fetching_calls": [],
            "n_fetching": 0, "n_retrieving": 0, "write_set_touches": [],
            "touches_write_set": False,
        }
        units[jf.name] = rec
        for line in jf.open(errors="replace"):
            try:
                e = json.loads(line)
            except Exception:
                continue
            if e.get("kind") != "tool_run":
                if e.get("kind") == "finalize":
                    rec["candidates"] = candidates_from_final_text(
                        e.get("finalize", {}).get("final_text", ""))
                continue
            call = e["tool_run"]["call"]
            result = e["tool_run"].get("result")
            events.append({
                "time": e.get("time", ""), "step": e.get("step"),
                "unit": jf.name, "name": call["name"],
                "args": call.get("arguments", {}),
                "result": result if isinstance(result, str) else "",
            })
    # event stamps are call ENDs, so time order is execution order
    events.sort(key=lambda x: x["time"])

    seen_obs: dict[str, str] = {}
    for ev in events:
        if ev["name"] != "run_code":
            continue
        for p in observed_paths(ev["result"], ev["args"].get("command", "")):
            if p not in seen_obs or ev["time"] < seen_obs[p]:
                seen_obs[p] = ev["time"]

    def under(d, items):
        pref = d.rstrip("/") + "/"
        return [p for p in items if p == d or p.startswith(pref)]

    # Q1
    fetch_events = []
    for ev in events:
        if ev["name"] != "run_code":
            continue
        cmd = ev["args"].get("command", "")
        ev["fetching"] = bool(FETCH_RE.search(cmd)) and not is_pip_local(cmd)
        if not ev["fetching"]:
            continue
        fetch_events.append(ev)
        exit_ = parse_exit(ev["result"])
        flat = lambda xs: [x[0] if isinstance(x, tuple) else x for x in xs]  # noqa: E731
        bad = flat(BAD_SIG.findall(ev["result"])[:4])
        ok = flat((STRONG_OK.findall(ev["result"]) + WEAK_OK.findall(ev["result"]))[:4])
        dests = destinations(cmd)
        ev["fetch"] = {
            "exit": exit_, "ok_sigs": ok, "bad_sigs": bad,
            "destinations": dests, "command": cmd,
            "step": ev["step"], "time": ev["time"],
            "retrieved": False, "basis": "",
        }
        f = ev["fetch"]
        if exit_ == "budget_exhausted":
            f["basis"] = "refused by run_code budget (never executed)"
            continue
        start = call_start(ev["time"], parse_duration_ms(ev["result"]))
        end = ts(ev["time"])
        concrete = [d for d in dests if d.startswith("/tmp/")]
        ever = sorted({p for d in concrete for p in under(d, seen_obs) if p != d})
        own_under = sorted({p for d in concrete
                            for p in under(d, observed_paths(ev["result"], cmd))
                            if p != d})
        pip_tree = None
        if re.search(r"\bpip3?\s+download\b", cmd):
            # pip's temp tree survives a killed sdist build; attribute it to
            # the call whose window holds its first sighting
            for root in sorted({"/".join(p.split("/")[:3]) for p in seen_obs
                                if p.startswith("/tmp/pip-download-")}):
                first = min(t for p, t in seen_obs.items()
                            if p == root or p.startswith(root + "/"))
                has_content = any(p.startswith(root + "/") for p in seen_obs)
                if start <= ts(first) <= end and has_content:
                    pip_tree = root
                    break
        piped = "|" in cmd
        if re.search(r"\bgit\s+clone\b", cmd) and exit_ == 0 and not bad:
            f["retrieved"] = True
            f["basis"] = "git clone exit 0, no failure signal"
        elif STRONG_OK.search(ev["result"]):
            f["retrieved"] = True
            f["basis"] = ("success line in output: "
                          + STRONG_OK.search(ev["result"]).group(0))
        elif pip_tree:
            f["retrieved"] = True
            f["destinations"] = sorted(set(f["destinations"]) | {pip_tree})
            f["basis"] = (f"pip temp tree {pip_tree} first seen inside this call's "
                          "window, with content beneath it read later")
        elif bad:
            f["basis"] = ("failed, nothing written ("
                          + "; ".join(sorted(set(bad))[:2]) + ")")
        elif exit_ == 0 and (not piped or WEAK_OK.search(ev["result"]) or own_under):
            f["retrieved"] = True
            f["basis"] = "exit 0, no fetch-tool failure signal"
        elif ever:
            f["retrieved"] = True
            f["basis"] = f"destination exists afterwards: {ever[0]}"
        else:
            f["basis"] = (f"exit {exit_} masked by a pipe, no success signal, "
                          "nothing under the destination")

    # Q2: write-set = retrieved dests + observed paths under them, closed
    # under materialization by later commands referencing a write-set path
    write_set = set()
    for ev in fetch_events:
        if ev["fetch"]["retrieved"]:
            for d in ev["fetch"]["destinations"]:
                write_set.add(d)
            for p in seen_obs:
                if any(under(d, [p]) for d in ev["fetch"]["destinations"]):
                    write_set.add(p)
    for ev in events:
        if ev["name"] != "run_code" or ev.get("fetching"):
            continue
        cmd = ev["args"].get("command", "")
        if write_set and any(p in cmd for p in write_set):
            for tok in re.findall(r"/tmp/[A-Za-z0-9_./;$*-]+", cmd):
                tok = tok.rstrip("/;,)'\"")
                if (re.fullmatch(r"/tmp/[A-Za-z0-9_./;$*-]+", tok)
                        and "/llmkit-sandbox" not in tok):
                    write_set.add(tok)

    sk_fail_trial = sum(
        1 for ev in events
        if ev["name"] == "run_code" and SKLEARN_BUILD_ERR in ev["result"])

    # per-unit rollup; touches cover command arguments AND command output
    for rec in units.values():
        rc, fetches, touches = [], [], []
        for ev in events:
            if ev["unit"] != rec["transcript"]:
                continue
            if ev["name"] == "run_code":
                rc.append({"command": ev["args"].get("command", ""),
                           "exit_code": parse_exit(ev["result"]),
                           "fetching": ev["fetching"]})
                if ev["fetching"]:
                    fetches.append(ev["fetch"])
                cmd_hits = sorted({p for p in write_set
                                   if p in ev["args"].get("command", "")})
                out_hits = sorted({p for p in write_set if p in ev["result"]})
                if cmd_hits:
                    touches.append({"tool": "run_code", "via": "argument",
                                    "paths": cmd_hits})
                if out_hits:
                    touches.append({"tool": "run_code", "via": "output",
                                    "paths": out_hits})
            else:
                argstr = json.dumps(ev["args"])
                arg_hits = sorted({p for p in write_set if p in argstr})
                res_hits = sorted({p for p in write_set if p in ev["result"]})
                if arg_hits:
                    touches.append({"tool": ev["name"], "via": "argument",
                                    "paths": arg_hits})
                if res_hits:
                    touches.append({"tool": ev["name"], "via": "output",
                                    "paths": res_hits})
        rec["run_code"] = rc
        rec["n_run_code"] = len(rc)
        rec["fetching_calls"] = fetches
        rec["n_fetching"] = len(fetches)
        rec["n_retrieving"] = sum(1 for f in fetches if f["retrieved"])
        rec["write_set_touches"] = touches
        rec["touches_write_set"] = bool(touches)
    return {"trial": trial.name, "fetch_write_set": sorted(write_set),
            "sklearn_import_failures": sk_fail_trial,
            "units": list(units.values())}


def summarize(arm: str, data: list) -> None:
    """Print the summary lines the README quotes."""
    exits, refused = Counter(), 0
    n_rc = n_fetch = n_ret = n_fail = n_ref = 0
    per_trial, sk_fail = {}, {}
    for tr in data:
        task = tr["trial"].split("__")[0]
        for u in tr["units"]:
            n_rc += u["n_run_code"]
            for r in u["run_code"]:
                if r["exit_code"] == "budget_exhausted":
                    refused += 1
                elif r["exit_code"] is not None:
                    exits[r["exit_code"]] += 1
            for f in u["fetching_calls"]:
                n_fetch += 1
                if f["retrieved"]:
                    n_ret += 1
                elif f["exit"] == "budget_exhausted":
                    n_ref += 1
                else:
                    n_fail += 1
        per_trial[task] = sum(u["n_fetching"] for u in tr["units"])
        if tr.get("sklearn_import_failures"):
            sk_fail[task] = tr["sklearn_import_failures"]
    dist = ", ".join(f"{k}: {v}" for k, v in sorted(exits.items(), key=lambda x: str(x[0])))
    per = ", ".join(f"{t} {n}" for t, n in sorted(per_trial.items()))
    print(f"{arm}: {len(data)} trials, {n_rc} run_code calls "
          f"({n_rc - refused} executed / {refused} refused by budget)")
    print(f"  exit codes (executed): {dist}")
    print(f"  fetching calls: {n_fetch} ({per})")
    print(f"  Q1: {n_ret} retrieved, {n_fail} failed with nothing written, "
          f"{n_ref} refused before running")
    if sk_fail:
        print(f"  sklearn import failures ({SKLEARN_BUILD_ERR}): "
              + ", ".join(f"{t} {n}" for t, n in sorted(sk_fail.items())))


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--jobs", default=DEFAULT_JOBS, help="jobs root")
    ap.add_argument("--out", default="audit-out", help="output directory")
    ap.add_argument("--fresh", action="store_true",
                    help="re-extract transcripts instead of reusing cached ones")
    ap.add_argument("arms", nargs="+", help="control | exec1 | exec2 | ARM:JOB_DIR")
    args = ap.parse_args()
    jobs = Path(args.jobs)
    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    for spec in args.arms:
        arm, _, explicit = spec.partition(":")
        trial_root = Path(explicit) if explicit else jobs / ARM_DIRS[arm]
        trials = sorted(p for p in trial_root.iterdir() if (p / "agent").is_dir())
        extract = out / f"extract-{arm}"
        extract.mkdir(parents=True, exist_ok=True)
        data = [audit_trial(t, extract, args.fresh) for t in trials]
        path = out / f"audit-{arm}.json"
        path.write_text(json.dumps(data, indent=1) + "\n")
        summarize(arm, data)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
