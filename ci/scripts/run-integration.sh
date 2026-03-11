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

kubectl version --client=true || true
kubectl get nodes -o wide

echo "[ci] running integration suite"
echo "[ci] operator image: ${JOULIE_OPERATOR_IMAGE_REPOSITORY:-unset}:${JOULIE_OPERATOR_IMAGE_TAG:-unset}"
echo "[ci] agent image: ${JOULIE_AGENT_IMAGE_REPOSITORY:-unset}:${JOULIE_AGENT_IMAGE_TAG:-unset}"
python3 ci/tests/integration_runner.py
