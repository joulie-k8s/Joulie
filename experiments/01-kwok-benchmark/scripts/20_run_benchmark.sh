#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT/.."/..

CFG=${1:-experiments/01-kwok-benchmark/configs/benchmark.yaml}

python3 experiments/01-kwok-benchmark/scripts/05_sweep.py --config "$CFG"
python3 experiments/01-kwok-benchmark/scripts/06_collect.py
python3 experiments/01-kwok-benchmark/scripts/07_plot.py

printf 'benchmark run+collect+plot completed (config=%s)\n' "$CFG"
