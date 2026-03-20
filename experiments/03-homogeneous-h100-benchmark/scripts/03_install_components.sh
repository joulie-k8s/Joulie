#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
EXP_ROOT="$ROOT/experiments/03-homogeneous-h100-benchmark"
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
SIM_BASE_SPEED_PER_CORE=${SIM_BASE_SPEED_PER_CORE:-}
PERFORMANCE_CAP_WATTS=${PERFORMANCE_CAP_WATTS:-500}
ECO_CAP_WATTS=${ECO_CAP_WATTS:-140}
CPU_PERFORMANCE_CAP_PCT_OF_MAX=${CPU_PERFORMANCE_CAP_PCT_OF_MAX:-100}
CPU_ECO_CAP_PCT_OF_MAX=${CPU_ECO_CAP_PCT_OF_MAX:-60}
GPU_PERFORMANCE_CAP_PCT_OF_MAX=${GPU_PERFORMANCE_CAP_PCT_OF_MAX:-100}
GPU_ECO_CAP_PCT_OF_MAX=${GPU_ECO_CAP_PCT_OF_MAX:-60}
CPU_WRITE_ABSOLUTE_CAPS=${CPU_WRITE_ABSOLUTE_CAPS:-false}
GPU_WRITE_ABSOLUTE_CAPS=${GPU_WRITE_ABSOLUTE_CAPS:-true}
OPERATOR_RECONCILE_INTERVAL=${OPERATOR_RECONCILE_INTERVAL:-20s}
AGENT_RECONCILE_INTERVAL=${AGENT_RECONCILE_INTERVAL:-10s}
GENERATED_CLASSES=${GENERATED_CLASSES:-$EXAMPLE_DIR/manifests/10-node-classes.yaml}
GENERATED_CATALOG=${GENERATED_CATALOG:-$ROOT/simulator/catalog/hardware.generated.yaml}
SIM_FACILITY_AMBIENT_BASE_C=${SIM_FACILITY_AMBIENT_BASE_C:-22.0}
SIM_FACILITY_AMBIENT_AMPLITUDE_C=${SIM_FACILITY_AMBIENT_AMPLITUDE_C:-8.0}
SIM_FACILITY_AMBIENT_PERIOD_SEC=${SIM_FACILITY_AMBIENT_PERIOD_SEC:-600}
ENABLE_FACILITY_METRICS=${ENABLE_FACILITY_METRICS:-false}

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

# ---------------------------------------------------------------------------
# 1. CRDs (needed by both simulator and Joulie components)
# ---------------------------------------------------------------------------
kubectl apply -f "$ROOT/charts/joulie/crds/joulie.io_nodehardwares.yaml"
kubectl apply -f "$ROOT/charts/joulie/crds/joulie.io_nodetwins.yaml"

# ---------------------------------------------------------------------------
# 2. Joulie components (extender must be up before any pod scheduling when
#    ignorable=false in the scheduler extender config)
# ---------------------------------------------------------------------------
if [[ "$BASELINE" == "A" ]]; then
  echo "baseline A selected: simulator only (no operator/agent)"
  helm uninstall joulie -n joulie-system >/dev/null 2>&1 || true
