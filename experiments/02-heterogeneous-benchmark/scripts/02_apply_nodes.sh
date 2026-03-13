#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
INPUT_CFG=${1:-$ROOT/experiments/02-heterogeneous-benchmark/configs/cluster-nodes.yaml}
NODES_MANIFEST="$ROOT/examples/07 - simulator-gpu-powercaps/manifests/00-kwok-nodes.yaml"

"$ROOT/experiments/02-heterogeneous-benchmark/scripts/00_generate_assets.sh" "$INPUT_CFG"
kubectl apply -f "$NODES_MANIFEST"
kubectl wait --for=condition=Ready node -l type=kwok --timeout=120s || true
kubectl get nodes -l type=kwok -o wide
