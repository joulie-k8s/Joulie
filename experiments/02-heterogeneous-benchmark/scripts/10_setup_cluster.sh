#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
cd "$ROOT"
KUBECONFIG="$ROOT/experiments/02-heterogeneous-benchmark/kubeconfig.yaml"
export KUBECONFIG
CFG=${1:-experiments/02-heterogeneous-benchmark/configs/benchmark.yaml}
INVENTORY=${2:-experiments/02-heterogeneous-benchmark/configs/cluster-nodes.yaml}

printf '\n1) CHECKING PREREQUISITES\n\n'
./experiments/02-heterogeneous-benchmark/scripts/00_prereqs_check.sh
printf '\n2) CREATING CLUSTER\n\n'
./experiments/02-heterogeneous-benchmark/scripts/01_create_cluster_kwokctl.sh "$CFG"
printf '\n3) GENERATING AND APPLYING HETEROGENEOUS KWOK NODES\n\n'
./experiments/02-heterogeneous-benchmark/scripts/02_apply_nodes.sh "$INVENTORY"
printf '\nSetup completed.\n'
