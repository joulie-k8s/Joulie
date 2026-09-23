#!/usr/bin/env bash
#
# kwok-scale-run.sh - measure the Joulie controller manager on a cluster of
# fake KWOK nodes.
#
# Two claims about Joulie at scale are otherwise unmeasured:
#
#   1. reconcileWithCatalog plans every managed node in one pass under a 20s
#      context (cmd/controller-manager/main.go: reconcileTimeout) with the
#      client rate limit from the chart (KUBE_CLIENT_QPS/BURST). There is a
#      node count beyond which a pass cannot finish; this run counts the passes
#      that did not.
#   2. The informer cache holds a transformed copy of every Pod in the cluster
#      (cmd/controller-manager/reader.go: podTransform). Memory therefore grows
#      with the cluster; this run measures what it actually costs.
#
# Nothing here belongs in per-PR CI: it creates a kind cluster, builds images
# and takes minutes. It is driven by .github/workflows/scale-nightly.yml.
#
set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

# ---------------------------------------------------------------------------
# Defaults
# ---------------------------------------------------------------------------
NODES=200
PODS=""                 # empty means 5 per node
CLUSTER="joulie-scale"
IMAGE_TAG="scale"
IMAGE_PREFIX="joulie-scale"
RECONCILE_INTERVAL="20s"
TWIN_TIMEOUT=900        # seconds to wait for every twin to carry a status
OBSERVE_SECONDS=90      # steady-state window after the twins are written
OUT_DIR="$PWD/scale-run-out"
KWOK_VERSION=""         # empty means resolve the latest release
KEEP=false
SKIP_BUILD=false

# Thresholds, derived from a measured run and not from a guess. On a 28 core,
# 160 GiB box, --nodes 200 --pods 1000 reached all twins in 16.74s with a peak
# RSS of 25.1 MiB and zero deadline overruns (kind v0.31.0, KWOK v0.8.0,
# RECONCILE_INTERVAL=20s, the chart's KUBE_CLIENT_QPS=50 / BURST=100).
#
# The time budget is 3.6x the measured 16.74s. Most of that time is the client
# rate limit, not the CPU: one pass writes a spec and a status per node, so 200
# nodes is over 400 writes at 50 QPS. A CI runner with a quarter of the cores
# is slower at decoding the 1000 pod informer sync but not at waiting on a
# token bucket, so 60s absorbs a slow runner without hiding a regression.
#
# The memory budget is 5x the measured 25.1 MiB. What it guards is the pod
# cache: podTransform drops containers, volumes and container statuses, which
# dominate a Pod. Losing that transform, or caching a new heavy kind, shows up
# here long before 128 MiB, while ordinary Go heap and GC timing noise does
# not.
MAX_SECONDS=60
MAX_RSS_MB=128

