#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
INPUT_CFG=${1:-$ROOT/experiments/02-heterogeneous-benchmark/configs/cluster-nodes.yaml}
NODES_MANIFEST="$ROOT/experiments/02-heterogeneous-benchmark/generated/00-kwok-nodes.yaml"

"$ROOT/experiments/02-heterogeneous-benchmark/scripts/00_generate_assets.sh" "$INPUT_CFG"
kubectl apply -f "$NODES_MANIFEST"
kubectl get nodes -l type=kwok -o wide
