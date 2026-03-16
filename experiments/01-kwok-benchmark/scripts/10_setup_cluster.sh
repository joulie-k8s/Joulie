#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
cd "$ROOT"
KUBECONFIG="$ROOT/experiments/01-kwok-benchmark/kubeconfig.yaml"
export KUBECONFIG
CFG=${1:-experiments/01-kwok-benchmark/configs/benchmark.yaml}
INVENTORY=${2:-experiments/01-kwok-benchmark/configs/cluster-nodes.yaml}

printf '\n1) CHECKING PREREQUISITES\n\n'
./experiments/01-kwok-benchmark/scripts/00_prereqs_check.sh
printf '\n2) CREATING CLUSTER\n\n'
./experiments/01-kwok-benchmark/scripts/01_create_cluster_kwokctl.sh "$CFG"
printf '\n3) GENERATING AND APPLYING CPU-ONLY KWOK NODES\n\n'
./experiments/01-kwok-benchmark/scripts/02_apply_nodes.sh "$INVENTORY"
printf '\nSetup completed.\n'