usage() {
  cat <<'USAGE'
Usage: scripts/kwok-scale-run.sh [options]

Creates a kind cluster, installs KWOK, registers fake nodes, installs the
Joulie chart from the working tree with the agent disabled, and measures how
the controller manager copes.

What it measures
  - seconds from controller manager start to a NodeTwin with a written status
    for every managed node (the pod cache is already populated at that point),
  - the controller manager's peak RSS, from `kubectl top pod` when metrics are
    available, otherwise from the container cgroup (memory.current on cgroup
    v2, memory.usage_in_bytes on cgroup v1) read through crictl on the kind
    node; the summary names the source it used,
  - the number of reconcile passes that exceeded their 20s context deadline,
    counted from "reconcile failed: ... context deadline exceeded" in the log.

Options
  --nodes N              fake KWOK nodes to register (default 200)
  --pods M               pods to spread over the fake nodes (default 5 per
                         node); 0 creates none
  --max-seconds S        fail if the twins take longer than S seconds
                         (default 60)
  --max-rss-mb MB        fail if the peak RSS exceeds MB mebibytes
                         (default 128)
  --twin-timeout S       give up waiting for the twins after S seconds
                         (default 900)
  --observe-seconds S    keep sampling for S seconds after the twins are
                         written, to catch deadline overruns in steady state
                         (default 90)
  --reconcile-interval D RECONCILE_INTERVAL for the controller manager
                         (default 20s)
  --cluster NAME         kind cluster name (default joulie-scale)
  --kwok-version VER     KWOK release to install (default: latest release)
  --image-tag TAG        tag for the images built from the working tree
                         (default scale)
  --skip-build           reuse images already loaded in the cluster
  --out-dir DIR          where to write summary.json and
                         controller-manager.log (default ./scale-run-out)
  --keep                 leave the cluster up for inspection; without it the
                         cluster is always deleted, including on failure
  -h, --help             this text

Output
  Progress goes to stderr. stdout carries a human readable table and, as its
  last line, one JSON object with the measurements. The same JSON is written
  to <out-dir>/summary.json and the controller manager log to
  <out-dir>/controller-manager.log.

Exit status
  0 when every threshold held, 1 otherwise (the summary is still printed).

Examples
  scripts/kwok-scale-run.sh --nodes 200 --pods 1000
  scripts/kwok-scale-run.sh --nodes 50 --pods 0 --keep
USAGE
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --nodes) NODES=$2; shift 2 ;;
    --pods) PODS=$2; shift 2 ;;
    --max-seconds) MAX_SECONDS=$2; shift 2 ;;
    --max-rss-mb) MAX_RSS_MB=$2; shift 2 ;;
    --twin-timeout) TWIN_TIMEOUT=$2; shift 2 ;;
    --observe-seconds) OBSERVE_SECONDS=$2; shift 2 ;;
    --reconcile-interval) RECONCILE_INTERVAL=$2; shift 2 ;;
    --cluster) CLUSTER=$2; shift 2 ;;
    --kwok-version) KWOK_VERSION=$2; shift 2 ;;
    --image-tag) IMAGE_TAG=$2; shift 2 ;;
    --skip-build) SKIP_BUILD=true; shift ;;
    --out-dir) OUT_DIR=$2; shift 2 ;;
    --keep) KEEP=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[[ -n "$PODS" ]] || PODS=$(( NODES * 5 ))

log() { printf '[scale] %s\n' "$*" >&2; }
die() { printf '[scale] FATAL: %s\n' "$*" >&2; exit 1; }

for tool in kind kubectl helm docker python3 jq; do
  command -v "$tool" >/dev/null 2>&1 || die "missing required tool: $tool"
done
[[ "$NODES" -ge 1 ]] || die "--nodes must be at least 1"
[[ "$PODS" -ge 0 ]] || die "--pods must not be negative"

WORK_DIR=$(mktemp -d)
# A per-run kubeconfig: kind never touches ~/.kube/config and a parallel run on
# another cluster cannot interfere.
export KUBECONFIG="$WORK_DIR/kubeconfig.yaml"
mkdir -p "$OUT_DIR"
SUMMARY_FILE="$OUT_DIR/summary.json"
CM_LOG_FILE="$OUT_DIR/controller-manager.log"

NAMESPACE=joulie-system
LOAD_NS=joulie-scale-load
NODE_PREFIX="kwok-scale"
# The generated fake nodes carry joulie.io/managed=true, which is what
# charts/joulie/values.yaml sets as the controller manager's NODE_SELECTOR.
# kube-scheduler runs with hostNetwork and cannot resolve .cluster.local on
# kind, so the extender gets a fixed ClusterIP, exactly as
# experiments/01-cpu-only-benchmark/scripts/01_create_cluster_kwokctl.sh does.
EXTENDER_CLUSTER_IP=${EXTENDER_CLUSTER_IP:-10.96.100.76}

START_EPOCH=$(date +%s)
FAILURES=()

