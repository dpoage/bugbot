#!/usr/bin/env bash
set -euo pipefail

python3 <<'PYEOF'
with open("cargo_movement.go") as f:
    src = f.read()
assert "func MoveCargo" in src
print("SENTINEL_OK")
PYEOF
