#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
BASELINE=${1:-B}
JOULIE_REGISTRY=${JOULIE_REGISTRY:-registry.cern.ch/mbunino/joulie}
JOULIE_TAG=${JOULIE_TAG:-latest}
SIM_REGISTRY=${SIM_REGISTRY:-registry.cern.ch/mbunino/joulie}
SIM_IMAGE=${SIM_IMAGE:-joulie-simulator}
SIM_TAG=${SIM_TAG:-latest}
POLICY_TYPE=${POLICY_TYPE:-static_partition}
STATIC_HP_FRAC=${STATIC_HP_FRAC:-0.50}
QUEUE_HP_BASE_FRAC=${QUEUE_HP_BASE_FRAC:-0.60}
QUEUE_HP_MIN=${QUEUE_HP_MIN:-1}
QUEUE_HP_MAX=${QUEUE_HP_MAX:-5}
QUEUE_PERF_PER_HP_NODE=${QUEUE_PERF_PER_HP_NODE:-10}
SIMULATOR_MANIFEST=${SIMULATOR_MANIFEST:-}

if [[ "$BASELINE" == "B" ]]; then
  POLICY_TYPE=static_partition
elif [[ "$BASELINE" == "C" ]]; then
  POLICY_TYPE=queue_aware_v1
fi

if [[ -z "$SIMULATOR_MANIFEST" ]]; then
  if [[ -f "$ROOT/../../examples/06-simulator-kwok/manifests/10-simulator.yaml" ]]; then
    SIMULATOR_MANIFEST="$ROOT/../../examples/06-simulator-kwok/manifests/10-simulator.yaml"
  else
    echo "error: simulator manifest not found in expected locations." >&2
    echo "set SIMULATOR_MANIFEST to the full path of 10-simulator.yaml" >&2
    exit 1
  fi
fi

kubectl apply -f "$SIMULATOR_MANIFEST"
kubectl -n joulie-sim-demo set image deploy/joulie-telemetry-sim \
  simulator="${SIM_REGISTRY}/${SIM_IMAGE}:${SIM_TAG}"
kubectl -n joulie-sim-demo rollout status deploy/joulie-telemetry-sim

if [[ "$BASELINE" == "A" ]]; then
  echo "baseline A selected: simulator only (no operator/agent)"
  helm uninstall joulie -n joulie-system >/dev/null 2>&1 || true
  exit 0
fi

cat > /tmp/benchmark-values.yaml <<'YAML'
agent:
  image:
    repository: REPLACE_AGENT_REPO
    tag: REPLACE_AGENT_TAG
  mode: pool
  hostNetwork: false
  privileged: false
  pool:
    replicas: 2
    shards: 2
    nodeSelector: "joulie.io/managed=true"
  env:
    RECONCILE_INTERVAL: "10s"
    METRICS_ADDR: ":8080"
    SIMULATE_ONLY: "false"
operator:
  image:
    repository: REPLACE_OPERATOR_REPO
    tag: REPLACE_OPERATOR_TAG
  env:
    POLICY_TYPE: REPLACE_POLICY_TYPE
    STATIC_HP_FRAC: "REPLACE_STATIC_HP_FRAC"
    QUEUE_HP_BASE_FRAC: "REPLACE_QUEUE_HP_BASE_FRAC"
    QUEUE_HP_MIN: "REPLACE_QUEUE_HP_MIN"
    QUEUE_HP_MAX: "REPLACE_QUEUE_HP_MAX"
    QUEUE_PERF_PER_HP_NODE: "REPLACE_QUEUE_PERF_PER_HP_NODE"
    RECONCILE_INTERVAL: "20s"
    NODE_SELECTOR: "joulie.io/managed=true"
    ECO_CAP_WATTS: "140"
    PERFORMANCE_CAP_WATTS: "500"
YAML

sed -i \
  -e "s|REPLACE_AGENT_REPO|${JOULIE_REGISTRY}/joulie-agent|g" \
  -e "s|REPLACE_AGENT_TAG|${JOULIE_TAG}|g" \
  -e "s|REPLACE_OPERATOR_REPO|${JOULIE_REGISTRY}/joulie-operator|g" \
  -e "s|REPLACE_OPERATOR_TAG|${JOULIE_TAG}|g" \
  -e "s|REPLACE_POLICY_TYPE|${POLICY_TYPE}|g" \
  -e "s|REPLACE_STATIC_HP_FRAC|${STATIC_HP_FRAC}|g" \
  -e "s|REPLACE_QUEUE_HP_BASE_FRAC|${QUEUE_HP_BASE_FRAC}|g" \
  -e "s|REPLACE_QUEUE_HP_MIN|${QUEUE_HP_MIN}|g" \
  -e "s|REPLACE_QUEUE_HP_MAX|${QUEUE_HP_MAX}|g" \
  -e "s|REPLACE_QUEUE_PERF_PER_HP_NODE|${QUEUE_PERF_PER_HP_NODE}|g" \
  /tmp/benchmark-values.yaml

helm upgrade --install joulie "$ROOT/../../charts/joulie" -n joulie-system --create-namespace -f /tmp/benchmark-values.yaml
kubectl -n joulie-system rollout status deploy/joulie-operator
kubectl -n joulie-system rollout status statefulset/joulie-agent-pool

for n in $(kubectl get nodes -l joulie.io/managed=true -o jsonpath='{.items[*].metadata.name}'); do
  sed "s/target-node/$n/g" "$ROOT/manifests/telemetryprofile.yaml" | kubectl apply -f -
done

echo "components installed for baseline ${BASELINE}"
echo "operator policy: ${POLICY_TYPE} (STATIC_HP_FRAC=${STATIC_HP_FRAC})"
echo "simulator image: ${SIM_REGISTRY}/${SIM_IMAGE}:${SIM_TAG}"
echo "agent image: ${JOULIE_REGISTRY}/joulie-agent:${JOULIE_TAG}"
echo "operator image: ${JOULIE_REGISTRY}/joulie-operator:${JOULIE_TAG}"
