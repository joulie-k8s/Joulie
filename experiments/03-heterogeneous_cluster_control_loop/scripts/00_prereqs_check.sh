#!/usr/bin/env bash
# Check prerequisites for experiment 03.
set -euo pipefail

ERRORS=0

check() {
  local name="$1"; shift
  if "$@" &>/dev/null; then
    echo "  [OK]  $name"
  else
    echo "  [ERR] $name — not found or failed"
    ERRORS=$((ERRORS+1))
  fi
}

echo "=== Experiment 03 Prerequisites ==="
echo ""
echo "Required:"
check "kubectl"       kubectl version --client
check "kwokctl"       kwokctl --version
check "go >= 1.22"    go version
check "python3"       python3 --version

echo ""
echo "Optional (for plotting):"
check "pip/pandas"    python3 -c "import pandas"
check "pip/matplotlib" python3 -c "import matplotlib"

echo ""
if [ "$ERRORS" -gt 0 ]; then
  echo "FAILED: $ERRORS prerequisite(s) missing."
  exit 1
else
  echo "All required prerequisites found."
fi
