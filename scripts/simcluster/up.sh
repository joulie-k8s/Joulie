#!/usr/bin/env bash
#
# up.sh - create the simulated cluster, or bring a running one up to date.
#
# Usage: scripts/simcluster/up.sh
#
# Runs one k3s server in Docker (container $SIMCLUSTER_NAME), installs KWOK
# and Prometheus (and the OpenTelemetry Collector in modes otel-collector and
# otel-bridge), and registers the fake Nodes of the cluster spec. Running it
# again reuses the container when it was created for at least as many pods
# and with the same scheduler configuration and k3s image; otherwise it fails
# and says to recreate. Nodes the spec no longer names are deleted.
#
# Settings (environment):
#   SIMCLUSTER_NAME          container name, ^joulie-sim(-[a-z0-9]+)*$ (joulie-sim)
#   SIMCLUSTER_DIR           state directory (~/.cache/joulie/simcluster/<name>)
#   SIMCLUSTER_SPEC          cluster spec (tests/simcluster/specs/smoke.yaml)
#   SIMCLUSTER_NODES         instead of a spec: N Nodes of one profile
#   SIMCLUSTER_SCALE_PROFILE that profile (intel-xeon-2s-rapl)
#   SIMCLUSTER_PROFILES_DIR  local hardware profiles, read by the host tools
#                            only; a spec that needs one is refused, since the
#                            in-cluster simulator reads its built-in profiles
#   SIMCLUSTER_TELEMETRY     telemetry mode (otel)
#   SIMCLUSTER_TIMING        scripts/simcluster/timing/<name>.env (dev)
#   SIMCLUSTER_EXTENDER      how kube-scheduler reaches the extender: hostnet,
#                            on 127.0.0.1, or service, through the extender
#                            Service's fixed ClusterIP (hostnet); the
#                            container is created with the scheduler
#                            configuration of one of them
#   SIMCLUSTER_AIRGAP=1      preload the k3s system images from the release bundle
#   SIMCLUSTER_EXTRA_VALUES  Joulie values files, space separated, also used
#                            to decide which Nodes get an emulated agent
#
set -euo pipefail
# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
case ${1:-} in
  "") ;;
  -h | --help)
    sim_usage "$0"
    exit 0
    ;;
  *) sim_die "unknown argument $1 (see --help); up.sh reads its settings from the environment" ;;
esac
sim_init
sim_require docker kubectl helm jq sha256sum

N=$SIMCLUSTER_NAME
D=$SIMCLUSTER_DIR
mkdir -p "$D/images" "$D/render" "$D/values"

# 1. The spec: a copy of SIMCLUSTER_SPEC, or a scale spec, validated.
if [[ -n ${SIMCLUSTER_NODES:-} ]]; then
  [[ $SIMCLUSTER_NODES =~ ^[1-9][0-9]*$ ]] || sim_die "SIMCLUSTER_NODES=$SIMCLUSTER_NODES is not a positive number"
  sim_cli scale-spec --nodes "$SIMCLUSTER_NODES" --profile "${SIMCLUSTER_SCALE_PROFILE:-intel-xeon-2s-rapl}" >"$D/spec.yaml.tmp"
else
  cp "$(sim_path "${SIMCLUSTER_SPEC:-tests/simcluster/specs/smoke.yaml}")" "$D/spec.yaml.tmp"
fi
mv "$D/spec.yaml.tmp" "$D/spec.yaml"
spec=$D/spec.yaml
# simcluster validate loads and expands the spec as clusterspec does, then
# checks its harness section.
sim_cli validate --spec "$spec" "${SIM_PROFILES[@]}" >&2
# The simulator in the cluster cannot read SIMCLUSTER_PROFILES_DIR: a spec
# that needs it would pass here and fail in deploy.sh, after five minutes of
# waiting for the simulator's /readyz.
if [[ -n ${SIMCLUSTER_PROFILES_DIR:-} ]] && ! sim_cli validate --spec "$spec" --profiles-dir "" >/dev/null 2>&1; then
  sim_die "the spec needs a profile of SIMCLUSTER_PROFILES_DIR; the in-cluster simulator reads only its built-in profiles, so local profiles serve the host tools only"
fi

# 2. Every emulated agent is a pod on the infra Node, next to the
# infrastructure: size its kubelet and pod network for them.
files=("$SIM_REPO/values/simcluster.yaml" "${SIM_EXTENDER_VALUES[@]}")
for f in ${SIMCLUSTER_EXTRA_VALUES:-}; do files+=("$(sim_path "$f")"); done
sim_agents "$D/render/agents-up.yaml" "${files[@]}"
agents=$(sim_count_agents "$D/render/agents-up.yaml")
sim_pod_tier "$agents"
sched_sha=$(sha256sum "$SIM_SCHEDULER_CONFIG" | cut -d' ' -f1)
sim_log "$agents emulated agents: max-pods $SIM_MAX_PODS, Node CIDR /$SIM_NODE_CIDR_MASK, cluster CIDR $SIM_CLUSTER_CIDR"

