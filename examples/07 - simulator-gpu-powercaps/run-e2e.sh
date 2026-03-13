#!/usr/bin/env bash
set -Eeuo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
EXAMPLE_DIR="$ROOT_DIR/examples/07 - simulator-gpu-powercaps"
KIND_CLUSTER_CONFIG="${KIND_CLUSTER_CONFIG:-$ROOT_DIR/examples/07 - simulator-gpu-powercaps/manifests/01-kind-cluster.yaml}"

CLUSTER_NAME="${CLUSTER_NAME:-joulie-gpu-e2e}"
KCTX="kind-${CLUSTER_NAME}"
KWOK_VERSION="${KWOK_VERSION:-}"
KIND_FALLBACK_NO_CONFIG="${KIND_FALLBACK_NO_CONFIG:-true}"
KIND_REUSE_EXISTING_ON_CREATE_FAILURE="${KIND_REUSE_EXISTING_ON_CREATE_FAILURE:-true}"
LOCAL_TAG="${LOCAL_TAG:-gpu-e2e-$(date +%Y%m%d%H%M%S)}"
ARTIFACT_DIR="${ARTIFACT_DIR:-$ROOT_DIR/tmp/gpu-e2e-${LOCAL_TAG}}"

AGENT_IMAGE_REPO="${AGENT_IMAGE_REPO:-joulie-agent}"
OPERATOR_IMAGE_REPO="${OPERATOR_IMAGE_REPO:-joulie-operator}"
SIMULATOR_IMAGE_REPO="${SIMULATOR_IMAGE_REPO:-joulie-simulator}"
AGENT_IMAGE="${AGENT_IMAGE_REPO}:${LOCAL_TAG}"
OPERATOR_IMAGE="${OPERATOR_IMAGE_REPO}:${LOCAL_TAG}"
SIMULATOR_IMAGE="${SIMULATOR_IMAGE_REPO}:${LOCAL_TAG}"

mkdir -p "$ARTIFACT_DIR"
LOG_FILE="$ARTIFACT_DIR/run.log"

log() {
  echo "[$(date -u +%Y-%m-%dT%H:%M:%SZ)] $*" | tee -a "$LOG_FILE"
}

run() {
  log "+ $*"
  "$@" 2>&1 | tee -a "$LOG_FILE"
}

k() {
  kubectl --context "$KCTX" "$@"
}

require_bin() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "Missing required binary: $1" >&2
    exit 1
  }
}

pick_reusable_kind_cluster() {
  local c ctx
  while IFS= read -r c; do
    [[ -n "$c" ]] || continue
    ctx="kind-${c}"
    if ! kubectl config get-contexts "$ctx" >/dev/null 2>&1; then
      continue
    fi
    if kubectl --context "$ctx" get nodes >/dev/null 2>&1; then
      echo "$c"
      return 0
    fi
  done < <(kind get clusters 2>/dev/null || true)
  return 1
}

collect_diagnostics() {
  local out="$ARTIFACT_DIR/diagnostics"
  mkdir -p "$out"

  if ! kubectl config get-contexts "$KCTX" >/dev/null 2>&1; then
    log "diagnostics: context $KCTX not found, skipping cluster dump"
    return
  fi

  log "diagnostics: collecting cluster state into $out"
  k cluster-info >"$out/cluster-info.txt" 2>&1 || true
  k get nodes -o wide --show-labels >"$out/nodes.txt" 2>&1 || true
  k describe nodes >"$out/nodes.describe.txt" 2>&1 || true
  k get ns >"$out/namespaces.txt" 2>&1 || true
  k get pods -A -o wide >"$out/pods-wide.txt" 2>&1 || true
  k describe pods -A >"$out/pods.describe.txt" 2>&1 || true
  k get events -A --sort-by=.lastTimestamp >"$out/events.txt" 2>&1 || true
  k get nodepowerprofiles -o yaml >"$out/nodepowerprofiles.yaml" 2>&1 || true
  k get telemetryprofiles -o yaml >"$out/telemetryprofiles.yaml" 2>&1 || true
  k -n joulie-system get all >"$out/joulie-system.all.txt" 2>&1 || true
  k -n joulie-sim-demo get all >"$out/joulie-sim-demo.all.txt" 2>&1 || true

  k -n joulie-system logs deploy/joulie-operator --tail=-1 >"$out/operator.log" 2>&1 || true
  k -n joulie-system logs statefulset/joulie-agent-pool --tail=-1 >"$out/agent-pool.log" 2>&1 || true
  k -n joulie-sim-demo logs deploy/joulie-telemetry-sim --tail=-1 >"$out/simulator.log" 2>&1 || true

  local pf_pid=""
  if k -n joulie-sim-demo get deploy/joulie-telemetry-sim >/dev/null 2>&1; then
    (k -n joulie-sim-demo port-forward deploy/joulie-telemetry-sim 18080:18080 >"$out/port-forward.log" 2>&1) &
    pf_pid="$!"
    sleep 2
    curl -fsS "http://127.0.0.1:18080/debug/nodes" >"$out/sim-debug-nodes.json" 2>"$out/sim-debug-nodes.err" || true
    curl -fsS "http://127.0.0.1:18080/debug/events" >"$out/sim-debug-events.json" 2>"$out/sim-debug-events.err" || true
    curl -fsS "http://127.0.0.1:18080/metrics" >"$out/sim-metrics.prom" 2>"$out/sim-metrics.err" || true
  fi
  if [[ -n "$pf_pid" ]]; then
    kill "$pf_pid" >/dev/null 2>&1 || true
    wait "$pf_pid" >/dev/null 2>&1 || true
  fi
}

