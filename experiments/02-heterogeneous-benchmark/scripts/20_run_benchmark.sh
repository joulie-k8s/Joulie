#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
cd "$ROOT"
CFG=${1:-experiments/02-heterogeneous-benchmark/configs/benchmark.yaml}
if [[ "${CLEAN_RESULTS:-false}" == "true" ]]; then
  rm -rf experiments/02-heterogeneous-benchmark/results/20*
  rm -rf experiments/02-heterogeneous-benchmark/results/plots
  rm -rf experiments/02-heterogeneous-benchmark/results/traces
  rm -f experiments/02-heterogeneous-benchmark/results/summary.csv
  rm -f experiments/02-heterogeneous-benchmark/results/baseline_summary.csv
fi
bash experiments/02-heterogeneous-benchmark/scripts/00_prereqs_check.sh
python3 experiments/02-heterogeneous-benchmark/scripts/05_sweep.py --config "$CFG"
python3 experiments/02-heterogeneous-benchmark/scripts/06_collect.py
python3 experiments/02-heterogeneous-benchmark/scripts/07_plot.py
printf 'heterogeneous benchmark run+collect+plot completed (config=%s)\n' "$CFG"