# 3. The k3s system images, from the release's air-gap bundle, which k3s
# imports from agent/images when it starts.
if [[ ${SIMCLUSTER_AIRGAP:-0} == 1 ]]; then
  sim_require curl
  bundle=$D/images/${K3S_AIRGAP_URL##*/}
  if [[ ! -f $bundle ]] || ! sha256sum "$bundle" | grep -q "^$K3S_AIRGAP_SHA256 "; then
    sim_log "downloading $K3S_AIRGAP_URL"
    curl -fsSL --retry 3 -o "$bundle.tmp" "$K3S_AIRGAP_URL"
    sha256sum "$bundle.tmp" | grep -q "^$K3S_AIRGAP_SHA256 " || {
      rm -f "$bundle.tmp"
      sim_die "$K3S_AIRGAP_URL: sha256 is not $K3S_AIRGAP_SHA256"
    }
    mv "$bundle.tmp" "$bundle"
  fi
fi

# 4. The container: reuse a running one when it covers what this spec needs.
cluster_env=$D/cluster.env
state=$(sim_container_state)
if [[ -n $state ]]; then
  [[ -s $cluster_env ]] || sim_die "container $N exists but $cluster_env does not; recreate with simcluster-down, then simcluster-up"
  have() { sed -n "s/^$1=//p" "$cluster_env"; }
  h_image=$(have K3S_IMAGE) h_pods=$(have MAX_PODS) h_mask=$(have NODE_CIDR_MASK) h_cidr=$(have CLUSTER_CIDR)
  h_cidr_bits=${h_cidr#*/}
  [[ $h_pods =~ ^[0-9]+$ ]] || h_pods=0
  [[ $h_mask =~ ^[0-9]+$ ]] || h_mask=99
  [[ $h_cidr_bits =~ ^[0-9]+$ ]] || h_cidr_bits=99
  # A container covers a spec when its kubelet holds at least as many pods,
  # its Node CIDR and cluster CIDR are at least as large, and it runs the same
  # k3s image with the same scheduler configuration.
  why=()
  [[ $h_image == "$K3S_IMAGE" ]] || why+=("k3s image ${h_image:-unknown}, now $K3S_IMAGE")
  ((h_pods >= SIM_MAX_PODS)) || why+=("max-pods $h_pods, now $SIM_MAX_PODS")
  ((h_mask <= SIM_NODE_CIDR_MASK)) || why+=("Node CIDR /$h_mask, now /$SIM_NODE_CIDR_MASK")
  ((h_cidr_bits <= ${SIM_CLUSTER_CIDR#*/})) || why+=("cluster CIDR ${h_cidr:-unknown}, now $SIM_CLUSTER_CIDR")
  [[ $(have SCHEDULER_CONFIG_SHA256) == "$sched_sha" ]] || why+=("the scheduler configuration is not $(basename "$SIM_SCHEDULER_CONFIG") (SIMCLUSTER_EXTENDER=$SIMCLUSTER_EXTENDER) as it is now")
  if ((${#why[@]})); then
    sim_die "container $N was created for other settings ($(IFS=';'; printf '%s' "${why[*]}")); recreate with simcluster-down, then simcluster-up"
  fi
  if [[ $state != true ]]; then
    sim_log "starting the stopped container $N"
    docker start "$N" >/dev/null
  fi
  sim_log "reusing container $N"
else
  args=()
  # k3s 1.35 and later refuse cgroup v1 unless told otherwise; tmpfs at
  # /sys/fs/cgroup is a cgroup v1 host.
  if [[ $(stat -fc %T /sys/fs/cgroup) == tmpfs && ${K3S_IMAGE##*:} =~ ^v([0-9]+)\.([0-9]+) ]] &&
    ((BASH_REMATCH[1] > 1 || BASH_REMATCH[2] >= 35)); then
    args+=(--kubelet-arg=fail-cgroupv1=false)
  fi
  sim_log "creating container $N ($K3S_IMAGE)"
  docker run -d --privileged --name "$N" --hostname "$N" \
    -v "$N-k3s:/var/lib/rancher/k3s" -v "$N-hwemu:$SIM_HWEMU_ROOT" -v "$D/images:/var/lib/rancher/k3s/agent/images" \
    -v "$SIM_SCHEDULER_CONFIG:/etc/rancher/k3s/scheduler-config.yaml:ro" \
    -p 127.0.0.1::6443 "$K3S_IMAGE" server --node-name "$N-infra" --node-label joulie.io/infra=true --node-label joulie.io/hwemu-host=true \
    --disable traefik --disable metrics-server --kube-scheduler-arg=--config=/etc/rancher/k3s/scheduler-config.yaml \
    "${SIM_POD_ARGS[@]}" "${args[@]}" >/dev/null
  {
    printf 'K3S_IMAGE=%s\n' "$K3S_IMAGE"
    printf 'MAX_PODS=%s\n' "$SIM_MAX_PODS"
    printf 'NODE_CIDR_MASK=%s\n' "$SIM_NODE_CIDR_MASK"
    printf 'CLUSTER_CIDR=%s\n' "$SIM_CLUSTER_CIDR"
    printf 'SCHEDULER_CONFIG_SHA256=%s\n' "$sched_sha"
    printf 'CREATED_AT=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  } >"$cluster_env"
fi

# 5. The third-party images (Prometheus, KWOK, the Collector of the mode),
# imported while the cluster starts; waited for before KWOK is applied.
bash "$SIM_SCRIPTS/images.sh" --third-party &
third_party=$!
# shellcheck disable=SC2317 # called by the EXIT trap
stop_imports() { kill "$third_party" 2>/dev/null || true; }
trap stop_imports EXIT

# 6. The kubeconfig, pointed at the published port.
sim_wait_running 180 "k3s to write its kubeconfig" docker exec "$N" test -s /etc/rancher/k3s/k3s.yaml
port=$(docker port "$N" 6443/tcp | head -n1)
port=${port##*:}
[[ $port =~ ^[0-9]+$ ]] || sim_die "container $N publishes no port for 6443"
(
  umask 077
  docker exec "$N" cat /etc/rancher/k3s/k3s.yaml | sed "s#https://127.0.0.1:6443#https://127.0.0.1:$port#" >"$KUBECONFIG.tmp"
)
mv "$KUBECONFIG.tmp" "$KUBECONFIG"
sim_wait_running 180 "the API server" kubectl get --raw /readyz
sim_wait_running 180 "Node $N-infra to register" kubectl get node "$N-infra"

# 7. Namespaces, KWOK and its Stages, Prometheus and the Collector.
for ns in "$SIM_NS_JOULIE" "$SIM_NS_SIM" "$SIM_NS_MONITORING"; do
  kubectl create namespace "$ns" --dry-run=client -o yaml |
    kubectl apply --server-side --field-manager="$SIM_FIELD_MANAGER" -f - >/dev/null
done
sim_wait_pids "importing the third-party images" "$third_party"
kwok=$SIM_MANIFESTS/kwok-$KWOK_VERSION
kubectl apply --server-side --field-manager="$SIM_FIELD_MANAGER" --force-conflicts -f "$kwok/kwok.yaml" >/dev/null
kubectl wait --for=condition=Established crd/stages.kwok.x-k8s.io --timeout=120s >/dev/null
kubectl apply --server-side --field-manager="$SIM_FIELD_MANAGER" --force-conflicts -f "$kwok/stage-fast.yaml" >/dev/null
# Overrides of the vendored Stages, after them and in lexical order, so a
# later file wins.
while IFS= read -r -d '' stage; do
  kubectl apply --server-side --field-manager="$SIM_FIELD_MANAGER" --force-conflicts -f "$stage" >/dev/null
done < <(find "$SIM_MANIFESTS" -maxdepth 1 -name 'stage-*.yaml' -print0 | LC_ALL=C sort -z)
sim_apply_backend

# 8. The driver stub, the file the emulated GPU tools find where a driver
# library would be (toolDelivery mounted).
docker exec "$N" sh -c 'mkdir -p "$1/lib" && { [ -f "$1/lib/driver-stub" ] || echo "joulie simcluster driver stub" >"$1/lib/driver-stub"; }' sh "$SIM_HWEMU_ROOT"

# 9. The fake Nodes. The status apply forces: KWOK owns the conditions.
# Both applies force because the Node lifecycle controller takes over the
# atomic taint list while a new Node is not yet Ready.
sim_clusterspec render-nodes -spec "$spec" "${SIM_PROFILES[@]}" >"$D/render/nodes.yaml"
kubectl apply --server-side --field-manager="$SIM_FIELD_MANAGER" --force-conflicts -f "$D/render/nodes.yaml" -o name >"$D/render/nodes.names"
kubectl apply --server-side --field-manager="$SIM_FIELD_MANAGER" --force-conflicts --subresource=status -f "$D/render/nodes.yaml" >/dev/null

# 10. Nodes of an earlier spec.
stale=$(kubectl get nodes -l sim.joulie.io/cluster -o json | jq -r '.items[].metadata.name | "node/" + .' | grep -vxF -f "$D/render/nodes.names" || true)
if [[ -n $stale ]]; then
  sim_log "deleting Nodes the spec no longer names: $(tr '\n' ' ' <<<"$stale")"
  # shellcheck disable=SC2086
  kubectl delete $stale >/dev/null
fi

# 11. Every Node Ready.
kubectl wait --for=condition=Ready nodes --all --timeout=300s >/dev/null
sim_log "cluster $N is up with $(wc -l <"$D/render/nodes.names") fake Nodes; export KUBECONFIG=$KUBECONFIG"
