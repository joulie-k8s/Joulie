#!/usr/bin/env bash
#
# lib.sh - functions shared by the simulated cluster scripts. It is sourced,
# never run; every script starts with
#
#   source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
#   sim_init
#
# The simulated cluster is one k3s server in a Docker container named
# $SIMCLUSTER_NAME. Its own kubelet is the infra Node <name>-infra, which runs
# Prometheus, the simulator, the Joulie controller manager and scheduler
# extender, and one emulated Joulie agent per eligible fake Node. The fake
# Nodes come from a cluster spec and KWOK keeps them Ready. Cluster HTTP goes
# through the API server's service proxy; the only published port is the API
# server's, on 127.0.0.1.
#
# Everything the scripts write lives under $SIMCLUSTER_DIR, outside the
# repository (~/.kube is never read or written):
#
#   kubeconfig     the cluster's kubeconfig
#   cluster.env    how the container was created (up.sh)
#   telemetry.env  the telemetry mode and timing the backend was set up for
#   spec.yaml      the cluster spec the cluster holds (up.sh)
#   images.env     the images built and imported, with their image IDs
#   deploy.env     what deploy.sh installed
#   crds.sha       sha256 of the CRDs last applied
#   values/        generated Helm values files
#   render/        generated renders and configurations
#   bin/           binaries built for the images; bin/host holds the host tools
#   images/        image tarballs k3s imports when it starts (kept by down.sh)
#   runs/          smoke and scenario run directories, runs/latest the newest
#                  (kept by down.sh)
#
# The host tools are built from the working tree on first use: clusterspec
# (validate, render-nodes) and simcluster (validate, agents, telemetry,
# scale-spec); scenario.sh also builds simscenario and workloadgen there.
# simcluster prints each result on stdout and diagnostics on stderr; the
# scripts read:
#
#   agents       a Joulie values file setting agent.emulated.nodes, one entry
#                per eligible Node with exactly one profile key; the excluded
#                Nodes and their reasons go to stderr
#   telemetry    --part prom-config and otelcol-config: the configuration
#                file; sim-values and joulie-values: a Helm values file;
#                wait: one JSON object {"query", "label", "nodes"}, the query
#                that returns one series per Node that has reported, the label
#                holding the Node name, and the Nodes that must report
#   scale-spec   a cluster spec
#
# shellcheck disable=SC2034 # the variables below are read by the scripts that source this file
set -euo pipefail

SIM_REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
SIM_SCRIPTS=$SIM_REPO/scripts/simcluster
SIM_MANIFESTS=$SIM_SCRIPTS/manifests

# Names the scripts and the end-to-end test share.
SIM_NS_JOULIE=joulie-system
SIM_NS_SIM=joulie-sim
SIM_NS_MONITORING=monitoring
SIM_RELEASE_JOULIE=joulie
SIM_RELEASE_SIM=joulie-sim
SIM_PROM_SERVICE=prometheus-operated:9090
SIM_SIM_SERVICE=joulie-telemetry-sim:80
SIM_SIM_DEPLOYMENT=joulie-telemetry-sim
SIM_FIELD_MANAGER=joulie-simcluster
# The hwemu trees, on the infra Node (inside the k3s container), in a named
# volume so they outlive the container's restarts.
SIM_HWEMU_ROOT=/var/lib/joulie-hwemu
# The images are built for this architecture only.
SIM_IMAGE_ARCH=amd64
SIM_IMAGE_REGISTRY=joulie.local
# A Prometheus ConfigMap key and an otelcol one, mounted by the manifests.
SIM_PROM_CONFIG_KEY=prometheus.yml
SIM_OTELCOL_CONFIG_KEY=config.yaml

# sim_usage <script> prints the script's header comment, its usage.
sim_usage() {
  awk 'NR > 2 && /^#/ { sub(/^# ?/, ""); print; next } NR > 2 { exit }' "$1"
}

sim_log() {
  printf '[simcluster %s] %s\n' "$(date -u +%H:%M:%S)" "$*" >&2
}

sim_die() {
  sim_log "error: $*"
  exit 1
}

