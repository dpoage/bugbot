"""Score a findings file against a task's ground-truth bug sites.

Runs inside the task container as the Harbor verifier, so it is stdlib-only
and Python 3.6-compatible (SWE-bench images ship old interpreters).

Matching mirrors internal/eval/score.go: a finding matches a ground-truth site
when it names the same repo-relative file and its line lies within the site's
[start, end] range widened by the tolerance. Each site and each finding is
consumed by at most one match (greedy, closest first), so duplicate findings
cannot inflate true positives. Unmatched findings are false positives; they
are a lower bound on precision because a finding outside the gold patch may
still be a real, unlabeled bug.

Usage: score.py FINDINGS GROUND_TRUTH REWARD_OUT
"""

import json
import sys

ABSENT = object()


def norm_path(p):
    p = str(p).replace("\\", "/")
    for prefix in ("/testbed/", "./"):
        if p.startswith(prefix):
            p = p[len(prefix):]
    return p.lstrip("/")


def load_findings(path):
    """Return (findings, valid). Accepts bugbot's `report list --json` rows
    (File/Line/Tier) and the generic {file, line} shape baseline agents
    are instructed to write. A list wrapped as {"findings": [...]} is also
    accepted."""
    try:
        with open(path) as fh:
            raw = json.load(fh)
    except (IOError, OSError, ValueError):
        return [], False
    if isinstance(raw, dict):
        raw = raw.get("findings", ABSENT)
    if raw is None:
        raw = []
    if not isinstance(raw, list):
        return [], False
    out = []
    for row in raw:
        if not isinstance(row, dict):
            continue
        f = row.get("file", row.get("File"))
        line = row.get("line", row.get("Line"))
        try:
            line = int(line)
        except (TypeError, ValueError):
            continue
        if not f:
            continue
        tier = row.get("Tier", row.get("tier"))
        out.append({"file": norm_path(f), "line": line, "tier": tier})
    return out, True


def distance(site, line, tol):
    if site["start"] <= line <= site["end"]:
        return 0
    d = site["start"] - line if line < site["start"] else line - site["end"]
    return d if d <= tol else None


def match(findings, sites, tol):
    pairs = []
    for fi, f in enumerate(findings):
        for si, s in enumerate(sites):
            if f["file"] != s["file"]:
                continue
            d = distance(s, f["line"], tol)
            if d is not None:
                pairs.append((d, fi, si))
    pairs.sort()
    used_f, used_s = set(), set()
    for _, fi, si in pairs:
        if fi in used_f or si in used_s:
            continue
        used_f.add(fi)
        used_s.add(si)
    tp = len(used_s)
    return {
        "detected": 1 if tp > 0 else 0,
        "site_tp": tp,
        "site_fn": len(sites) - tp,
        "fp": len(findings) - len(used_f),
        "findings": len(findings),
    }


def main(argv):
    findings_path, gt_path, out_path = argv[1:4]
    with open(gt_path) as fh:
        gt = json.load(fh)
    sites = [dict(s, file=norm_path(s["file"])) for s in gt["sites"]]
    tol = int(gt.get("tolerance", 5))

    findings, valid = load_findings(findings_path)
    reward = match(findings, sites, tol)
    reward["valid_output"] = 1 if valid else 0

    # Bugbot rows carry a tier (0 fix-witnessed .. 3 suspected). Score the
    # verified subset (tier <= 2) separately so suspected-only noise is
    # visible without changing the headline numbers.
    if any(f["tier"] is not None for f in findings):
        verified = [f for f in findings if f["tier"] is not None and int(f["tier"]) <= 2]
        for k, v in match(verified, sites, tol).items():
            reward["verified_" + k] = v

    with open(out_path, "w") as fh:
        json.dump(reward, fh)
    print(json.dumps(reward))


if __name__ == "__main__":
    main(sys.argv)
