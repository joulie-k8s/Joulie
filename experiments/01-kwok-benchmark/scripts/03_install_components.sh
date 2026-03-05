#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
BASELINE=${1:-B}
JOULIE_REGISTRY=${JOULIE_REGISTRY:-registry.cern.ch/mbunino/joulie}
JOULIE_TAG=${JOULIE_TAG:-latest}
SIM_REGISTRY=${SIM_REGISTRY:-registry.cern.ch/mbunino/joulie}
SIM_IMAGE=${SIM_IMAGE:-joulie-simulator}
SIM_TAG=${SIM_TAG:-}
POLICY_TYPE=${POLICY_TYPE:-static_partition}
STATIC_HP_FRAC=${STATIC_HP_FRAC:-0.50}
QUEUE_HP_BASE_FRAC=${QUEUE_HP_BASE_FRAC:-0.60}
QUEUE_HP_MIN=${QUEUE_HP_MIN:-1}
QUEUE_HP_MAX=${QUEUE_HP_MAX:-5}
QUEUE_PERF_PER_HP_NODE=${QUEUE_PERF_PER_HP_NODE:-10}
SIMULATOR_MANIFEST=${SIMULATOR_MANIFEST:-}
SIM_BASE_SPEED_PER_CORE=${SIM_BASE_SPEED_PER_CORE:-}
PERFORMANCE_CAP_WATTS=${PERFORMANCE_CAP_WATTS:-500}
ECO_CAP_WATTS=${ECO_CAP_WATTS:-140}
OPERATOR_RECONCILE_INTERVAL=${OPERATOR_RECONCILE_INTERVAL:-20s}
AGENT_RECONCILE_INTERVAL=${AGENT_RECONCILE_INTERVAL:-10s}

actual_image_from_workload() {
  local ns=$1
  local kindname=$2
  kubectl -n "$ns" get "$kindname" -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || true
}

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
if [[ -n "${SIM_TAG}" ]]; then
  echo "simulator image override requested: ${SIM_REGISTRY}/${SIM_IMAGE}:${SIM_TAG}"
  kubectl -n joulie-sim-demo set image deploy/joulie-telemetry-sim \
    simulator="${SIM_REGISTRY}/${SIM_IMAGE}:${SIM_TAG}"
else
  echo "SIM_TAG is empty; keeping simulator image from manifest"
fi
if [[ -n "${SIM_BASE_SPEED_PER_CORE}" ]]; then
  echo "simulator speed override requested: SIM_BASE_SPEED_PER_CORE=${SIM_BASE_SPEED_PER_CORE}"
  kubectl -n joulie-sim-demo set env deploy/joulie-telemetry-sim \
    SIM_BASE_SPEED_PER_CORE="${SIM_BASE_SPEED_PER_CORE}"
fi
kubectl -n joulie-sim-demo rollout status deploy/joulie-telemetry-sim
SIM_ACTUAL_IMAGE=$(actual_image_from_workload "joulie-sim-demo" "deploy/joulie-telemetry-sim")
echo "simulator deployment image in use: ${SIM_ACTUAL_IMAGE}"
if [[ -n "${SIM_BASE_SPEED_PER_CORE}" ]]; then
  SIM_ACTUAL_SPEED=$(kubectl -n joulie-sim-demo get deploy/joulie-telemetry-sim -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="SIM_BASE_SPEED_PER_CORE")].value}')
  echo "simulator speed in use: SIM_BASE_SPEED_PER_CORE=${SIM_ACTUAL_SPEED}"
fi

if [[ "$BASELINE" == "A" ]]; then
  echo "baseline A selected: simulator only (no operator/agent)"
  helm uninstall joulie -n joulie-system >/dev/null 2>&1 || true
  echo "operator image in use: n/a (baseline A)"
  echo "agent image in use: n/a (baseline A)"
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
    RECONCILE_INTERVAL: "REPLACE_AGENT_RECONCILE_INTERVAL"
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
    RECONCILE_INTERVAL: "REPLACE_OPERATOR_RECONCILE_INTERVAL"
    NODE_SELECTOR: "joulie.io/managed=true"
    ECO_CAP_WATTS: "REPLACE_ECO_CAP_WATTS"
    PERFORMANCE_CAP_WATTS: "REPLACE_PERFORMANCE_CAP_WATTS"
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
  -e "s|REPLACE_PERFORMANCE_CAP_WATTS|${PERFORMANCE_CAP_WATTS}|g" \
  -e "s|REPLACE_ECO_CAP_WATTS|${ECO_CAP_WATTS}|g" \
  -e "s|REPLACE_OPERATOR_RECONCILE_INTERVAL|${OPERATOR_RECONCILE_INTERVAL}|g" \
  -e "s|REPLACE_AGENT_RECONCILE_INTERVAL|${AGENT_RECONCILE_INTERVAL}|g" \
  /tmp/benchmark-values.yaml

helm upgrade --install joulie "$ROOT/../../charts/joulie" -n joulie-system --create-namespace -f /tmp/benchmark-values.yaml
kubectl -n joulie-system rollout status deploy/joulie-operator
kubectl -n joulie-system rollout status statefulset/joulie-agent-pool
OP_ACTUAL_IMAGE=$(actual_image_from_workload "joulie-system" "deploy/joulie-operator")
AGENT_ACTUAL_IMAGE=$(actual_image_from_workload "joulie-system" "statefulset/joulie-agent-pool")

for n in $(kubectl get nodes -l joulie.io/managed=true -o jsonpath='{.items[*].metadata.name}'); do
  sed "s/target-node/$n/g" "$ROOT/manifests/telemetryprofile.yaml" | kubectl apply -f -
done

echo "components installed for baseline ${BASELINE}"
echo "operator policy: ${POLICY_TYPE} (STATIC_HP_FRAC=${STATIC_HP_FRAC})"
echo "operator caps: performance=${PERFORMANCE_CAP_WATTS}W eco=${ECO_CAP_WATTS}W"
echo "reconcile intervals: operator=${OPERATOR_RECONCILE_INTERVAL} agent=${AGENT_RECONCILE_INTERVAL}"
if [[ -n "${SIM_TAG}" ]]; then
  echo "simulator configured image: ${SIM_REGISTRY}/${SIM_IMAGE}:${SIM_TAG}"
else
  echo "simulator configured image: from manifest"
fi
echo "agent configured image: ${JOULIE_REGISTRY}/joulie-agent:${JOULIE_TAG}"
echo "operator configured image: ${JOULIE_REGISTRY}/joulie-operator:${JOULIE_TAG}"
echo "simulator image in use: ${SIM_ACTUAL_IMAGE}"
echo "agent image in use: ${AGENT_ACTUAL_IMAGE}"
echo "operator image in use: ${OP_ACTUAL_IMAGE}"