# sim_init checks the settings, reads versions.env and the timing file, and
# points kubectl and helm at the cluster's kubeconfig.
sim_init() {
  SIMCLUSTER_NAME=${SIMCLUSTER_NAME:-joulie-sim}
  # Every container and volume the scripts create or remove carries this
  # name, so the guard keeps them away from anything else on the host.
  if [[ ! $SIMCLUSTER_NAME =~ ^joulie-sim(-[a-z0-9]+)*$ ]]; then
    sim_die "SIMCLUSTER_NAME=$SIMCLUSTER_NAME does not match ^joulie-sim(-[a-z0-9]+)*\$; the scripts only touch containers and volumes named that way"
  fi
  SIMCLUSTER_DIR=${SIMCLUSTER_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/joulie/simcluster/$SIMCLUSTER_NAME}
  # The host tools run from the repository root, so every path is absolute.
  [[ $SIMCLUSTER_DIR == /* ]] || SIMCLUSTER_DIR=$PWD/$SIMCLUSTER_DIR
  SIMCLUSTER_TELEMETRY=${SIMCLUSTER_TELEMETRY:-otel}
  SIMCLUSTER_POWER_SCOPE=${SIMCLUSTER_POWER_SCOPE:-components}
  SIMCLUSTER_TIMING=${SIMCLUSTER_TIMING:-dev}
  case $SIMCLUSTER_TELEMETRY in
    otel | otel-unpromoted | otel-labels-unpromoted | otel-utf8 | otel-collector | kepler | kepler-legacy) ;;
    *) sim_die "SIMCLUSTER_TELEMETRY=$SIMCLUSTER_TELEMETRY is not a telemetry mode (otel, otel-unpromoted, otel-labels-unpromoted, otel-utf8, otel-collector, kepler, kepler-legacy)" ;;
  esac
  case $SIMCLUSTER_POWER_SCOPE in
    components | host) ;;
    *) sim_die "SIMCLUSTER_POWER_SCOPE=$SIMCLUSTER_POWER_SCOPE is not components or host" ;;
  esac
  local timing=$SIM_SCRIPTS/timing/$SIMCLUSTER_TIMING.env
  if [[ ! $SIMCLUSTER_TIMING =~ ^[a-z0-9-]+$ || ! -f $timing ]]; then
    sim_die "SIMCLUSTER_TIMING=$SIMCLUSTER_TIMING names no file scripts/simcluster/timing/<name>.env"
  fi
  export SIMCLUSTER_NAME SIMCLUSTER_DIR SIMCLUSTER_TELEMETRY SIMCLUSTER_POWER_SCOPE SIMCLUSTER_TIMING

  set -a
  # shellcheck source=versions.env
  source "$SIM_SCRIPTS/versions.env"
  set +a
  # The harness names of the timing (CONTROLLER_RECONCILE_INTERVAL, ...);
  # simcluster maps them onto the real settings.
  # shellcheck source=timing/dev.env
  source "$timing"

  SIM_PROFILES_CLI=()
  SIM_PROFILES_SPEC=()
  if [[ -n ${SIMCLUSTER_PROFILES_DIR:-} ]]; then
    [[ -d $SIMCLUSTER_PROFILES_DIR ]] || sim_die "SIMCLUSTER_PROFILES_DIR=$SIMCLUSTER_PROFILES_DIR is not a directory"
    SIMCLUSTER_PROFILES_DIR=$(cd "$SIMCLUSTER_PROFILES_DIR" && pwd)
    export SIMCLUSTER_PROFILES_DIR
    SIM_PROFILES_CLI=(--profiles-dir "$SIMCLUSTER_PROFILES_DIR")
    SIM_PROFILES_SPEC=(-profiles "$SIMCLUSTER_PROFILES_DIR")
  fi
  SIM_TELEMETRY_FLAGS=(--mode "$SIMCLUSTER_TELEMETRY" --scope "$SIMCLUSTER_POWER_SCOPE" --timing "$SIMCLUSTER_TIMING")

  # /usr/bin/go may be another release than the toolchain go.mod asks for;
  # the user's own Go goes first. GOTOOLCHAIN pins the go.mod toolchain, so
  # the images are built by the release CI builds with.
  export PATH="$HOME/.local/go/bin:$PATH"
  GO=${GO:-$(command -v go || true)}
  GOTOOLCHAIN=$(sim_go_toolchain)
  export GO GOTOOLCHAIN

  export KUBECONFIG=$SIMCLUSTER_DIR/kubeconfig
  # kubectl caches API discovery under ~/.kube/cache unless told otherwise.
  export KUBECACHEDIR=$SIMCLUSTER_DIR/kube-cache
  SIM_HOST_BIN=$SIMCLUSTER_DIR/bin/host
}

# sim_go_toolchain prints the go.mod toolchain line's release, or go<go line>
# when there is no toolchain line.
sim_go_toolchain() {
  local tc
  tc=$(awk '$1 == "toolchain" { print $2; exit }' "$SIM_REPO/go.mod")
  if [[ -z $tc ]]; then
    tc=go$(awk '$1 == "go" { print $2; exit }' "$SIM_REPO/go.mod")
  fi
  printf '%s\n' "$tc"
}

# sim_require <command>... fails naming the first command that is missing.
sim_require() {
  local c
  for c in "$@"; do
    command -v "$c" >/dev/null 2>&1 || sim_die "$c is required and not on PATH"
  done
}

# sim_wait <secs> <desc> <cmd...> runs cmd every 2 s until it succeeds. On
# timeout it runs cmd once more with its output shown and fails.
sim_wait() {
  local secs=$1 desc=$2
  shift 2
  local deadline=$((SECONDS + secs))
  until "$@" >/dev/null 2>&1; do
    if ((SECONDS >= deadline)); then
      sim_log "timed out after ${secs}s waiting for $desc; last attempt:"
      "$@" >&2 || true
      return 1
    fi
    sleep 2
  done
}

# sim_seconds <duration> prints a Go duration (500ms, 10s, 5m, 1h) in whole
# seconds, rounded up.
sim_seconds() {
  local d=$1
  if [[ $d =~ ^([0-9]+)ms$ ]]; then
    printf '%d\n' $(((BASH_REMATCH[1] + 999) / 1000))
  elif [[ $d =~ ^([0-9]+)s$ ]]; then
    printf '%d\n' "${BASH_REMATCH[1]}"
  elif [[ $d =~ ^([0-9]+)m$ ]]; then
    printf '%d\n' $((BASH_REMATCH[1] * 60))
  elif [[ $d =~ ^([0-9]+)h$ ]]; then
    printf '%d\n' $((BASH_REMATCH[1] * 3600))
  else
    sim_die "duration $d is not a whole number of ms, s, m or h"
  fi
}

# sim_path <file> prints file as an absolute path: as given when absolute,
# else relative to the working directory, else to the repository.
sim_path() {
  local f=$1
  if [[ $f == /* ]]; then
    :
  elif [[ -e $PWD/$f ]]; then
    f=$PWD/$f
  else
    f=$SIM_REPO/$f
  fi
  [[ -e $f ]] || sim_die "$1: no such file"
  printf '%s\n' "$f"
}

# sim_container_state prints true or false for the cluster's container, or
# nothing when there is none.
sim_container_state() {
  docker inspect -f '{{.State.Running}}' "$SIMCLUSTER_NAME" 2>/dev/null || true
}

sim_require_container() {
  [[ $(sim_container_state) == true ]] ||
    sim_die "the cluster $SIMCLUSTER_NAME is not running; start it with make simcluster-up"
}

sim_require_cluster() {
  sim_require_container
  [[ -s $KUBECONFIG ]] || sim_die "$KUBECONFIG is missing; run make simcluster-up"
}

# sim_host_tools builds the host tools once per run of the scripts.
sim_host_tools() {
  [[ ${SIM_HOST_TOOLS_BUILT:-} == "$SIM_HOST_BIN" ]] && return 0
  [[ -n $GO ]] || sim_die "no Go toolchain found: install Go or set GO"
  mkdir -p "$SIM_HOST_BIN"
  (cd "$SIM_REPO" && "$GO" build -trimpath -o "$SIM_HOST_BIN/" ./simulator/cmd/simcluster ./simulator/cmd/clusterspec) ||
    sim_die "building the host tools (simcluster, clusterspec) failed"
  export SIM_HOST_TOOLS_BUILT=$SIM_HOST_BIN
}

# sim_cli runs the simcluster command from the repository root, where it
# finds scripts/simcluster/timing.
sim_cli() {
  sim_host_tools
  (cd "$SIM_REPO" && "$SIM_HOST_BIN/simcluster" "$@")
}

sim_clusterspec() {
  sim_host_tools
  (cd "$SIM_REPO" && "$SIM_HOST_BIN/clusterspec" "$@")
}

# sim_prom <query> runs an instant PromQL query through the API server's
# service proxy and prints Prometheus' JSON answer.
sim_prom() {
  local q
  q=$(jq -rn --arg q "$1" '$q|@uri')
  kubectl get --raw "/api/v1/namespaces/$SIM_NS_MONITORING/services/$SIM_PROM_SERVICE/proxy/api/v1/query?query=$q"
}

# sim_sim <path> GETs a path of the simulator through the service proxy.
sim_sim() {
  kubectl get --raw "/api/v1/namespaces/$SIM_NS_SIM/services/$SIM_SIM_SERVICE/proxy/${1#/}"
}

# sim_normalize_ref prints an image reference the way containerd names it.
sim_normalize_ref() {
  local ref=$1 first=${1%%/*}
  if [[ $ref != */* ]]; then
    printf 'docker.io/library/%s\n' "$ref"
  elif [[ $first == *.* || $first == *:* || $first == localhost ]]; then
    printf '%s\n' "$ref"
  else
    printf 'docker.io/%s\n' "$ref"
  fi
}

