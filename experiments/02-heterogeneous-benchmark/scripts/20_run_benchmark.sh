#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
cd "$ROOT"
KUBECONFIG="$ROOT/experiments/02-heterogeneous-benchmark/kubeconfig.yaml"
export KUBECONFIG
CFG=${1:-experiments/02-heterogeneous-benchmark/configs/benchmark.yaml}
START_EPOCH=$(date +%s)
RUNS_ROOT=${RUNS_ROOT:-experiments/02-heterogeneous-benchmark/runs}

log() {
  local now elapsed
  now=$(date -u --iso-8601=seconds)
  elapsed=$(( $(date +%s) - START_EPOCH ))
  printf '[benchmark] %s +%4ss %s\n' "$now" "$elapsed" "$*"
}

run_step() {
  local label=$1
  shift
  local step_start
  step_start=$(date +%s)
  log "starting: $label"
  "$@"
  log "completed: $label (step_elapsed=$(( $(date +%s) - step_start ))s)"
}

next_run_index() {
  python3 - "$RUNS_ROOT" <<'PY'
import pathlib
import re
import sys

root = pathlib.Path(sys.argv[1])
root.mkdir(parents=True, exist_ok=True)
pat = re.compile(r"^(\d+)_")
max_idx = 0
for path in root.iterdir():
    if not path.is_dir() or path.name == "latest":
        continue
    m = pat.match(path.name)
    if m:
        max_idx = max(max_idx, int(m.group(1)))
print(max_idx + 1)
PY
}

ensure_run_root() {
  if [[ -n "${BENCHMARK_RUN_ROOT:-}" ]]; then
    mkdir -p "$BENCHMARK_RUN_ROOT"
    return
  fi
  local idx stamp uuid
  idx=$(next_run_index)
  stamp=$(date -u +%Y%m%dT%H%M%SZ)
  uuid=$(python3 - <<'PY'
import uuid
print(uuid.uuid4().hex)
PY
)
  BENCHMARK_RUN_ROOT="$RUNS_ROOT/$(printf '%04d' "$idx")_${stamp}_u${uuid}"
  export BENCHMARK_RUN_ROOT
  mkdir -p "$BENCHMARK_RUN_ROOT"
}

ensure_run_root
export RESULTS_DIR=${RESULTS_DIR:-$BENCHMARK_RUN_ROOT/results}
export SIM_DEBUG_PERSIST_DIR=${SIM_DEBUG_PERSIST_DIR:-$BENCHMARK_RUN_ROOT/simulator-debug}
mkdir -p "$RESULTS_DIR" "$SIM_DEBUG_PERSIST_DIR" "$BENCHMARK_RUN_ROOT/logs"
case "$BENCHMARK_RUN_ROOT" in
  /*) ln -sfn "$BENCHMARK_RUN_ROOT" "$RUNS_ROOT/latest" ;;
  *) ln -sfn "$ROOT/$BENCHMARK_RUN_ROOT" "$RUNS_ROOT/latest" ;;
esac

log "benchmark_run_root: $BENCHMARK_RUN_ROOT"
log "results_dir: $RESULTS_DIR"
log "sim_debug_persist_dir: $SIM_DEBUG_PERSIST_DIR"

if [[ "${CLEAN_RESULTS:-false}" == "true" ]]; then
  log "cleaning previous results"
  rm -rf "$RESULTS_DIR"/20*
  rm -rf "$RESULTS_DIR"/plots
  rm -rf "$RESULTS_DIR"/traces
  rm -f "$RESULTS_DIR"/summary.csv
  rm -f "$RESULTS_DIR"/baseline_summary.csv
fi
run_step "prerequisites check" bash experiments/02-heterogeneous-benchmark/scripts/00_prereqs_check.sh
run_step "sanity tests (experiment)" python3 -m pytest experiments/02-heterogeneous-benchmark/scripts/test_sweep.py -q --tb=short
run_step "sanity tests (simulator)" go test -count=1 ./simulator/cmd/workloadgen/... ./simulator/cmd/simulator/...
run_step "sanity tests (contracts)" go test -count=1 ./tests/contracts/...
run_step "benchmark sweep" python3 experiments/02-heterogeneous-benchmark/scripts/05_sweep.py --config "$CFG"
run_step "result collection" python3 experiments/02-heterogeneous-benchmark/scripts/06_collect.py
run_step "plot generation" python3 experiments/02-heterogeneous-benchmark/scripts/07_plot.py
run_step "timeseries plots" python3 experiments/02-heterogeneous-benchmark/scripts/08_plot_timeseries.py
log "heterogeneous benchmark run+collect+plot completed (config=$CFG total_elapsed=$(( $(date +%s) - START_EPOCH ))s)"
