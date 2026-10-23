#!/bin/bash
# Harbor verifier: score the agent's findings against the hidden gold-patch
# bug sites. Ground truth lives only in /tests, which Harbor uploads after the
# agent finishes, so the agent never sees it.
set -euo pipefail

mkdir -p /logs/verifier
PY=""
for cand in /usr/bin/python3 /usr/local/bin/python3 /opt/miniconda3/bin/python python3; do
  if command -v "$cand" >/dev/null 2>&1; then PY="$cand"; break; fi
done
if [ -z "$PY" ]; then
  echo "no python interpreter found for scoring" >&2
  exit 1
fi

"$PY" /tests/score.py /logs/agent/findings.json /tests/ground_truth.json /logs/verifier/reward.json
