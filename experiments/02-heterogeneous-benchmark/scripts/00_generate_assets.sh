#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
CFG="$ROOT/experiments/02-heterogeneous-benchmark/configs/cluster.yaml"
INPUT=${1:-"$ROOT/experiments/02-heterogeneous-benchmark/configs/cluster-nodes.yaml"}
SHEET=${SHEET:-}

GENERATED_DIR="$ROOT/experiments/02-heterogeneous-benchmark/generated"
mkdir -p "$GENERATED_DIR"

python3 "$ROOT/scripts/generate_heterogeneous_assets.py" \
  --input "$INPUT" \
  --sheet "$SHEET" \
  --out-nodes "$GENERATED_DIR/00-kwok-nodes.yaml" \
  --out-classes "$GENERATED_DIR/10-node-classes.yaml" \
  --out-catalog "$GENERATED_DIR/hardware.generated.yaml"

printf 'generated heterogeneous benchmark assets using %s\n' "$INPUT"
printf 'config reference: %s\n' "$CFG"