cleanup() {
  local rc=$?
  if [[ "$KEEP" == true ]]; then
    log "--keep: leaving cluster kind-${CLUSTER} up (KUBECONFIG=$KUBECONFIG)"
  else
    log "deleting cluster kind-${CLUSTER}"
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
    rm -rf "$WORK_DIR"
  fi
  exit "$rc"
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# 1. Cluster and KWOK
# ---------------------------------------------------------------------------
KIND_CONFIG="$REPO_ROOT/examples/06-simulator-kwok/manifests/01-kind-cluster.yaml"
[[ -f "$KIND_CONFIG" ]] || die "missing kind config: $KIND_CONFIG"

if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  log "deleting pre-existing cluster kind-${CLUSTER}"
  kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
fi
log "creating cluster kind-${CLUSTER}"
kind create cluster --name "$CLUSTER" --config "$KIND_CONFIG" --kubeconfig "$KUBECONFIG" >&2

if [[ -z "$KWOK_VERSION" ]]; then
  KWOK_VERSION=$(curl -sS --max-time 30 https://api.github.com/repos/kubernetes-sigs/kwok/releases/latest \
    | python3 -c 'import sys, json; print(json.load(sys.stdin)["tag_name"])')
fi
log "installing KWOK ${KWOK_VERSION}"
kubectl apply -f "https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VERSION}/kwok.yaml" >&2
kubectl apply -f "https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VERSION}/stage-fast.yaml" >&2
kubectl -n kube-system rollout status deploy/kwok-controller --timeout=300s >&2

# ---------------------------------------------------------------------------
# 2. Register the Joulie scheduler extender with kube-scheduler
# ---------------------------------------------------------------------------
CTRL_CONTAINER="${CLUSTER}-control-plane"
cat >"$WORK_DIR/joulie-scheduler-config.yaml" <<SCHED_CFG
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
clientConnection:
  kubeconfig: /etc/kubernetes/scheduler.conf
profiles:
- schedulerName: default-scheduler
extenders:
- urlPrefix: "http://${EXTENDER_CLUSTER_IP}:9876"
  filterVerb: "filter"
  prioritizeVerb: "prioritize"
  weight: 5
  enableHTTPS: false
  nodeCacheCapable: false
  ignorable: true
SCHED_CFG
docker exec -i "$CTRL_CONTAINER" sh -c 'cat > /etc/kubernetes/joulie-scheduler-config.yaml' \
  < "$WORK_DIR/joulie-scheduler-config.yaml"
docker cp "$CTRL_CONTAINER":/etc/kubernetes/manifests/kube-scheduler.yaml "$WORK_DIR/kube-scheduler.yaml"
python3 - "$WORK_DIR/kube-scheduler.yaml" <<'PY'
import sys, yaml

path = sys.argv[1]
with open(path) as f:
    manifest = yaml.safe_load(f)
container = manifest["spec"]["containers"][0]
if not any("joulie-scheduler-config" in arg for arg in container["command"]):
    container["command"].append("--config=/etc/kubernetes/joulie-scheduler-config.yaml")
    container.setdefault("volumeMounts", []).append({
        "mountPath": "/etc/kubernetes/joulie-scheduler-config.yaml",
        "name": "joulie-scheduler-config",
        "readOnly": True,
    })
    manifest["spec"].setdefault("volumes", []).append({
        "hostPath": {"path": "/etc/kubernetes/joulie-scheduler-config.yaml", "type": "File"},
        "name": "joulie-scheduler-config",
    })
    with open(path, "w") as f:
        yaml.dump(manifest, f, default_flow_style=False)
PY
docker exec -i "$CTRL_CONTAINER" sh -c 'cat > /etc/kubernetes/manifests/kube-scheduler.yaml' \
  < "$WORK_DIR/kube-scheduler.yaml"
log "waiting for kube-scheduler to restart with the extender config"
kubectl wait --for=condition=Ready pod -l component=kube-scheduler -n kube-system --timeout=180s >&2

# ---------------------------------------------------------------------------
# 3. Images from the working tree
# ---------------------------------------------------------------------------
CM_IMAGE="${IMAGE_PREFIX}/joulie-controller-manager:${IMAGE_TAG}"
SCHED_IMAGE="${IMAGE_PREFIX}/joulie-scheduler:${IMAGE_TAG}"
if [[ "$SKIP_BUILD" == false ]]; then
  log "building controller manager and scheduler images from the working tree"
  docker build --build-arg COMPONENT=controller-manager -t "$CM_IMAGE" -f "$REPO_ROOT/Dockerfile" "$REPO_ROOT" >&2
  docker build --build-arg COMPONENT=scheduler -t "$SCHED_IMAGE" -f "$REPO_ROOT/Dockerfile" "$REPO_ROOT" >&2
fi
log "loading images into kind-${CLUSTER}"
kind load docker-image --name "$CLUSTER" "$CM_IMAGE" "$SCHED_IMAGE" >&2

# ---------------------------------------------------------------------------
# 4. Fake nodes
# ---------------------------------------------------------------------------
# One homogeneous CPU-only class, generated by the repository's own node
# generator so the labels stay in step with the experiments.
cat >"$WORK_DIR/cluster-nodes.yaml" <<NODES_CFG
nodes:
  - node_name_prefix: ${NODE_PREFIX}
    replicas: ${NODES}
    vendor: none
    product: ""
    cpu: AMD EPYC 9655 96-Core Processor
    cpu_sockets: 2
    cpu_cores: 192
    memory_gib: 1536
    gpu_count: 0
NODES_CFG
python3 "$REPO_ROOT/scripts/generate_heterogeneous_assets.py" \
  --input "$WORK_DIR/cluster-nodes.yaml" \
  --out-nodes "$WORK_DIR/00-kwok-nodes.yaml" \
  --out-classes "$WORK_DIR/10-node-classes.yaml" \
  --out-catalog "$WORK_DIR/hardware.generated.yaml" >&2
log "registering ${NODES} fake nodes"
kubectl apply -f "$WORK_DIR/00-kwok-nodes.yaml" >/dev/null
log "waiting for the fake nodes to go Ready"
for _ in $(seq 1 120); do
  ready=$(kubectl get nodes -l type=kwok --no-headers 2>/dev/null | awk '$2=="Ready"' | wc -l | tr -d ' ')
  [[ "$ready" -ge "$NODES" ]] && break
  sleep 2
done
[[ "$ready" -ge "$NODES" ]] || die "only ${ready}/${NODES} fake nodes went Ready"

# ---------------------------------------------------------------------------
# 5. Install the chart from the working tree, agent disabled
# ---------------------------------------------------------------------------
# agent.mode is neither "daemonset" nor "pool", so neither agent workload is
# rendered: fake nodes have no agent to run. NODE_POWER_SOURCE=static keeps the
# twin loop off any external telemetry.
log "installing the chart from ${REPO_ROOT}/charts/joulie"
helm upgrade --install joulie "$REPO_ROOT/charts/joulie" \
  -n "$NAMESPACE" --create-namespace \
  --set "agent.mode=none" \
  --set "controllerManager.image.repository=${IMAGE_PREFIX}/joulie-controller-manager" \
  --set "controllerManager.image.tag=${IMAGE_TAG}" \
  --set "controllerManager.image.pullPolicy=IfNotPresent" \
  --set "controllerManager.env.RECONCILE_INTERVAL=${RECONCILE_INTERVAL}" \
  --set "controllerManager.env.NODE_POWER_SOURCE=static" \
  --set-string "controllerManager.nodeSelector.joulie\\.io/infra=true" \
  --set "schedulerExtender.enabled=true" \
  --set "schedulerExtender.clusterIP=${EXTENDER_CLUSTER_IP}" \
  --set "schedulerExtender.image.repository=${IMAGE_PREFIX}/joulie-scheduler" \
  --set "schedulerExtender.image.tag=${IMAGE_TAG}" \
  --set "schedulerExtender.nodeName=${CLUSTER}-worker" >&2
kubectl -n "$NAMESPACE" rollout status deploy/joulie-scheduler-extender --timeout=300s >&2

# Park the control loop while the pods are created, so the measured window
# starts with the informer cache already holding every pod.
log "parking the controller manager while the load pods are created"
kubectl -n "$NAMESPACE" scale deploy/joulie-controller-manager --replicas=0 >&2
kubectl -n "$NAMESPACE" wait --for=delete pod -l app.kubernetes.io/name=joulie-controller-manager --timeout=120s >/dev/null 2>&1 || true
kubectl delete nodetwins --all --ignore-not-found >/dev/null 2>&1 || true

# ---------------------------------------------------------------------------
# 6. Load pods on the fake nodes
# ---------------------------------------------------------------------------
PODS_RUNNING=0
if [[ "$PODS" -gt 0 ]]; then
  PODS_PER_NODE=$(( (PODS + NODES - 1) / NODES ))
  # Exact-fit CPU requests are what spreads the pods: 95% of a node's 192 CPUs
  # divided by the wanted pods per node, so one more pod does not fit.
  POD_CPU_MILLI=$(( 192000 * 95 / 100 / PODS_PER_NODE ))
  [[ "$POD_CPU_MILLI" -ge 1 ]] || POD_CPU_MILLI=1
  log "creating ${PODS} load pods (${PODS_PER_NODE} per node, ${POD_CPU_MILLI}m each)"
  kubectl create namespace "$LOAD_NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  cat <<LOAD_YAML | kubectl apply -f - >/dev/null
apiVersion: apps/v1
kind: Deployment
metadata:
  name: joulie-scale-load
  namespace: ${LOAD_NS}
spec:
  replicas: ${PODS}
  selector:
    matchLabels:
      app.kubernetes.io/name: joulie-scale-load
  template:
    metadata:
      labels:
        app.kubernetes.io/name: joulie-scale-load
    spec:
      nodeSelector:
        type: kwok
      tolerations:
        - key: kwok.x-k8s.io/node
          value: fake
          effect: NoSchedule
      containers:
        - name: fake
          image: registry.k8s.io/pause:3.10
          resources:
            requests:
              cpu: "${POD_CPU_MILLI}m"
              memory: 64Mi
LOAD_YAML
  log "waiting for the load pods to run"
  for _ in $(seq 1 300); do
    PODS_RUNNING=$(kubectl -n "$LOAD_NS" get pods --field-selector=status.phase=Running --no-headers 2>/dev/null | wc -l | tr -d ' ')
    [[ "$PODS_RUNNING" -ge "$PODS" ]] && break
    sleep 2
  done
  [[ "$PODS_RUNNING" -ge "$PODS" ]] || log "warning: only ${PODS_RUNNING}/${PODS} load pods are Running"
fi

# ---------------------------------------------------------------------------
# 7. RSS probe
# ---------------------------------------------------------------------------
RSS_SOURCE="none"
RSS_NODE=""
RSS_CID=""
RSS_CGROUP=""
PEAK_RSS=0
RSS_SAMPLES=0

cm_pod() {
  kubectl -n "$NAMESPACE" get pods -l app.kubernetes.io/name=joulie-controller-manager \
    --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true
}

# kubectl top prints "NAME CPU MEMORY" with the memory in Mi or Ki.
rss_from_top() {
  local pod=$1 raw
  raw=$(kubectl top pod -n "$NAMESPACE" "$pod" --no-headers 2>/dev/null | awk '{print $3}') || return 1
  case "$raw" in
    *Mi) echo $(( ${raw%Mi} * 1024 * 1024 )) ;;
    *Ki) echo $(( ${raw%Ki} * 1024 )) ;;
    *) return 1 ;;
  esac
}