else
  # Baselines B and C both enable the scheduler extender for adaptive placement.
  # On KinD, the extender uses nodeName to bypass the scheduler (chicken-and-egg:
  # the extender pod itself cannot go through the extender filter when ignorable=false).
  # On real clusters, set INFRA_NODE="" to let the scheduler place the extender normally.
  INFRA_NODE=${INFRA_NODE:-${CLUSTER_NAME:-joulie-homogeneous-h100-benchmark}-worker}
  EXTENDER_CLUSTER_IP=${EXTENDER_CLUSTER_IP:-10.96.100.76}
  SCHED_ARGS=(
    --set "schedulerExtender.enabled=true"
    --set "schedulerExtender.clusterIP=${EXTENDER_CLUSTER_IP}"
    --set "schedulerExtender.image.repository=${JOULIE_REGISTRY}/joulie-scheduler"
    --set "schedulerExtender.image.tag=${JOULIE_TAG}"
  )
  if [[ -n "$INFRA_NODE" ]]; then
    SCHED_ARGS+=(--set "schedulerExtender.nodeName=${INFRA_NODE}")
  fi

  INFRA_SELECTOR_ARGS=()
  if [[ -n "$INFRA_NODE" ]]; then
    INFRA_SELECTOR_ARGS=(
      --set-string "agent.pool.podNodeSelector.joulie\\.io/infra=true"
      --set-string "operator.nodeSelector.joulie\\.io/infra=true"
    )
  fi

  # Telemetry env vars for baselines B/C — included in the Helm install to avoid
  # a second rolling restart from kubectl-set-env.
  AGENT_TELEMETRY_ARGS=()
  if [[ "$BASELINE" != "A" ]]; then
    AGENT_TELEMETRY_ARGS=(
      --set "agent.env.TELEMETRY_CPU_SOURCE=http"
      --set "agent.env.TELEMETRY_CPU_HTTP_ENDPOINT=http://joulie-telemetry-sim.joulie-sim-demo.svc.cluster.local/telemetry/{node}"
      --set "agent.env.TELEMETRY_CPU_CONTROL=http"
      --set "agent.env.TELEMETRY_CPU_CONTROL_HTTP_ENDPOINT=http://joulie-telemetry-sim.joulie-sim-demo.svc.cluster.local/control/{node}"
      --set "agent.env.TELEMETRY_CPU_CONTROL_MODE=rapl"
      --set "agent.env.TELEMETRY_GPU_CONTROL=http"
      --set "agent.env.TELEMETRY_GPU_CONTROL_HTTP_ENDPOINT=http://joulie-telemetry-sim.joulie-sim-demo.svc.cluster.local/control/{node}"
      --set "agent.env.TELEMETRY_GPU_CONTROL_MODE=rapl"
      --set "agent.env.JOULIE_TELEMETRY_SOURCE_TYPE=http"
      --set "agent.env.JOULIE_TELEMETRY_HTTP_ENDPOINT=http://joulie-telemetry-sim.joulie-sim-demo.svc.cluster.local/telemetry/{node}"
      --set "agent.env.JOULIE_CONTROL_TYPE=http"
      --set "agent.env.JOULIE_CONTROL_HTTP_ENDPOINT=http://joulie-telemetry-sim.joulie-sim-demo.svc.cluster.local/control/{node}"
      --set "agent.env.JOULIE_CONTROL_MODE=rapl"
    )
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
    --set "agent.env.KUBE_CLIENT_QPS=200" \
    --set "agent.env.KUBE_CLIENT_BURST=400" \
    --set "operator.env.RECONCILE_INTERVAL=${OPERATOR_RECONCILE_INTERVAL}" \
    --set "operator.env.KUBE_CLIENT_QPS=200" \
    --set "operator.env.KUBE_CLIENT_BURST=400" \
    --set "operator.env.POLICY_TYPE=${POLICY_TYPE}" \
    --set "operator.env.STATIC_HP_FRAC=${STATIC_HP_FRAC}" \
    --set "operator.env.QUEUE_HP_BASE_FRAC=${QUEUE_HP_BASE_FRAC}" \
    --set "operator.env.QUEUE_HP_MIN=${QUEUE_HP_MIN}" \
    --set "operator.env.QUEUE_HP_MAX=${QUEUE_HP_MAX}" \
    --set "operator.env.QUEUE_PERF_PER_HP_NODE=${QUEUE_PERF_PER_HP_NODE}" \
    --set "operator.env.PERFORMANCE_CAP_WATTS=${PERFORMANCE_CAP_WATTS}" \
    --set "operator.env.ECO_CAP_WATTS=${ECO_CAP_WATTS}" \
    --set "operator.env.CPU_PERFORMANCE_CAP_PCT_OF_MAX=${CPU_PERFORMANCE_CAP_PCT_OF_MAX}" \
    --set "operator.env.CPU_ECO_CAP_PCT_OF_MAX=${CPU_ECO_CAP_PCT_OF_MAX}" \
    --set "operator.env.CPU_WRITE_ABSOLUTE_CAPS=${CPU_WRITE_ABSOLUTE_CAPS}" \
    --set "operator.env.GPU_PERFORMANCE_CAP_PCT_OF_MAX=${GPU_PERFORMANCE_CAP_PCT_OF_MAX}" \
    --set "operator.env.GPU_ECO_CAP_PCT_OF_MAX=${GPU_ECO_CAP_PCT_OF_MAX}" \
    --set "operator.env.GPU_WRITE_ABSOLUTE_CAPS=${GPU_WRITE_ABSOLUTE_CAPS}" \
    --set "operator.env.ENABLE_FACILITY_METRICS=${ENABLE_FACILITY_METRICS}" \
    "${AGENT_TELEMETRY_ARGS[@]}" \
    "${SCHED_ARGS[@]}" \
    ${INFRA_SELECTOR_ARGS[@]+"${INFRA_SELECTOR_ARGS[@]}"}

  # Extender must be ready first — operator/agent pods go through the extender filter.
  kubectl -n joulie-system rollout status deploy/joulie-scheduler-extender --timeout=120s
  kubectl -n joulie-system rollout status deploy/joulie-operator
  kubectl -n joulie-system rollout status statefulset/joulie-agent-pool

  # Verify the extender is reachable. On KinD we can check from the control-plane
  # container (same network namespace as kube-scheduler with hostNetwork: true).
  # On real clusters we fall back to a kubectl port-forward probe.
  EXTENDER_URL="http://${EXTENDER_CLUSTER_IP}:9876/healthz"
  echo "verifying scheduler extender reachable..."
  CTRL_CONTAINER="${CLUSTER_NAME:-joulie-homogeneous-h100-benchmark}-control-plane"
  if docker inspect "$CTRL_CONTAINER" >/dev/null 2>&1; then
    if ! docker exec "$CTRL_CONTAINER" curl -sf --max-time 5 "$EXTENDER_URL" >/dev/null 2>&1; then
      echo "FATAL: scheduler extender not reachable from control-plane at $EXTENDER_URL" >&2
      echo "DNS or networking is misconfigured — kube-scheduler cannot reach the extender." >&2
      exit 1
    fi
  else
    kubectl -n joulie-system port-forward svc/joulie-scheduler-extender 19876:9876 &
    PF_PID=$!
    sleep 2
    if ! curl -sf --max-time 5 "http://localhost:19876/healthz" >/dev/null 2>&1; then
      kill "$PF_PID" 2>/dev/null || true
      echo "FATAL: scheduler extender not reachable via port-forward" >&2
      exit 1
    fi
    kill "$PF_PID" 2>/dev/null || true
  fi
  echo "scheduler extender deployed and reachable for baseline ${BASELINE}"
