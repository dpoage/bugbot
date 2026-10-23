#!/usr/bin/env python3
"""Turn Harbor's SWE-bench Verified tasks into find-the-bug tasks.

Each generated task reuses the instance's prebuilt SWE-bench image (repo at
the buggy base commit) but replaces the fix-the-issue framing:

* instruction: audit a scoped slice of the repo and report findings; the
  issue text is withheld, because bugbot hunts unprompted;
* ground truth: the old-side line ranges the gold patch touches, one site
  per hunk, hidden in tests/ until the agent has finished;
* verifier: score.py's file+line-tolerance 1:1 matcher (internal/eval rules).

Git history is squashed to a single commit in the image so no agent can
recover the fix from later commits still in the object store.

Usage:
  harbor download swebench-verified@1.0 -o /tmp/hb
  bench/harbor/adapter.py --source /tmp/hb/swebench-verified \
      --out bench/harbor/out/bugfind-swebench --instances bench/harbor/instances.txt
"""

import argparse
import json
import posixpath
import re
import shutil
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
TEMPLATE = HERE / "template"

HUNK_RE = re.compile(r"^@@ -(\d+)(?:,(\d+))? \+\d+(?:,\d+)? @@")
FILE_RE = re.compile(r"^diff --git a/(\S+) b/\S+")
FROM_RE = re.compile(r"^FROM\s+(\S+)", re.M)
TEST_PATH_RE = re.compile(r"(^|/)(tests?|testing)(/|$)|(^|/)test_[^/]*\.py$|_test\.py$")


def gold_sites(patch: str) -> list[dict]:
    """One site per hunk: the old-side line span of its changed lines.

    Removed lines contribute their own old line number. A pure insertion
    contributes the old lines on either side of the insertion point, since
    the defect is the missing code between them.
    """
    sites: list[dict] = []
    cur_file = None
    in_hunk = False
    old = 0
    lines: list[int] = []

    def flush():
        if cur_file is not None and lines:
            sites.append({"file": cur_file, "start": min(lines), "end": max(lines)})

    for raw in patch.splitlines():
        m = FILE_RE.match(raw)
        if m:
            flush()
            lines, in_hunk, cur_file = [], False, m.group(1)
            continue
        m = HUNK_RE.match(raw)
        if m:
            flush()
            lines, in_hunk, old = [], True, int(m.group(1))
            continue
        if not in_hunk:
            continue  # ---/+++ and index headers
        if raw.startswith("-"):
            lines.append(old)
            old += 1
        elif raw.startswith("+"):
            # Insertion point sits between old-1 and old.
            lines.extend([max(old - 1, 1), old])
        elif raw.startswith(" ") or raw == "":
            old += 1
    flush()
    return sites


def scope_globs(sites: list[dict], mode: str) -> list[str]:
    files = sorted({s["file"] for s in sites})
    if mode == "file":
        return files
    dirs = sorted({posixpath.dirname(f) for f in files})
    return [posixpath.join(d, "*.py") if d else "*.py" for d in dirs]


def build_task(src: Path, dst: Path, scope_mode: str, tolerance: int, env_tag: str = "") -> dict:
    cfg = json.loads((src / "tests" / "config.json").read_text())
    sites = [s for s in gold_sites(cfg["patch"]) if not TEST_PATH_RE.search(s["file"])]
    if not sites:
        raise ValueError(f"{cfg['instance_id']}: gold patch touches no non-test source")
    image = FROM_RE.search((src / "environment" / "Dockerfile").read_text())
    if not image:
        raise ValueError(f"{cfg['instance_id']}: no FROM line in source Dockerfile")
    globs = scope_globs(sites, scope_mode)

    if dst.exists():
        shutil.rmtree(dst)
    (dst / "environment").mkdir(parents=True)
    (dst / "tests").mkdir()

    scope_lines = "\\n".join(globs)
    (dst / "environment" / "Dockerfile").write_text(
        f"FROM {image.group(1)}\n"
        # Harbor names the image after the environment's content, so a distinct
        # tag per arm gives concurrent jobs on the same task separate images.
        + (f"# env-tag: {env_tag}\n" if env_tag else "")
        + "WORKDIR /testbed\n"
        "# Squash history: later commits (including the fix) must not be recoverable.\n"
        "RUN rm -rf .git && git init -q && git add -A"
        " && git -c user.name=bench -c user.email=bench@localhost commit -qm base\n"
        f"RUN mkdir -p /logs /task && printf '{scope_lines}\\n' > /task/scope.txt\n"
    )
    bullets = "\n".join(f"- `{g}`" for g in globs)
    (dst / "instruction.md").write_text(
        (TEMPLATE / "instruction.md").read_text().replace("{scope}", bullets)
    )
    (dst / "task.toml").write_text(
        "[metadata]\n"
        f'source_instance = "{cfg["instance_id"]}"\n'
        f'repo = "{cfg["repo"]}"\n'
        f'scope = "{scope_mode}"\n'
        'category = "bug-finding"\n'
        'tags = ["bug-finding", "swe-bench-derived"]\n'
        "\n[verifier]\n"
        'environment_mode = "shared"\n'
        "timeout_sec = 300\n"
        "\n[agent]\n"
        "timeout_sec = 7200\n"
        "\n[environment]\n"
        "build_timeout_sec = 1800.0\n"
        "cpus = 2\n"
        "memory = '4G'\n"
        "storage = '10G'\n"
    )
    (dst / "tests" / "ground_truth.json").write_text(
        json.dumps(
            {
                "instance_id": cfg["instance_id"],
                "base_commit": cfg["base_commit"],
                "tolerance": tolerance,
                "sites": sites,
            },
            indent=2,
        )
        + "\n"
    )
    for name in ("test.sh", "score.py"):
        shutil.copy2(TEMPLATE / name, dst / "tests" / name)
    return {"instance": cfg["instance_id"], "scope": globs, "sites": sites}


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--source", type=Path, required=True, help="downloaded swebench-verified dataset dir")
    ap.add_argument("--out", type=Path, required=True, help="output dataset dir")
    ap.add_argument("--instances", type=Path, required=True, help="file of instance ids, one per line (# comments ok)")
    ap.add_argument("--scope", choices=("dir", "file"), default="dir",
                    help="dir: the gold file's directory (*.py, non-recursive); file: the gold file only")
    ap.add_argument("--tolerance", type=int, default=5, help="line tolerance around each gold hunk")
    ap.add_argument("--env-tag", default="",
                    help="label baked into each Dockerfile so concurrent jobs (one dataset copy "
                         "per arm) get distinct images; Harbor otherwise shares one image per task")
    args = ap.parse_args()

    ids = [
        ln.split("#", 1)[0].strip()
        for ln in args.instances.read_text().splitlines()
        if ln.split("#", 1)[0].strip()
    ]
    args.out.mkdir(parents=True, exist_ok=True)
    for iid in ids:
        info = build_task(args.source / iid, args.out / iid, args.scope, args.tolerance, args.env_tag)
        print(json.dumps(info))
    return 0


if __name__ == "__main__":
    sys.exit(main())
