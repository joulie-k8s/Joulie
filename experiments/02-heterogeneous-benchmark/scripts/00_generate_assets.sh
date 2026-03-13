#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
CFG="$ROOT/experiments/02-heterogeneous-benchmark/configs/cluster.yaml"
INPUT=${1:-"$ROOT/experiments/02-heterogeneous-benchmark/configs/cluster-nodes.yaml"}
SHEET=${SHEET:-}

python3 "$ROOT/scripts/generate_heterogeneous_assets.py" \
  --input "$INPUT" \
  --sheet "$SHEET" \
  --out-nodes "$ROOT/examples/07 - simulator-gpu-powercaps/manifests/00-kwok-nodes.yaml" \
  --out-classes "$ROOT/examples/07 - simulator-gpu-powercaps/manifests/10-node-classes.yaml" \
  --out-catalog "$ROOT/simulator/catalog/hardware.generated.yaml"

printf 'generated heterogeneous benchmark assets using %s\n' "$INPUT"
printf 'config reference: %s\n' "$CFG"
