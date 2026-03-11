#!/usr/bin/env bash
set -euo pipefail

ROOT=/src
cd "${ROOT}"

if [[ -z "${KUBECONFIG:-}" || ! -s "${KUBECONFIG:-}" ]]; then
  echo "[ci] waiting for k3s kubeconfig artifact"
  for _ in $(seq 1 120); do
    if [[ -s /k3s-output/kubeconfig.yaml ]]; then
      break
    fi
    sleep 1
  done

  if [[ ! -s /k3s-output/kubeconfig.yaml ]]; then
    echo "[ci] kubeconfig not found at /k3s-output/kubeconfig.yaml and KUBECONFIG is not usable"
    exit 1
  fi

  cp /k3s-output/kubeconfig.yaml /tmp/kubeconfig.yaml
  sed -i 's#https://127.0.0.1:6443#https://k3s:6443#g' /tmp/kubeconfig.yaml
  export KUBECONFIG=/tmp/kubeconfig.yaml
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
python3 ci/tests/integration_runner.py