# The image is distroless, so there is no shell to exec into. crictl on the
# kind node reads the same cgroup counters the kubelet does.
rss_from_crictl() {
  docker exec "$RSS_NODE" crictl stats --id "$RSS_CID" -o json 2>/dev/null \
    | jq -r '.stats[0].memory.workingSetBytes.value // empty' 2>/dev/null
}

rss_from_cgroup() {
  docker exec "$RSS_NODE" cat "$RSS_CGROUP" 2>/dev/null
}

# Picks the first probe that returns a number and remembers it.
detect_rss_source() {
  local pod=$1 val
  pod=${pod:-$(cm_pod)}
  [[ -n "$pod" ]] || return 1

  if val=$(rss_from_top "$pod") && [[ "$val" =~ ^[0-9]+$ ]]; then
    RSS_SOURCE="kubectl-top"
    return 0
  fi

  RSS_NODE=$(kubectl -n "$NAMESPACE" get pod "$pod" -o jsonpath='{.spec.nodeName}')
  RSS_CID=$(kubectl -n "$NAMESPACE" get pod "$pod" \
    -o jsonpath='{.status.containerStatuses[0].containerID}' | sed 's#.*://##')
  [[ -n "$RSS_NODE" && -n "$RSS_CID" ]] || return 1

  val=$(rss_from_crictl)
  if [[ "$val" =~ ^[0-9]+$ ]]; then
    RSS_SOURCE="crictl-stats"
    return 0
  fi

  RSS_CGROUP=$(docker exec "$RSS_NODE" sh -c \
    "find /sys/fs/cgroup -path '*${RSS_CID}*' \\( -name memory.current -o -name memory.usage_in_bytes \\) 2>/dev/null | head -1")
  [[ -n "$RSS_CGROUP" ]] || return 1
  val=$(rss_from_cgroup)
  [[ "$val" =~ ^[0-9]+$ ]] || return 1
  case "$RSS_CGROUP" in
    *memory.current) RSS_SOURCE="cgroup-v2-memory.current" ;;
    *) RSS_SOURCE="cgroup-v1-memory.usage_in_bytes" ;;
  esac
  return 0
}

