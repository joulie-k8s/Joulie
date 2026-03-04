#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
TPL="$ROOT/manifests/kwok_nodes.yaml.tpl"
OUT=$(mktemp)

COUNT=${FAKE_NODE_COUNT:-5}
CPU=${FAKE_NODE_CPU:-32}
MEMORY=${FAKE_NODE_MEMORY:-128Gi}
PODS=${FAKE_NODE_PODS:-110}

for i in $(seq 0 $((COUNT-1))); do
  if (( i % 2 == 0 )); then
    VID=Intel
    VENDOR=GenuineIntel
  else
    VID=AMD
    VENDOR=AuthenticAMD
  fi
  sed -e "s/{{INDEX}}/${i}/g" \
      -e "s/{{CPU_VENDOR_ID}}/${VID}/g" \
      -e "s/{{CPU_VENDOR}}/${VENDOR}/g" \
      -e "s/{{CPU}}/${CPU}/g" \
      -e "s/{{MEMORY}}/${MEMORY}/g" \
      -e "s/{{PODS}}/${PODS}/g" "$TPL" >> "$OUT"
  echo "---" >> "$OUT"
done

kubectl apply -f "$OUT"
rm -f "$OUT"

kubectl wait --for=condition=Ready node -l type=kwok --timeout=120s || true
kubectl get nodes -l type=kwok -o wide
