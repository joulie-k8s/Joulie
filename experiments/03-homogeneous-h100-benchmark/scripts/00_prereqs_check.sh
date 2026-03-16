#!/usr/bin/env bash
set -euo pipefail

need_cmds=(kubectl helm kind python3 go)
for c in "${need_cmds[@]}"; do
  command -v "$c" >/dev/null || { echo "missing command: $c"; exit 1; }
  echo "$c: $($c version 2>/dev/null | head -n1 || true)"
done

python3 - <<'PY'
from importlib import util
mods = ["yaml", "pandas", "matplotlib"]
missing = [m for m in mods if util.find_spec(m) is None]
if missing:
    raise SystemExit(
        "missing python modules: "
        + ", ".join(missing)
        + " (install with: python -m pip install -r experiments/02-heterogeneous-benchmark/requirements.txt)"
    )
print("python modules: ok (yaml, pandas, matplotlib)")
PY

echo "prereqs check passed"
