#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
EXP_ROOT="$ROOT/experiments/02-heterogeneous-benchmark"
EXAMPLE_DIR="$ROOT/examples/07 - simulator-gpu-powercaps"
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
QUEUE_HP_MAX=${QUEUE_HP_MAX:-8}
QUEUE_PERF_PER_HP_NODE=${QUEUE_PERF_PER_HP_NODE:-10}
SIMULATOR_MANIFEST=${SIMULATOR_MANIFEST:-$EXAMPLE_DIR/manifests/20-simulator.yaml}
SIM_BASE_SPEED_PER_CORE=${SIM_BASE_SPEED_PER_CORE:-}
PERFORMANCE_CAP_WATTS=${PERFORMANCE_CAP_WATTS:-500}
ECO_CAP_WATTS=${ECO_CAP_WATTS:-140}
GPU_PERFORMANCE_CAP_PCT_OF_MAX=${GPU_PERFORMANCE_CAP_PCT_OF_MAX:-100}
GPU_ECO_CAP_PCT_OF_MAX=${GPU_ECO_CAP_PCT_OF_MAX:-60}
GPU_WRITE_ABSOLUTE_CAPS=${GPU_WRITE_ABSOLUTE_CAPS:-true}
OPERATOR_RECONCILE_INTERVAL=${OPERATOR_RECONCILE_INTERVAL:-20s}
AGENT_RECONCILE_INTERVAL=${AGENT_RECONCILE_INTERVAL:-10s}
GENERATED_CLASSES=${GENERATED_CLASSES:-$EXAMPLE_DIR/manifests/10-node-classes.yaml}
GENERATED_CATALOG=${GENERATED_CATALOG:-$ROOT/simulator/catalog/hardware.generated.yaml}

actual_image_from_workload() {
  local ns=$1
  local kindname=$2
  kubectl -n "$ns" get "$kindname" -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || true
}

create_or_update_configmap_from_file() {
  local ns=$1
  local name=$2
  local key=$3
  local path=$4
  kubectl -n "$ns" create configmap "$name" --from-file="$key=$path" --dry-run=client -o yaml | kubectl apply -f -
}

if [[ "$BASELINE" == "B" ]]; then
  POLICY_TYPE=static_partition
elif [[ "$BASELINE" == "C" ]]; then
  POLICY_TYPE=queue_aware_v1
fi

kubectl create namespace joulie-sim-demo --dry-run=client -o yaml | kubectl apply -f -
create_or_update_configmap_from_file joulie-sim-demo joulie-simulator-node-classes node-classes.yaml "$GENERATED_CLASSES"
create_or_update_configmap_from_file joulie-sim-demo joulie-simulator-hardware-catalog hardware.generated.yaml "$GENERATED_CATALOG"
cat <<'YAML' | kubectl apply -f -
apiVersion: v1
kind: ConfigMap
metadata:
  name: joulie-simulator-workload-trace
  namespace: joulie-sim-demo
data:
  trace.jsonl: |
    {"type":"job","schemaVersion":"v1","jobId":"bootstrap-cpu","submitTimeOffsetSec":0,"namespace":"default","podTemplate":{"requests":{"cpu":"1","memory":"64Mi"}},"work":{"cpuUnits":1,"gpuUnits":0},"sensitivity":{"cpu":0.5,"gpu":1.0}}
YAML

kubectl apply -f "$SIMULATOR_MANIFEST"
kubectl -n joulie-sim-demo patch deploy/joulie-telemetry-sim --type='json' -p='[
  {"op":"replace","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"IfNotPresent"}
]'
kubectl -n joulie-sim-demo set env deploy/joulie-telemetry-sim \
  SIM_NODE_CLASS_CONFIG=/etc/joulie-sim/node-classes.yaml \
  SIM_HARDWARE_CATALOG_PATH=/etc/joulie-sim-catalog/hardware.generated.yaml
if [[ -n "$SIM_BASE_SPEED_PER_CORE" ]]; then
  kubectl -n joulie-sim-demo set env deploy/joulie-telemetry-sim SIM_BASE_SPEED_PER_CORE="$SIM_BASE_SPEED_PER_CORE"
fi
if [[ -n "$SIM_TAG" ]]; then
  kubectl -n joulie-sim-demo set image deploy/joulie-telemetry-sim simulator="${SIM_REGISTRY}/${SIM_IMAGE}:${SIM_TAG}"
