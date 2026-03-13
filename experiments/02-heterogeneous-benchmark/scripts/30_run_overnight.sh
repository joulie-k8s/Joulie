#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
cd "$ROOT"

CFG=${1:-experiments/02-heterogeneous-benchmark/configs/benchmark-overnight.yaml}
INVENTORY=${2:-experiments/02-heterogeneous-benchmark/configs/cluster-nodes.yaml}
STAMP=$(date -u +%Y%m%dT%H%M%SZ)
ARTIFACT_DIR=${ARTIFACT_DIR:-tmp/heterogeneous-benchmark-overnight-$STAMP}
LOG_FILE="$ARTIFACT_DIR/run.log"

mkdir -p "$ARTIFACT_DIR"

if [[ -f experiments/02-heterogeneous-benchmark/.venv/bin/activate ]]; then
  # shellcheck disable=SC1091
  source experiments/02-heterogeneous-benchmark/.venv/bin/activate
fi

export PYTHONUNBUFFERED=1
export REUSE_EXISTING_CLUSTER=${REUSE_EXISTING_CLUSTER:-true}
export CLEAN_RESULTS=${CLEAN_RESULTS:-true}

{
  echo "[overnight] start: $(date -u --iso-8601=seconds)"
  echo "[overnight] config: $CFG"
  echo "[overnight] inventory: $INVENTORY"
  echo "[overnight] artifact_dir: $ARTIFACT_DIR"
  echo "[overnight] reuse_existing_cluster: $REUSE_EXISTING_CLUSTER"
  echo "[overnight] clean_results: $CLEAN_RESULTS"

  bash experiments/02-heterogeneous-benchmark/scripts/00_generate_assets.sh "$INVENTORY"
  bash experiments/02-heterogeneous-benchmark/scripts/10_setup_cluster.sh "$CFG" "$INVENTORY"
  bash experiments/02-heterogeneous-benchmark/scripts/20_run_benchmark.sh "$CFG"

  mkdir -p "$ARTIFACT_DIR"
  cp "$CFG" "$ARTIFACT_DIR/benchmark-config.yaml"
  cp "$INVENTORY" "$ARTIFACT_DIR/cluster-nodes.yaml"
  cp -R experiments/02-heterogeneous-benchmark/results "$ARTIFACT_DIR/results"

  echo "[overnight] end: $(date -u --iso-8601=seconds)"
  echo "[overnight] completed successfully"
} 2>&1 | tee "$LOG_FILE"
