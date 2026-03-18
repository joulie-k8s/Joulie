#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
EXP_ROOT="$ROOT/experiments/02-heterogeneous-benchmark"
CFG=${1:-$EXP_ROOT/configs/benchmark.yaml}
CLUSTER_NAME=${CLUSTER_NAME:-joulie-heterogeneous-benchmark}
REUSE_EXISTING_CLUSTER=${REUSE_EXISTING_CLUSTER:-false}
KIND_CLUSTER_CONFIG=${KIND_CLUSTER_CONFIG:-$(python3 - <<'PY' "$CFG"
import pathlib, sys, yaml
cfg = yaml.safe_load(pathlib.Path(sys.argv[1]).read_text()) or {}
print(cfg.get("install", {}).get("kind_cluster_config", "examples/07 - simulator-gpu-powercaps/manifests/01-kind-cluster.yaml"))
PY
)}
KIND_CLUSTER_CONFIG="$ROOT/${KIND_CLUSTER_CONFIG}"

# Use a per-experiment kubeconfig so kind never touches ~/.kube/config and parallel
# experiments on separate clusters cannot interfere with each other.
KUBECONFIG="$EXP_ROOT/kubeconfig.yaml"
export KUBECONFIG

if kind get clusters | grep -qx "${CLUSTER_NAME}" && [[ "${REUSE_EXISTING_CLUSTER}" == "true" ]]; then
  # Regenerate per-experiment kubeconfig if it was lost (e.g. after a machine restart)
  [[ -f "$KUBECONFIG" ]] || kind export kubeconfig --name "$CLUSTER_NAME" --kubeconfig "$KUBECONFIG"
  echo "reusing existing cluster: kind-${CLUSTER_NAME} (kubeconfig=${KUBECONFIG})"
elif kind get clusters | grep -qx "${CLUSTER_NAME}"; then
  kind delete cluster --name "${CLUSTER_NAME}" --kubeconfig "$KUBECONFIG"
  [[ -f "$KIND_CLUSTER_CONFIG" ]] || { echo "missing kind cluster config: $KIND_CLUSTER_CONFIG" >&2; exit 1; }
  kind create cluster --name "$CLUSTER_NAME" --config "$KIND_CLUSTER_CONFIG" --kubeconfig "$KUBECONFIG"
else
  [[ -f "$KIND_CLUSTER_CONFIG" ]] || { echo "missing kind cluster config: $KIND_CLUSTER_CONFIG" >&2; exit 1; }
  kind create cluster --name "$CLUSTER_NAME" --config "$KIND_CLUSTER_CONFIG" --kubeconfig "$KUBECONFIG"
fi

KWOK_VER=${KWOK_VER:-$(curl -s https://api.github.com/repos/kubernetes-sigs/kwok/releases/latest | python3 -c 'import sys, json; print(json.load(sys.stdin)["tag_name"])')}
kubectl apply -f "https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VER}/kwok.yaml"
kubectl apply -f "https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VER}/stage-fast.yaml"

# ---------------------------------------------------------------------------
# Configure kube-scheduler to call the Joulie scheduler extender.
# kube-scheduler runs with hostNetwork: true, so it resolves DNS via the node's
# /etc/resolv.conf (not CoreDNS).  On KinD this means Docker's embedded DNS,
# which cannot resolve .cluster.local names.  Using a fixed ClusterIP bypasses
# DNS entirely — kube-proxy iptables rules route ClusterIP traffic correctly
# even from hostNetwork.  On real clusters where node DNS can resolve
# .cluster.local, set EXTENDER_CLUSTER_IP="" to use the DNS name instead.
# When using a fixed IP, the same value must be set via
# schedulerExtender.clusterIP in the Helm values.
# ---------------------------------------------------------------------------
EXTENDER_CLUSTER_IP=${EXTENDER_CLUSTER_IP:-10.96.100.76}
CTRL_CONTAINER="${CLUSTER_NAME}-control-plane"

if [[ -n "$EXTENDER_CLUSTER_IP" ]]; then
  EXTENDER_URL_PREFIX="http://${EXTENDER_CLUSTER_IP}:9876"
else
  EXTENDER_URL_PREFIX="http://joulie-scheduler-extender.joulie-system.svc.cluster.local:9876"
fi

cat >/tmp/joulie-scheduler-config.yaml <<SCHED_CFG
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
clientConnection:
  kubeconfig: /etc/kubernetes/scheduler.conf
profiles:
- schedulerName: default-scheduler
extenders:
- urlPrefix: "${EXTENDER_URL_PREFIX}"
  filterVerb: "filter"
  prioritizeVerb: "prioritize"
  weight: 5
  enableHTTPS: false
  nodeCacheCapable: false
  ignorable: true
SCHED_CFG
docker exec -i "$CTRL_CONTAINER" sh -c 'cat > /etc/kubernetes/joulie-scheduler-config.yaml' < /tmp/joulie-scheduler-config.yaml
rm -f /tmp/joulie-scheduler-config.yaml

docker cp "$CTRL_CONTAINER":/etc/kubernetes/manifests/kube-scheduler.yaml /tmp/kube-scheduler-patch.yaml
python3 <<'PY'
import yaml

with open("/tmp/kube-scheduler-patch.yaml") as f:
    manifest = yaml.safe_load(f)

container = manifest["spec"]["containers"][0]
cmd = container["command"]
if any("joulie-scheduler-config" in arg for arg in cmd):
    print("kube-scheduler: extender already configured, skipping")
    exit(0)

cmd.append("--config=/etc/kubernetes/joulie-scheduler-config.yaml")

container.setdefault("volumeMounts", []).append({
    "mountPath": "/etc/kubernetes/joulie-scheduler-config.yaml",
    "name": "joulie-scheduler-config",
    "readOnly": True,
})

manifest["spec"].setdefault("volumes", []).append({
    "hostPath": {
        "path": "/etc/kubernetes/joulie-scheduler-config.yaml",
        "type": "File",
    },
    "name": "joulie-scheduler-config",
})

with open("/tmp/kube-scheduler-patch.yaml", "w") as f:
    yaml.dump(manifest, f, default_flow_style=False)
print("kube-scheduler: extender configuration applied")
PY

docker exec -i "$CTRL_CONTAINER" sh -c 'cat > /etc/kubernetes/manifests/kube-scheduler.yaml' < /tmp/kube-scheduler-patch.yaml
rm -f /tmp/kube-scheduler-patch.yaml

echo "waiting for kube-scheduler to restart with extender config..."
sleep 5
kubectl wait --for=condition=Ready pod -l component=kube-scheduler -n kube-system --timeout=60s

echo "cluster ready: kind-${CLUSTER_NAME} (kubeconfig=${KUBECONFIG}) with KWOK ${KWOK_VER}"
