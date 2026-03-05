#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT"

printf '\n1) CHECKING PREREQUISITES\n\n'
./scripts/00_prereqs_check.sh

printf '\n2) CREATING CLUSTER\n\n'
./scripts/01_create_cluster_kwokctl.sh

printf '\n3) CREATING KWOK NODES\n\n'
./scripts/02_apply_nodes.sh

printf '\nSetup completed.\n'