# sim_import_image <ref> copies a local Docker image into the cluster's
# containerd, unless containerd lists it already.
sim_import_image() {
  local ref=$1 listed
  listed=$(docker exec "$SIMCLUSTER_NAME" ctr -n k8s.io images ls -q "name==$(sim_normalize_ref "$ref")")
  if [[ -n $listed ]]; then
    sim_log "$ref: already in the cluster"
    return 0
  fi
  sim_log "$ref: importing"
  local save=(docker save)
  # With Docker's containerd image store, a plain save of a multi-platform
  # image wants every platform's content, which a pull did not fetch.
  if docker save --help 2>/dev/null | grep -q -- '--platform'; then
    save+=(--platform "linux/$SIM_IMAGE_ARCH")
  fi
  "${save[@]}" "$ref" | docker exec -i "$SIMCLUSTER_NAME" ctr -n k8s.io images import --platform "linux/$SIM_IMAGE_ARCH" - >/dev/null
}

# sim_image_ids <ref> prints, comma separated, the image ID and the repo
# digests the kubelet may report as a container's imageID.
sim_image_ids() {
  sim_wait 60 "the kubelet's image service to list $1" docker exec "$SIMCLUSTER_NAME" crictl inspecti -o json "$1"
  docker exec "$SIMCLUSTER_NAME" crictl inspecti -o json "$1" |
    jq -r '[.status.id] + (.status.repoDigests // []) | map(select(. != null and . != "")) | join(",")'
}