fi

# ---------------------------------------------------------------------------
# 3. Simulator (deployed after the extender so pods can be scheduled)
# ---------------------------------------------------------------------------
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

SIM_HELM_ARGS=(
  --set "image.repository=${SIM_REGISTRY}/${SIM_IMAGE}"
  --set "image.tag=${SIM_TAG:-latest}"
  --set "image.pullPolicy=Always"
  --set "env.SIM_FACILITY_AMBIENT_BASE_C=${SIM_FACILITY_AMBIENT_BASE_C}"
  --set "env.SIM_FACILITY_AMBIENT_AMPLITUDE_C=${SIM_FACILITY_AMBIENT_AMPLITUDE_C}"
  --set "env.SIM_FACILITY_AMBIENT_PERIOD_SEC=${SIM_FACILITY_AMBIENT_PERIOD_SEC}"
)
if [[ -n "$SIM_BASE_SPEED_PER_CORE" ]]; then
  SIM_HELM_ARGS+=(--set "extraEnv.SIM_BASE_SPEED_PER_CORE=${SIM_BASE_SPEED_PER_CORE}")
fi
# Delete any pre-helm simulator deployment to avoid server-side apply merge conflicts
# (old kubectl-managed volumes vs new helm-managed volumes).
kubectl -n joulie-sim-demo delete deploy/joulie-telemetry-sim --ignore-not-found 2>/dev/null || true
helm upgrade --install joulie-simulator "$ROOT/charts/joulie-simulator" \
  -n joulie-sim-demo --create-namespace \
  "${SIM_HELM_ARGS[@]}"
kubectl -n joulie-sim-demo rollout status deploy/joulie-telemetry-sim --timeout=600s
echo "simulator deployed via helm (image: ${SIM_REGISTRY}/${SIM_IMAGE}:${SIM_TAG:-latest})"

# ---------------------------------------------------------------------------
# 4. Agent telemetry env vars are now passed via Helm (above) to avoid a
#    second rolling restart of the agent StatefulSet.
# ---------------------------------------------------------------------------

echo "components installed for baseline ${BASELINE}"
