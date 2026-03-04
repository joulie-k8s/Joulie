#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT"

./scripts/00_prereqs_check.sh
./scripts/01_create_cluster_kwokctl.sh
./scripts/02_apply_nodes.sh

printf 'setup completed\n'