sample_rss() {
  local val=""
  if [[ "$RSS_SOURCE" == "none" ]]; then
    detect_rss_source "" >/dev/null 2>&1 || return 0
  fi
  case "$RSS_SOURCE" in
    kubectl-top) val=$(rss_from_top "$(cm_pod)") || return 0 ;;
    crictl-stats) val=$(rss_from_crictl) ;;
    cgroup-*) val=$(rss_from_cgroup) ;;
    *) return 0 ;;
  esac
  [[ "$val" =~ ^[0-9]+$ ]] || return 0
  RSS_SAMPLES=$(( RSS_SAMPLES + 1 ))
  [[ "$val" -gt "$PEAK_RSS" ]] && PEAK_RSS=$val
  return 0
}

count_twins_with_status() {
  kubectl get nodetwins -o json 2>/dev/null \
    | jq --arg p "$NODE_PREFIX" \
      '[.items[] | select((.spec.nodeName // "") | startswith($p)) | select((.status.lastUpdated // "") != "")] | length' \
    2>/dev/null || echo 0
}

# ---------------------------------------------------------------------------
# 8. Measured window
# ---------------------------------------------------------------------------
log "starting the controller manager (measured window opens now)"
T0=$(date +%s.%N)
kubectl -n "$NAMESPACE" scale deploy/joulie-controller-manager --replicas=1 >&2

