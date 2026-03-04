#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
CLUSTER_NAME=${CLUSTER_NAME:-joulie-benchmark}
KCTX="kind-${CLUSTER_NAME}"
KIND_CLUSTER_CONFIG=${KIND_CLUSTER_CONFIG:-$ROOT/manifests/kind-cluster.yaml}

if kind get clusters | grep -qx "${CLUSTER_NAME}"; then
  kind delete cluster --name "${CLUSTER_NAME}"
fi

if [[ ! -f "${KIND_CLUSTER_CONFIG}" ]]; then
  echo "missing kind cluster config: ${KIND_CLUSTER_CONFIG}" >&2
  exit 1
fi

kind create cluster --name "${CLUSTER_NAME}" --config "${KIND_CLUSTER_CONFIG}"
kubectl config use-context "${KCTX}"

KWOK_VER=${KWOK_VER:-$(curl -s https://api.github.com/repos/kubernetes-sigs/kwok/releases/latest | python3 -c 'import sys, json; print(json.load(sys.stdin)["tag_name"])')}
kubectl apply -f "https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VER}/kwok.yaml"
kubectl apply -f "https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VER}/stage-fast.yaml"

echo "cluster ready: ${KCTX} with KWOK ${KWOK_VER} (config=${KIND_CLUSTER_CONFIG})"
