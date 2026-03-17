#!/usr/bin/env bash
set -euo pipefail

ROOT=/src
cd "${ROOT}"

if [[ -z "${KUBECONFIG:-}" || ! -s "${KUBECONFIG:-}" ]]; then
  echo "[ci] KUBECONFIG is not set or not readable"
  exit 1
fi

echo "[ci] waiting for API server readiness"
for _ in $(seq 1 120); do
  if kubectl get nodes >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

echo "[ci] waiting for 2 nodes to register (server + worker)"
for i in $(seq 1 180); do
  count=$(kubectl get nodes --no-headers 2>/dev/null | wc -l)
  if [[ "${count}" -ge 2 ]]; then
    break
  fi
  if (( i % 10 == 0 )); then
    echo "[ci] still waiting for 2 nodes (have ${count}) after ${i}x2s..."
    kubectl get nodes --no-headers 2>/dev/null || true
  fi
  sleep 2
done
count=$(kubectl get nodes --no-headers 2>/dev/null | wc -l)
if [[ "${count}" -lt 2 ]]; then
  echo "[ci] ERROR: timed out waiting for 2 nodes (have ${count})"
  kubectl get nodes -o wide || true
  exit 1
fi

echo "[ci] waiting for both nodes to be Ready"
for i in $(seq 1 120); do
  ready=$(kubectl get nodes --no-headers 2>/dev/null | grep -c " Ready " || true)
  if [[ "${ready}" -ge 2 ]]; then
    break
  fi
  if (( i % 10 == 0 )); then
    echo "[ci] still waiting for 2 Ready nodes (have ${ready}) after ${i}x3s..."
    kubectl get nodes --no-headers 2>/dev/null || true
  fi
  sleep 3
done

kubectl version --client=true || true
kubectl get nodes -o wide

echo "[ci] running integration suite"
echo "[ci] operator image: ${JOULIE_OPERATOR_IMAGE_REPOSITORY:-unset}:${JOULIE_OPERATOR_IMAGE_TAG:-unset}"
echo "[ci] agent image: ${JOULIE_AGENT_IMAGE_REPOSITORY:-unset}:${JOULIE_AGENT_IMAGE_TAG:-unset}"
echo "[ci] scheduler image: ${JOULIE_SCHEDULER_IMAGE_REPOSITORY:-unset}:${JOULIE_SCHEDULER_IMAGE_TAG:-unset}"
python3 ci/tests/integration_runner.py