on_exit() {
  local rc=$?
  collect_diagnostics
  if [[ $rc -eq 0 ]]; then
    log "SUCCESS: GPU e2e completed. Artifacts: $ARTIFACT_DIR"
  else
    log "FAILURE: GPU e2e failed with exit code $rc. Artifacts: $ARTIFACT_DIR"
  fi
}
trap on_exit EXIT

wait_for_jq_expr() {
  local timeout_sec="$1"
  local interval_sec="$2"
  local expr="$3"
  local desc="$4"
  local start now
  start="$(date +%s)"
  while true; do
    if eval "$expr"; then
      log "check passed: $desc"
      return 0
    fi
    now="$(date +%s)"
    if (( now - start >= timeout_sec )); then
      log "check timeout: $desc"
      return 1
    fi
    sleep "$interval_sec"
  done
}

main() {
  log "artifacts dir: $ARTIFACT_DIR"
  for b in kind kubectl helm docker curl jq go; do
    require_bin "$b"
  done

  if ! kind get clusters | grep -Fxq "$CLUSTER_NAME"; then
    log "creating kind cluster $CLUSTER_NAME"
    if ! run kind create cluster --name "$CLUSTER_NAME" --config "$KIND_CLUSTER_CONFIG"; then
      if [[ "$KIND_FALLBACK_NO_CONFIG" == "true" ]]; then
        log "kind create with config failed; retrying with default kind config"
        if ! run kind create cluster --name "$CLUSTER_NAME"; then
          if [[ "$KIND_REUSE_EXISTING_ON_CREATE_FAILURE" == "true" ]]; then
            local existing_cluster=""
            if existing_cluster="$(pick_reusable_kind_cluster)"; then
              CLUSTER_NAME="$existing_cluster"
              KCTX="kind-${CLUSTER_NAME}"
              log "cluster creation failed; reusing existing healthy cluster $CLUSTER_NAME"
            else
              log "cluster creation failed and no reusable existing kind cluster was found"
              return 1
            fi
          else
            log "cluster creation failed and KIND_REUSE_EXISTING_ON_CREATE_FAILURE=false"
            return 1
          fi
        fi
      else
        log "kind create with config failed and KIND_FALLBACK_NO_CONFIG=false"
        return 1
      fi
    fi
  else
    log "reusing existing kind cluster $CLUSTER_NAME"
  fi

  run kubectl --context "$KCTX" get nodes -o wide

  if [[ -z "$KWOK_VERSION" ]]; then
    KWOK_VERSION="$(curl -fsSL https://api.github.com/repos/kubernetes-sigs/kwok/releases/latest | jq -r .tag_name)"
  fi
  log "using KWOK release $KWOK_VERSION"
  run k apply -f "https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VERSION}/kwok.yaml"
  run k apply -f "https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VERSION}/stage-fast.yaml"
  run k -n kube-system rollout status deploy/kwok-controller --timeout=240s

  log "cleaning previous simulator/kwok state for deterministic run"
  run k delete telemetryprofiles --all --ignore-not-found=true
  run k delete nodepowerprofiles --all --ignore-not-found=true
  run k -n default delete pod -l app.kubernetes.io/part-of=joulie-sim-workload --ignore-not-found=true
  run k delete nodes -l type=kwok --ignore-not-found=true

  log "applying simulator GPU example manifests"
  run k apply -f "$EXAMPLE_DIR/manifests/00-kwok-nodes.yaml"
  run k apply -f "$EXAMPLE_DIR/manifests/10-node-classes.yaml"
  run k apply -f "$EXAMPLE_DIR/manifests/50-workload-trace-configmap.yaml"
  run k apply -f "$EXAMPLE_DIR/manifests/20-simulator.yaml"
  run k -n joulie-sim-demo rollout status deploy/joulie-telemetry-sim --timeout=240s
  wait_for_jq_expr 120 3 \
    "[[ \$(k get nodes -l type=kwok -o json | jq '.items | length') -eq 3 ]]" \
    "three kwok example nodes created"

  log "building local images"
  run docker build --build-arg COMPONENT=agent -t "$AGENT_IMAGE" -f "$ROOT_DIR/Dockerfile" "$ROOT_DIR"
  run docker build --build-arg COMPONENT=operator -t "$OPERATOR_IMAGE" -f "$ROOT_DIR/Dockerfile" "$ROOT_DIR"
  run docker build -t "$SIMULATOR_IMAGE" -f "$ROOT_DIR/simulator/Dockerfile" "$ROOT_DIR"

  log "loading images into kind"
  run kind load docker-image --name "$CLUSTER_NAME" "$AGENT_IMAGE" "$OPERATOR_IMAGE" "$SIMULATOR_IMAGE"
  run k -n joulie-sim-demo set image deploy/joulie-telemetry-sim simulator="$SIMULATOR_IMAGE"
  run k -n joulie-sim-demo patch deploy/joulie-telemetry-sim --type='json' \
    -p='[{"op":"replace","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"IfNotPresent"}]'
  run k -n joulie-sim-demo rollout status deploy/joulie-telemetry-sim --timeout=240s

  log "applying latest CRDs (helm does not upgrade CRDs on release upgrade)"
  run k apply -f "$ROOT_DIR/charts/joulie/crds/joulie.io_nodepowerprofiles.yaml"
  run k apply -f "$ROOT_DIR/charts/joulie/crds/joulie.io_telemetryprofiles.yaml"

  log "installing joulie chart in pool mode"
  run helm upgrade --install joulie "$ROOT_DIR/charts/joulie" \
    --kube-context "$KCTX" \
    -n joulie-system --create-namespace \
    -f "$EXAMPLE_DIR/manifests/30-joulie-values-pool.yaml" \
    --set "agent.image.repository=${AGENT_IMAGE_REPO}" \
    --set "agent.image.tag=${LOCAL_TAG}" \
    --set "agent.image.pullPolicy=IfNotPresent" \
    --set "operator.image.repository=${OPERATOR_IMAGE_REPO}" \
    --set "operator.image.tag=${LOCAL_TAG}" \
    --set "operator.image.pullPolicy=IfNotPresent"

  run k -n joulie-system rollout status deploy/joulie-operator --timeout=240s
  run k -n joulie-system rollout status statefulset/joulie-agent-pool --timeout=240s

  log "creating per-node telemetry profiles"
  mapfile -t managed_nodes < <(k get nodes -l joulie.io/managed=true -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')
  if [[ "${#managed_nodes[@]}" -eq 0 ]]; then
    log "no managed nodes found"
    return 1
  fi
  for n in "${managed_nodes[@]}"; do
    log "applying telemetry profile for node=$n"
    sed "s/TARGET_NODE/${n}/g" "$EXAMPLE_DIR/manifests/40-telemetryprofile-template.yaml" | k apply -f -
  done

  wait_for_jq_expr 300 5 \
    "[[ \$(k get nodepowerprofiles -o json | jq '.items | length') -ge 3 ]]" \
    "nodepowerprofiles created"

  wait_for_jq_expr 300 5 \
    "[[ \$(k get telemetryprofiles -o json | jq '.items | length') -ge 3 ]]" \
    "telemetryprofiles created"

  wait_for_jq_expr 240 5 \
    "[[ \$(k -n default get pods -l app.kubernetes.io/part-of=joulie-sim-workload -o json | jq '.items | length') -ge 3 ]]" \
    "simulator injected workload pods"

  wait_for_jq_expr 300 5 \
    "[[ \$(k get telemetryprofiles -o json | jq '[.items[] | select(.status.control.gpu.result == \"applied\")] | length') -ge 2 ]]" \
    "gpu control applied on gpu nodes"

  wait_for_jq_expr 180 5 \
    "[[ \$(k -n joulie-sim-demo logs deploy/joulie-telemetry-sim --tail=2000 | grep -c 'action=gpu.set_power_cap_watts') -ge 2 ]]" \
    "simulator observed gpu power-cap control actions"

  run k get nodes -L type,joulie.io/managed,joulie.io/power-profile,joulie.io/gpu.product
  run k get nodepowerprofiles -o yaml
  run k get telemetryprofiles -o yaml

  log "end-to-end validation complete"
}

main "$@"