fi
HAS_HW_CATALOG=$(kubectl -n joulie-sim-demo get deploy/joulie-telemetry-sim -o jsonpath='{range .spec.template.spec.volumes[*]}{.name}{"\n"}{end}' 2>/dev/null | grep -xc 'hardware-catalog' || true)
if [[ "$HAS_HW_CATALOG" == "0" ]]; then
  kubectl -n joulie-sim-demo patch deploy/joulie-telemetry-sim --type='json' -p='[
    {"op":"add","path":"/spec/template/spec/containers/0/volumeMounts/-","value":{"name":"hardware-catalog","mountPath":"/etc/joulie-sim-catalog","readOnly":true}},
    {"op":"add","path":"/spec/template/spec/volumes/-","value":{"name":"hardware-catalog","configMap":{"name":"joulie-simulator-hardware-catalog"}}}
  ]'
fi
kubectl -n joulie-sim-demo rollout status deploy/joulie-telemetry-sim
SIM_ACTUAL_IMAGE=$(actual_image_from_workload "joulie-sim-demo" "deploy/joulie-telemetry-sim")

echo "simulator deployment image in use: ${SIM_ACTUAL_IMAGE}"

kubectl apply -f "$ROOT/charts/joulie/crds/joulie.io_nodepowerprofiles.yaml"
kubectl apply -f "$ROOT/charts/joulie/crds/joulie.io_nodehardwares.yaml"
kubectl apply -f "$ROOT/charts/joulie/crds/joulie.io_telemetryprofiles.yaml"

if [[ "$BASELINE" == "A" ]]; then
  echo "baseline A selected: simulator only (no operator/agent)"
  helm uninstall joulie -n joulie-system >/dev/null 2>&1 || true
  exit 0
fi

helm upgrade --install joulie "$ROOT/charts/joulie" -n joulie-system --create-namespace \
  -f "$EXAMPLE_DIR/manifests/30-joulie-values-pool.yaml" \
  --set "agent.image.repository=${JOULIE_REGISTRY}/joulie-agent" \
  --set "agent.image.tag=${JOULIE_TAG}" \
  --set "agent.image.pullPolicy=IfNotPresent" \
  --set "operator.image.repository=${JOULIE_REGISTRY}/joulie-operator" \
  --set "operator.image.tag=${JOULIE_TAG}" \
  --set "operator.image.pullPolicy=IfNotPresent" \
  --set "agent.env.RECONCILE_INTERVAL=${AGENT_RECONCILE_INTERVAL}" \
  --set "operator.env.RECONCILE_INTERVAL=${OPERATOR_RECONCILE_INTERVAL}" \
  --set "operator.env.POLICY_TYPE=${POLICY_TYPE}" \
  --set "operator.env.STATIC_HP_FRAC=${STATIC_HP_FRAC}" \
  --set "operator.env.QUEUE_HP_BASE_FRAC=${QUEUE_HP_BASE_FRAC}" \
  --set "operator.env.QUEUE_HP_MIN=${QUEUE_HP_MIN}" \
  --set "operator.env.QUEUE_HP_MAX=${QUEUE_HP_MAX}" \
  --set "operator.env.QUEUE_PERF_PER_HP_NODE=${QUEUE_PERF_PER_HP_NODE}" \
  --set "operator.env.PERFORMANCE_CAP_WATTS=${PERFORMANCE_CAP_WATTS}" \
  --set "operator.env.ECO_CAP_WATTS=${ECO_CAP_WATTS}" \
  --set "operator.env.GPU_PERFORMANCE_CAP_PCT_OF_MAX=${GPU_PERFORMANCE_CAP_PCT_OF_MAX}" \
  --set "operator.env.GPU_ECO_CAP_PCT_OF_MAX=${GPU_ECO_CAP_PCT_OF_MAX}" \
  --set "operator.env.GPU_WRITE_ABSOLUTE_CAPS=${GPU_WRITE_ABSOLUTE_CAPS}"

kubectl -n joulie-system rollout status deploy/joulie-operator
kubectl -n joulie-system rollout status statefulset/joulie-agent-pool

for n in $(kubectl get nodes -l joulie.io/managed=true -o jsonpath='{.items[*].metadata.name}'); do
  sed "s/TARGET_NODE/$n/g" "$EXAMPLE_DIR/manifests/40-telemetryprofile-template.yaml" | kubectl apply -f -
done

echo "components installed for baseline ${BASELINE}"