TWINS_WRITTEN=0
TWIN_DEADLINE=$(( $(date +%s) + TWIN_TIMEOUT ))
TWINS_OK=false
while :; do
  sample_rss
  TWINS_WRITTEN=$(count_twins_with_status)
  [[ "$TWINS_WRITTEN" =~ ^[0-9]+$ ]] || TWINS_WRITTEN=0
  if [[ "$TWINS_WRITTEN" -ge "$NODES" ]]; then
    TWINS_OK=true
    break
  fi
  if [[ $(date +%s) -ge $TWIN_DEADLINE ]]; then
    break
  fi
  sleep 2
done
T1=$(date +%s.%N)
SECONDS_TO_TWINS=$(python3 -c "print(round(float('$T1') - float('$T0'), 2))")

CM_POD=$(cm_pod)
if [[ "$TWINS_OK" == true ]]; then
  log "all ${NODES} twins carry a status after ${SECONDS_TO_TWINS}s"
else
  log "TIMEOUT: only ${TWINS_WRITTEN}/${NODES} twins carry a status after ${TWIN_TIMEOUT}s"
  log "controller manager logs (last 100 lines):"
  kubectl -n "$NAMESPACE" logs "${CM_POD:-deploy/joulie-controller-manager}" --tail=100 >&2 || true
  FAILURES+=("only ${TWINS_WRITTEN}/${NODES} NodeTwins carried a status within ${TWIN_TIMEOUT}s")
fi

if [[ "$OBSERVE_SECONDS" -gt 0 ]]; then
  log "observing steady state for ${OBSERVE_SECONDS}s"
  OBSERVE_END=$(( $(date +%s) + OBSERVE_SECONDS ))
  while [[ $(date +%s) -lt $OBSERVE_END ]]; do
    sample_rss
    sleep 5
  done
fi

# ---------------------------------------------------------------------------
# 9. Collect
# ---------------------------------------------------------------------------
kubectl -n "$NAMESPACE" logs "${CM_POD:-deploy/joulie-controller-manager}" >"$CM_LOG_FILE" 2>/dev/null || true
DEADLINE_EXCEEDED=$(grep -c 'reconcile failed:.*context deadline exceeded' "$CM_LOG_FILE" 2>/dev/null || true)
RECONCILE_FAILURES=$(grep -c 'reconcile failed:' "$CM_LOG_FILE" 2>/dev/null || true)
[[ "$DEADLINE_EXCEEDED" =~ ^[0-9]+$ ]] || DEADLINE_EXCEEDED=0
[[ "$RECONCILE_FAILURES" =~ ^[0-9]+$ ]] || RECONCILE_FAILURES=0
PEAK_RSS_MB=$(python3 -c "print(round($PEAK_RSS / 1048576.0, 1))")
WALL_CLOCK=$(( $(date +%s) - START_EPOCH ))