# sim_load_images reads images.env and checks it names every Joulie image.
sim_load_images() {
  local f=$SIMCLUSTER_DIR/images.env
  [[ -s $f ]] || sim_die "$f is missing; run scripts/simcluster/images.sh"
  set -a
  # shellcheck disable=SC1090
  source "$f"
  set +a
  local k
  for k in IMAGE_AGENT IMAGE_CONTROLLER_MANAGER IMAGE_SCHEDULER IMAGE_SIMULATOR; do
    [[ -n ${!k:-} ]] || sim_die "$f does not set $k; run scripts/simcluster/images.sh"
  done
}

# sim_tag <ref> prints the tag of an image reference.
sim_tag() {
  printf '%s\n' "${1##*:}"
}

# sim_crds_sha prints the sha256 of the chart's CRDs, the files deploy.sh
# applies.
sim_crds_sha() {
  cat "$SIM_REPO"/charts/joulie/crds/*.yaml | sha256sum | cut -d' ' -f1
}

# sim_apply_crds applies the chart's CRDs server side and records their
# sha256. Helm installs a chart's crds/ only once and never upgrades them.
sim_apply_crds() {
  kubectl apply --server-side --field-manager="$SIM_FIELD_MANAGER" --force-conflicts -f "$SIM_REPO/charts/joulie/crds/" >/dev/null
  sim_crds_sha >"$SIMCLUSTER_DIR/crds.sha"
}

# sim_helm_sim <values file>... installs or upgrades the simulator with the
# cluster's spec and the simulator image of images.env, then the values files
# in SIMCLUSTER_SIM_EXTRA_VALUES (space separated), which scenario.sh sets
# for a scenario's simulator.set. deploy.sh and reload.sh both run this
# command, so a reload renders like a deploy.
sim_helm_sim() {
  local args=() f
  for f in "$@"; do args+=(-f "$f"); done
  for f in ${SIMCLUSTER_SIM_EXTRA_VALUES:-}; do args+=(-f "$(sim_path "$f")"); done
  helm upgrade --install "$SIM_RELEASE_SIM" "$SIM_REPO/charts/joulie-simulator" -n "$SIM_NS_SIM" "${args[@]}" \
    --set-file emulate.clusterSpec="$SIMCLUSTER_DIR/spec.yaml" --set image.tag="$(sim_tag "$IMAGE_SIMULATOR")" >/dev/null
}

# sim_helm_joulie <values file>... installs or upgrades Joulie with the images
# of images.env; sim_apply_crds applies the CRDs. Run by deploy.sh and
# reload.sh.
sim_helm_joulie() {
  local args=() f
  for f in "$@"; do args+=(-f "$f"); done
  helm upgrade --install "$SIM_RELEASE_JOULIE" "$SIM_REPO/charts/joulie" -n "$SIM_NS_JOULIE" "${args[@]}" --skip-crds \
    --set agent.image.tag="$(sim_tag "$IMAGE_AGENT")" \
    --set controllerManager.image.tag="$(sim_tag "$IMAGE_CONTROLLER_MANAGER")" \
    --set schedulerExtender.image.tag="$(sim_tag "$IMAGE_SCHEDULER")" >/dev/null
}

# sim_pod_tier <agents> sets SIM_MAX_PODS, SIM_NODE_CIDR_MASK, SIM_CLUSTER_CIDR
# and SIM_POD_ARGS for an infra Node that runs <agents> emulated agents next
# to the infrastructure. One kubelet holds 110 pods by default; a /24 Node
# CIDR holds 254 pod IPs, a /22 one 1022, and k3s' default cluster CIDR
# (/16) has room for only 64 /22 Node CIDRs.
sim_pod_tier() {
  local n=$(($1 + 50))
  ((n < 110)) && n=110
  SIM_POD_ARGS=()
  if ((n <= 110)); then
    SIM_MAX_PODS=110 SIM_NODE_CIDR_MASK=24 SIM_CLUSTER_CIDR=10.42.0.0/16
  elif ((n <= 250)); then
    SIM_MAX_PODS=$n SIM_NODE_CIDR_MASK=24 SIM_CLUSTER_CIDR=10.42.0.0/16
    SIM_POD_ARGS=(--kubelet-arg=max-pods="$n" --kube-controller-manager-arg=node-cidr-mask-size=24)
  elif ((n <= 1000)); then
    SIM_MAX_PODS=$n SIM_NODE_CIDR_MASK=22 SIM_CLUSTER_CIDR=10.64.0.0/12
    SIM_POD_ARGS=(--kubelet-arg=max-pods="$n" --kube-controller-manager-arg=node-cidr-mask-size=22 --cluster-cidr=10.64.0.0/12)
  else
    sim_die "$1 emulated agents need $n pods on the infra Node, more than one k3s Node holds (1000); split across k3s agents"
  fi
}

# sim_check_pod_capacity <agents> fails when the running cluster was created
# for fewer pods than <agents> emulated agents need.
sim_check_pod_capacity() {
  local f=$SIMCLUSTER_DIR/cluster.env
  [[ -s $f ]] || sim_die "$f is missing; recreate with simcluster-down, then simcluster-up"
  sim_pod_tier "$1"
  local have
  have=$(sed -n 's/^MAX_PODS=//p' "$f")
  if ((${have:-0} < SIM_MAX_PODS)); then
    sim_die "$1 emulated agents need max-pods $SIM_MAX_PODS, the cluster was created with ${have:-?}; recreate with simcluster-down, then simcluster-up"
  fi
}

# sim_agents <out> <values file>... renders the agent DaemonSet the way a real
# install with these values files would, and writes the emulated agents the
# simcluster command derives from it to <out>. The Nodes left out and why
# go to render/exclusions.txt and stderr.
sim_agents() {
  local out=$1
  shift
  local render=$SIMCLUSTER_DIR/render excl=$SIMCLUSTER_DIR/render/exclusions.txt
  mkdir -p "$render"
  local args=() f
  for f in "$@"; do args+=(-f "$f"); done
  helm template "$SIM_RELEASE_JOULIE" "$SIM_REPO/charts/joulie" -n "$SIM_NS_JOULIE" "${args[@]}" \
    --set agent.mode=daemonset --show-only templates/agent-daemonset.yaml >"$render/agent-daemonset.yaml"
  if ! sim_cli agents --spec "$SIMCLUSTER_DIR/spec.yaml" --daemonset "$render/agent-daemonset.yaml" \
    --image-arch "$SIM_IMAGE_ARCH" --root "$SIM_HWEMU_ROOT" "${SIM_PROFILES_CLI[@]}" >"$out.tmp" 2>"$excl"; then
    cat "$excl" >&2
    sim_die "simcluster agents failed"
  fi
  mv "$out.tmp" "$out"
  if [[ -s $excl ]]; then
    sim_log "spec Nodes the agent DaemonSet would not reach (no emulated agent):"
    sed 's/^/  /' "$excl" >&2
  fi
}

# sim_count_agents <agents values file> prints how many emulated agents it
# lists: each entry has exactly one profile key.
sim_count_agents() {
  grep -cE '^[[:space:]-]*profile:' "$1" || true
}

# sim_backend_current succeeds when Prometheus (and the Collector) were set
# up for the current telemetry mode and timing.
sim_backend_current() {
  local f=$SIMCLUSTER_DIR/telemetry.env
  [[ -s $f ]] && grep -qx "TELEMETRY=$SIMCLUSTER_TELEMETRY" "$f" && grep -qx "TIMING=$SIMCLUSTER_TIMING" "$f"
}

# sim_apply_config <manifest> <configmap> <key> <file> applies a backend
# manifest, then its ConfigMap from the generated file, and restarts the
# manifest's Deployment when the configuration changed: Prometheus and the
# Collector read their configuration only at start.
sim_apply_config() {
  local manifest=$1 cm=$2 key=$3 file=$4 before objs dep
  before=$(kubectl -n "$SIM_NS_MONITORING" get configmap "$cm" -o json 2>/dev/null | jq -r --arg k "$key" '.data[$k] // empty') || before=
  objs=$(kubectl apply --server-side --field-manager="$SIM_FIELD_MANAGER" --force-conflicts -f "$manifest" -o name)
  kubectl -n "$SIM_NS_MONITORING" create configmap "$cm" --from-file="$key=$file" --dry-run=client -o yaml |
    kubectl apply --server-side --field-manager="$SIM_FIELD_MANAGER" --force-conflicts -f - >/dev/null
  dep=$(grep -m1 '^deployment' <<<"$objs" || true)
  [[ -n $dep ]] || sim_die "$manifest holds no Deployment"
  if [[ $before != "$(cat "$file")" ]]; then
    kubectl -n "$SIM_NS_MONITORING" rollout restart "$dep" >/dev/null
  fi
  kubectl -n "$SIM_NS_MONITORING" rollout status "$dep" --timeout=300s >/dev/null
}

# sim_apply_backend sets up Prometheus, and the Collector in mode
# otel-collector, for the current telemetry mode and timing. A Collector left
# from an earlier mode stays: nothing sends to it or scrapes it.
sim_apply_backend() {
  local render=$SIMCLUSTER_DIR/render
  mkdir -p "$render"
  sim_cli telemetry "${SIM_TELEMETRY_FLAGS[@]}" --part prom-config >"$render/prometheus.yml"
  sim_apply_config "$SIM_MANIFESTS/prometheus.yaml" prometheus-config "$SIM_PROM_CONFIG_KEY" "$render/prometheus.yml"
  if [[ $SIMCLUSTER_TELEMETRY == otel-collector ]]; then
    sim_cli telemetry "${SIM_TELEMETRY_FLAGS[@]}" --part otelcol-config >"$render/otelcol.yaml"
    sim_apply_config "$SIM_MANIFESTS/otelcol.yaml" otelcol-config "$SIM_OTELCOL_CONFIG_KEY" "$render/otelcol.yaml"
  fi
  sim_wait 300 "Prometheus to be ready" kubectl get --raw "/api/v1/namespaces/$SIM_NS_MONITORING/services/$SIM_PROM_SERVICE/proxy/-/ready"
  printf 'TELEMETRY=%s\nTIMING=%s\n' "$SIMCLUSTER_TELEMETRY" "$SIMCLUSTER_TIMING" >"$SIMCLUSTER_DIR/telemetry.env"
}

# sim_agent_nodes prints the Node of every emulated agent Deployment, read
# from its sim.joulie.io/node annotation.
sim_agent_nodes() {
  kubectl -n "$SIM_NS_JOULIE" get deployments -o json |
    jq -r '.items[] | (.metadata.annotations["sim.joulie.io/node"] // .spec.template.metadata.annotations["sim.joulie.io/node"] // empty)' |
    sort
}

# sim_missing_nodehardware <node>... prints each Node that has no
# NodeHardware. Objects are matched on spec.nodeName, so no object name is
# derived here (pkg/api.ObjectNameForNode is the only place that does).
sim_missing_nodehardware() {
  local have
  have=$(kubectl get nodehardwares -o json | jq -r '.items[].spec.nodeName') || return 1
  local n
  for n in "$@"; do
    grep -qx "$n" <<<"$have" || printf '%s\n' "$n"
  done
}

# sim_missing_series <wait json> prints each Node the wait query does not
# return yet.
sim_missing_series() {
  local w=$1 resp
  resp=$(sim_prom "$(jq -r .query <<<"$w")") || return 1
  jq -r --arg l "$(jq -r .label <<<"$w")" --argjson want "$(jq -c '.nodes // []' <<<"$w")" \
    '$want - [.data.result[].metric[$l]] | .[]' <<<"$resp"
}

# sim_missing_twins prints each managed Node whose NodeTwin (matched on
# spec.nodeName) has no status.lastUpdated yet.
sim_missing_twins() {
  local nodes twins
  nodes=$(kubectl get nodes -l joulie.io/managed=true -o json | jq -r '.items[].metadata.name') || return 1
  twins=$(kubectl get nodetwins -o json | jq -r '.items[] | select((.status.lastUpdated // "") != "") | .spec.nodeName') || return 1
  local n
  for n in $nodes; do
    grep -qx "$n" <<<"$twins" || printf '%s\n' "$n"
  done
}

# sim_none <cmd...> succeeds when cmd succeeds and prints nothing: the
# sim_missing_* functions under sim_wait. Otherwise it prints what cmd
# printed, so a timed out wait names what is still missing.
sim_none() {
  local out
  out=$("$@") || return 1
  [[ -z $out ]] && return 0
  printf '%s\n' "$out"
  return 1
}

# sim_verify_images prints every running container in the Joulie and
# simulator namespaces whose image is not a Joulie image of images.env, or
# whose imageID is not the one images.env recorded for it (DR-21), and fails
# when there is one.
sim_verify_images() {
  sim_load_images
  local expected pods bad
  expected=$(jq -n \
    --arg a "$IMAGE_AGENT" --arg ai "${IMAGE_IDS_AGENT:-}" \
    --arg c "$IMAGE_CONTROLLER_MANAGER" --arg ci "${IMAGE_IDS_CONTROLLER_MANAGER:-}" \
    --arg s "$IMAGE_SCHEDULER" --arg si "${IMAGE_IDS_SCHEDULER:-}" \
    --arg m "$IMAGE_SIMULATOR" --arg mi "${IMAGE_IDS_SIMULATOR:-}" \
    '{($a): ($ai | split(",")), ($c): ($ci | split(",")), ($s): ($si | split(",")), ($m): ($mi | split(","))}')
  pods=$({
    kubectl -n "$SIM_NS_JOULIE" get pods -o json
    kubectl -n "$SIM_NS_SIM" get pods -o json
  } | jq -s '[.[].items[]]')
  bad=$(jq -r --argjson exp "$expected" '
    .[] | select(.status.phase == "Running" and .metadata.deletionTimestamp == null) | . as $p
    | ($p.spec.initContainers // []) + $p.spec.containers | .[] | . as $c
    | ([$p.status.initContainerStatuses[]?, $p.status.containerStatuses[]? | select(.name == $c.name) | .imageID][0] // "") as $id
    | "\($p.metadata.namespace)/\($p.metadata.name) \($c.name): " as $where
    | if $exp[$c.image] == null then "\($where)image \($c.image) is not a Joulie image of images.env"
      elif ([$exp[$c.image][] | select(. == $id)] | length) == 0 then "\($where)imageID \($id) is not \($exp[$c.image] | join(" or "))"
      else empty end' <<<"$pods")
  if [[ -n $bad ]]; then
    sim_log "containers not running the images images.sh imported:"
    local lines
    mapfile -t lines <<<"$bad"
    printf '  %s\n' "${lines[@]}" >&2
    return 1
  fi
}
