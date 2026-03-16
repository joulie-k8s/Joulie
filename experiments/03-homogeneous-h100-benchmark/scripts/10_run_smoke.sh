#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
CLUSTER_CFG="$ROOT/experiments/03-homogeneous-h100-benchmark/configs/cluster.yaml"
INPUT_CFG="${1:-$ROOT/experiments/03-homogeneous-h100-benchmark/configs/cluster-nodes.yaml}"

"$ROOT/experiments/03-homogeneous-h100-benchmark/scripts/00_generate_assets.sh" "$INPUT_CFG"

export CLUSTER_NAME="${CLUSTER_NAME:-joulie-heterogeneous-smoke}"
export ARTIFACT_DIR="${ARTIFACT_DIR:-$ROOT/tmp/heterogeneous-smoke-${CLUSTER_NAME}}"

printf 'running heterogeneous smoke validation with config %s\n' "$CLUSTER_CFG"
printf 'generated inventory input: %s\n' "$INPUT_CFG"
printf 'artifacts dir: %s\n' "$ARTIFACT_DIR"

exec "$ROOT/examples/07 - simulator-gpu-powercaps/run-e2e.sh"