if [[ "$RSS_SOURCE" == "none" ]]; then
  FAILURES+=("could not read the controller manager RSS from any source")
fi
if python3 -c "import sys; sys.exit(0 if float('$SECONDS_TO_TWINS') > float('$MAX_SECONDS') else 1)"; then
  FAILURES+=("twins took ${SECONDS_TO_TWINS}s, over the --max-seconds ${MAX_SECONDS} budget")
fi
if python3 -c "import sys; sys.exit(0 if float('$PEAK_RSS_MB') > float('$MAX_RSS_MB') else 1)"; then
  FAILURES+=("peak RSS ${PEAK_RSS_MB} MiB, over the --max-rss-mb ${MAX_RSS_MB} budget")
fi
if [[ "$DEADLINE_EXCEEDED" -gt 0 ]]; then
  FAILURES+=("${DEADLINE_EXCEEDED} reconcile passes exceeded the 20s context deadline")
fi

STATUS=pass
[[ ${#FAILURES[@]} -eq 0 ]] || STATUS=fail

printf '\n'
printf '%-34s %s\n' "nodes (managed, fake)"            "$NODES"
printf '%-34s %s\n' "pods requested"                   "$PODS"
printf '%-34s %s\n' "pods running"                     "$PODS_RUNNING"
printf '%-34s %s\n' "twins with a written status"      "${TWINS_WRITTEN}/${NODES}"
printf '%-34s %s\n' "seconds to all twins"             "$SECONDS_TO_TWINS"
printf '%-34s %s\n' "  budget (--max-seconds)"         "$MAX_SECONDS"
printf '%-34s %s\n' "peak RSS (MiB)"                   "$PEAK_RSS_MB"
printf '%-34s %s\n' "  budget (--max-rss-mb)"          "$MAX_RSS_MB"
printf '%-34s %s\n' "  RSS source"                     "$RSS_SOURCE"
printf '%-34s %s\n' "  RSS samples"                    "$RSS_SAMPLES"
printf '%-34s %s\n' "reconcile passes over deadline"   "$DEADLINE_EXCEEDED"
printf '%-34s %s\n' "reconcile failures (any cause)"   "$RECONCILE_FAILURES"
printf '%-34s %s\n' "reconcile interval"               "$RECONCILE_INTERVAL"
printf '%-34s %s\n' "KWOK version"                     "$KWOK_VERSION"
printf '%-34s %s\n' "wall clock (s)"                   "$WALL_CLOCK"
printf '%-34s %s\n' "status"                           "$STATUS"
if [[ ${#FAILURES[@]} -gt 0 ]]; then
  printf '\n'
  for f in "${FAILURES[@]}"; do printf 'FAIL: %s\n' "$f"; done
fi
printf '\n'

jq -c -n \
  --argjson nodes "$NODES" \
  --argjson pods "$PODS" \
  --argjson pods_running "$PODS_RUNNING" \
  --argjson twins_written "$TWINS_WRITTEN" \
  --argjson seconds_to_all_twins "$SECONDS_TO_TWINS" \
  --argjson peak_rss_bytes "$PEAK_RSS" \
  --argjson peak_rss_mb "$PEAK_RSS_MB" \
  --arg rss_source "$RSS_SOURCE" \
  --argjson rss_samples "$RSS_SAMPLES" \
  --argjson reconcile_deadline_exceeded "$DEADLINE_EXCEEDED" \
  --argjson reconcile_failures_total "$RECONCILE_FAILURES" \
  --arg reconcile_interval "$RECONCILE_INTERVAL" \
  --argjson observe_seconds "$OBSERVE_SECONDS" \
  --argjson max_seconds "$MAX_SECONDS" \
  --argjson max_rss_mb "$MAX_RSS_MB" \
  --argjson wall_clock_seconds "$WALL_CLOCK" \
  --arg kwok_version "$KWOK_VERSION" \
  --arg status "$STATUS" \
  --argjson failures "$(printf '%s\n' ${FAILURES[@]+"${FAILURES[@]}"} | jq -R . | jq -s 'map(select(length > 0))')" \
  '$ARGS.named' | tee "$SUMMARY_FILE"

log "summary written to $SUMMARY_FILE"
log "controller manager log written to $CM_LOG_FILE"

[[ "$STATUS" == "pass" ]] || exit 1
exit 0
