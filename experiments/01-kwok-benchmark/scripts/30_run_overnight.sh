#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
cd "$ROOT"
KUBECONFIG="$ROOT/experiments/01-kwok-benchmark/kubeconfig.yaml"
export KUBECONFIG

CFG=${1:-experiments/01-kwok-benchmark/configs/benchmark-overnight.yaml}
INVENTORY=${2:-experiments/01-kwok-benchmark/configs/cluster-nodes.yaml}
RUNS_ROOT=${RUNS_ROOT:-experiments/01-kwok-benchmark/runs}
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

default_run_root() {
  local idx stamp uuid
  idx=$(next_run_index)
  stamp=$(date -u +%Y%m%dT%H%M%SZ)
  uuid=$(python3 - <<'PY'
import uuid
print(uuid.uuid4().hex)
PY
)
  printf '%s/%04d_%s_u%s\n' "$RUNS_ROOT" "$idx" "$stamp" "$uuid"
}

ARTIFACT_DIR=${ARTIFACT_DIR:-$(default_run_root)}
LOG_FILE="$ARTIFACT_DIR/run.log"
START_EPOCH=$(date +%s)

log() {
  local now elapsed
  now=$(date -u --iso-8601=seconds)
  elapsed=$(( $(date +%s) - START_EPOCH ))
  printf '[overnight] %s +%4ss %s\n' "$now" "$elapsed" "$*"
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

mkdir -p "$ARTIFACT_DIR"

if [[ -f experiments/01-kwok-benchmark/.venv/bin/activate ]]; then
  # shellcheck disable=SC1091
  source experiments/01-kwok-benchmark/.venv/bin/activate
elif [[ -f experiments/02-heterogeneous-benchmark/.venv/bin/activate ]]; then
  # shellcheck disable=SC1091
  source experiments/02-heterogeneous-benchmark/.venv/bin/activate
fi

export PYTHONUNBUFFERED=1
export REUSE_EXISTING_CLUSTER=${REUSE_EXISTING_CLUSTER:-true}
export CLEAN_RESULTS=${CLEAN_RESULTS:-true}
export BENCHMARK_RUN_ROOT=${BENCHMARK_RUN_ROOT:-$ARTIFACT_DIR}
export RESULTS_DIR=${RESULTS_DIR:-$BENCHMARK_RUN_ROOT/results}
export SIM_DEBUG_PERSIST_DIR=${SIM_DEBUG_PERSIST_DIR:-$BENCHMARK_RUN_ROOT/simulator-debug}
export GENERATED_CLASSES=${GENERATED_CLASSES:-"$ROOT/experiments/01-kwok-benchmark/generated/10-node-classes.yaml"}
export GENERATED_CATALOG=${GENERATED_CATALOG:-"$ROOT/experiments/01-kwok-benchmark/generated/hardware.generated.yaml"}

mkdir -p "$BENCHMARK_RUN_ROOT" "$RESULTS_DIR" "$SIM_DEBUG_PERSIST_DIR" "$BENCHMARK_RUN_ROOT/logs"
case "$BENCHMARK_RUN_ROOT" in
  /*) ln -sfn "$BENCHMARK_RUN_ROOT" "$RUNS_ROOT/latest" ;;
  *) ln -sfn "$ROOT/$BENCHMARK_RUN_ROOT" "$RUNS_ROOT/latest" ;;
esac

{
  log "start"
  log "config: $CFG"
  log "inventory: $INVENTORY"
  log "artifact_dir: $ARTIFACT_DIR"
  log "benchmark_run_root: $BENCHMARK_RUN_ROOT"
  log "results_dir: $RESULTS_DIR"
  log "sim_debug_persist_dir: $SIM_DEBUG_PERSIST_DIR"
  log "reuse_existing_cluster: $REUSE_EXISTING_CLUSTER"
  log "clean_results: $CLEAN_RESULTS"

  run_step "generate cpu-only assets" bash experiments/01-kwok-benchmark/scripts/00_generate_assets.sh "$INVENTORY"
  run_step "setup cluster" bash experiments/01-kwok-benchmark/scripts/10_setup_cluster.sh "$CFG" "$INVENTORY"
  run_step "run benchmark pipeline" bash experiments/01-kwok-benchmark/scripts/20_run_benchmark.sh "$CFG"

  cp "$CFG" "$ARTIFACT_DIR/benchmark-config.yaml"
  cp "$INVENTORY" "$ARTIFACT_DIR/cluster-nodes.yaml"

  log "results finalized under benchmark run root"
  log "completed successfully (total_elapsed=$(( $(date +%s) - START_EPOCH ))s)"
} 2>&1 | tee "$LOG_FILE"
