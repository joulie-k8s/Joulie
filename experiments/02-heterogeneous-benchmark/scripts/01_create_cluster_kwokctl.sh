#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
EXP_ROOT="$ROOT/experiments/02-heterogeneous-benchmark"
CFG=${1:-$EXP_ROOT/configs/benchmark.yaml}
CLUSTER_NAME=${CLUSTER_NAME:-joulie-heterogeneous-benchmark}
KCTX="kind-${CLUSTER_NAME}"
REUSE_EXISTING_CLUSTER=${REUSE_EXISTING_CLUSTER:-false}
KIND_CLUSTER_CONFIG=${KIND_CLUSTER_CONFIG:-$(python3 - <<'PY' "$CFG"
import pathlib, sys, yaml
cfg = yaml.safe_load(pathlib.Path(sys.argv[1]).read_text()) or {}
print(cfg.get("install", {}).get("kind_cluster_config", "examples/07 - simulator-gpu-powercaps/manifests/01-kind-cluster.yaml"))
PY
)}
KIND_CLUSTER_CONFIG="$ROOT/${KIND_CLUSTER_CONFIG}"

if kind get clusters | grep -qx "${CLUSTER_NAME}" && [[ "${REUSE_EXISTING_CLUSTER}" == "true" ]]; then
  kubectl config use-context "$KCTX"
  echo "reusing existing cluster: ${KCTX}"
elif kind get clusters | grep -qx "${CLUSTER_NAME}"; then
  kind delete cluster --name "${CLUSTER_NAME}"
  [[ -f "$KIND_CLUSTER_CONFIG" ]] || { echo "missing kind cluster config: $KIND_CLUSTER_CONFIG" >&2; exit 1; }
  kind create cluster --name "$CLUSTER_NAME" --config "$KIND_CLUSTER_CONFIG"
  kubectl config use-context "$KCTX"
else
  [[ -f "$KIND_CLUSTER_CONFIG" ]] || { echo "missing kind cluster config: $KIND_CLUSTER_CONFIG" >&2; exit 1; }
  kind create cluster --name "$CLUSTER_NAME" --config "$KIND_CLUSTER_CONFIG"
  kubectl config use-context "$KCTX"
fi

KWOK_VER=${KWOK_VER:-$(curl -s https://api.github.com/repos/kubernetes-sigs/kwok/releases/latest | python3 -c 'import sys, json; print(json.load(sys.stdin)["tag_name"])')}
kubectl apply -f "https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VER}/kwok.yaml"
kubectl apply -f "https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VER}/stage-fast.yaml"

echo "cluster ready: ${KCTX} with KWOK ${KWOK_VER} (config=${KIND_CLUSTER_CONFIG})"
