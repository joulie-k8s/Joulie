#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
INPUT=${1:-"$ROOT/experiments/01-kwok-benchmark/configs/cluster-nodes.yaml"}
SHEET=${SHEET:-}

GENERATED_DIR="$ROOT/experiments/01-kwok-benchmark/generated"
mkdir -p "$GENERATED_DIR"

python3 "$ROOT/scripts/generate_heterogeneous_assets.py" \
  --input "$INPUT" \
  --sheet "$SHEET" \
  --out-nodes "$GENERATED_DIR/00-kwok-nodes.yaml" \
  --out-classes "$GENERATED_DIR/10-node-classes.yaml" \
  --out-catalog "$GENERATED_DIR/hardware.generated.yaml"

printf 'generated cpu-only benchmark assets using %s\n' "$INPUT"
