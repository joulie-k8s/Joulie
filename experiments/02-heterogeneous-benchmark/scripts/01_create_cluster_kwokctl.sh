#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
EXP_ROOT="$ROOT/experiments/02-heterogeneous-benchmark"
CFG=${1:-$EXP_ROOT/configs/benchmark.yaml}
CLUSTER_NAME=${CLUSTER_NAME:-joulie-heterogeneous-benchmark}
REUSE_EXISTING_CLUSTER=${REUSE_EXISTING_CLUSTER:-false}
KIND_CLUSTER_CONFIG=${KIND_CLUSTER_CONFIG:-$(python3 - <<'PY' "$CFG"
import pathlib, sys, yaml
cfg = yaml.safe_load(pathlib.Path(sys.argv[1]).read_text()) or {}
print(cfg.get("install", {}).get("kind_cluster_config", "examples/07 - simulator-gpu-powercaps/manifests/01-kind-cluster.yaml"))
PY
)}
KIND_CLUSTER_CONFIG="$ROOT/${KIND_CLUSTER_CONFIG}"

# Use a per-experiment kubeconfig so kind never touches ~/.kube/config and parallel
# experiments on separate clusters cannot interfere with each other.
KUBECONFIG="$EXP_ROOT/kubeconfig.yaml"
export KUBECONFIG

if kind get clusters | grep -qx "${CLUSTER_NAME}" && [[ "${REUSE_EXISTING_CLUSTER}" == "true" ]]; then
  # Regenerate per-experiment kubeconfig if it was lost (e.g. after a machine restart)
  [[ -f "$KUBECONFIG" ]] || kind export kubeconfig --name "$CLUSTER_NAME" --kubeconfig "$KUBECONFIG"
  echo "reusing existing cluster: kind-${CLUSTER_NAME} (kubeconfig=${KUBECONFIG})"
elif kind get clusters | grep -qx "${CLUSTER_NAME}"; then
  kind delete cluster --name "${CLUSTER_NAME}" --kubeconfig "$KUBECONFIG"
  [[ -f "$KIND_CLUSTER_CONFIG" ]] || { echo "missing kind cluster config: $KIND_CLUSTER_CONFIG" >&2; exit 1; }
  kind create cluster --name "$CLUSTER_NAME" --config "$KIND_CLUSTER_CONFIG" --kubeconfig "$KUBECONFIG"
else
  [[ -f "$KIND_CLUSTER_CONFIG" ]] || { echo "missing kind cluster config: $KIND_CLUSTER_CONFIG" >&2; exit 1; }
  kind create cluster --name "$CLUSTER_NAME" --config "$KIND_CLUSTER_CONFIG" --kubeconfig "$KUBECONFIG"
fi

KWOK_VER=${KWOK_VER:-$(curl -s https://api.github.com/repos/kubernetes-sigs/kwok/releases/latest | python3 -c 'import sys, json; print(json.load(sys.stdin)["tag_name"])')}
kubectl apply -f "https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VER}/kwok.yaml"
kubectl apply -f "https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VER}/stage-fast.yaml"

echo "cluster ready: kind-${CLUSTER_NAME} (kubeconfig=${KUBECONFIG}) with KWOK ${KWOK_VER}"
