#!/usr/bin/env bash
set -euo pipefail

kind delete cluster --name "${CLUSTER_NAME:-joulie-benchmark}" || true
