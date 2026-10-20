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
# (render-nodes) and simcluster (validate, agents, telemetry, scale-spec);
# scenario.sh also builds simscenario and workloadgen there. Both read the
# local hardware profiles through --profiles-dir, which the scripts always
# pass ($SIMCLUSTER_PROFILES_DIR, empty for the builtins only), so the
# tools' own default, SIM_PROFILES_DIR, never applies here.
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
# A Prometheus ConfigMap key and an otelcol one, mounted by the manifests;
# the otel-bridge Collector reads the example's configuration under its
# own key first.
SIM_PROM_CONFIG_KEY=prometheus.yml
SIM_OTELCOL_CONFIG_KEY=config.yaml
SIM_BRIDGE_EXAMPLE_KEY=collector.yaml
SIM_BRIDGE_EXAMPLE=$SIM_REPO/examples/11-otel-collector-bridge/collector.yaml

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
  SIMCLUSTER_EXTENDER=${SIMCLUSTER_EXTENDER:-hostnet}
  # The modes of simcluster.Modes, in its order.
  case $SIMCLUSTER_TELEMETRY in
    otel | otel-unpromoted | otel-labels-unpromoted | otel-utf8 | otel-collector | otel-bridge | kepler | kepler-legacy) ;;
    *) sim_die "SIMCLUSTER_TELEMETRY=$SIMCLUSTER_TELEMETRY is not a telemetry mode (otel, otel-unpromoted, otel-labels-unpromoted, otel-utf8, otel-collector, otel-bridge, kepler, kepler-legacy)" ;;
  esac
  case $SIMCLUSTER_POWER_SCOPE in
    components | host) ;;
    *) sim_die "SIMCLUSTER_POWER_SCOPE=$SIMCLUSTER_POWER_SCOPE is not components or host" ;;
  esac
  # How kube-scheduler reaches the extender: hostnet, on 127.0.0.1 with the
  # extender on the infra Node's network; service, through the fixed
  # ClusterIP of the extender's Service with the extender off the host
  # network, as a real install defaults to.
  case $SIMCLUSTER_EXTENDER in
    hostnet) SIM_SCHEDULER_CONFIG=$SIM_MANIFESTS/scheduler-config.yaml SIM_EXTENDER_VALUES=() ;;
    service) SIM_SCHEDULER_CONFIG=$SIM_MANIFESTS/scheduler-config-service.yaml SIM_EXTENDER_VALUES=("$SIM_REPO/values/simcluster-extender-service.yaml") ;;
    *) sim_die "SIMCLUSTER_EXTENDER=$SIMCLUSTER_EXTENDER is not hostnet or service" ;;
  esac
  local timing=$SIM_SCRIPTS/timing/$SIMCLUSTER_TIMING.env
  if [[ ! $SIMCLUSTER_TIMING =~ ^[a-z0-9-]+$ || ! -f $timing ]]; then
    sim_die "SIMCLUSTER_TIMING=$SIMCLUSTER_TIMING names no file scripts/simcluster/timing/<name>.env"
  fi
  export SIMCLUSTER_NAME SIMCLUSTER_DIR SIMCLUSTER_TELEMETRY SIMCLUSTER_POWER_SCOPE SIMCLUSTER_TIMING SIMCLUSTER_EXTENDER

  set -a
  # shellcheck source=versions.env
  source "$SIM_SCRIPTS/versions.env"
  set +a
  # The harness names of the timing (CONTROLLER_RECONCILE_INTERVAL, ...);
  # simcluster maps them onto the real settings.
  # shellcheck source=timing/dev.env
  source "$timing"

  if [[ -n ${SIMCLUSTER_PROFILES_DIR:-} ]]; then
    [[ -d $SIMCLUSTER_PROFILES_DIR ]] || sim_die "SIMCLUSTER_PROFILES_DIR=$SIMCLUSTER_PROFILES_DIR is not a directory"
    SIMCLUSTER_PROFILES_DIR=$(cd "$SIMCLUSTER_PROFILES_DIR" && pwd)
    export SIMCLUSTER_PROFILES_DIR
  fi
  # The profiles flag of both host tools.
  SIM_PROFILES=(--profiles-dir "${SIMCLUSTER_PROFILES_DIR:-}")
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

# sim_git_state prints the commit of the working tree as a run directory
# name ends with it (scenario.RunDirName): exactly 12 characters of HEAD
# (--short=12 may print more to stay unambiguous), then -dirty when the tree
# has changes; nogit outside a git checkout.
sim_git_state() {
  local sha
  sha=$(git -C "$SIM_REPO" rev-parse HEAD 2>/dev/null | cut -c1-12) || true
  if [[ -z $sha ]]; then
    printf 'nogit\n'
  elif [[ -n $(git -C "$SIM_REPO" status --porcelain 2>/dev/null) ]]; then
    printf '%s-dirty\n' "$sha"
  else
    printf '%s\n' "$sha"
  fi
}

# sim_image_tag_key <component> prints the Helm value that holds the image
# tag of a component of images.env: agent, controller-manager and scheduler
# in the Joulie chart, simulator in the simulator chart.
sim_image_tag_key() {
  case $1 in
    agent) echo agent.image.tag ;;
    controller-manager) echo controllerManager.image.tag ;;
    scheduler) echo schedulerExtender.image.tag ;;
    simulator) echo image.tag ;;
    *) sim_die "component $1 has no image tag" ;;
  esac
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

# sim_container_down prints why the cluster's container does not run, its
# exit code and the last lines of its log, and succeeds; it fails, printing
# nothing, while the container runs. k3s exits when its kubelet cannot get
# an inotify instance, which every k3s container on the host takes from
# root's fs.inotify.max_user_instances: that case is named.
sim_container_down() {
  local state
  state=$(sim_container_state)
  [[ $state == true ]] && return 1
  if [[ -z $state ]]; then
    sim_log "container $SIMCLUSTER_NAME does not exist"
    return 0
  fi
  local code tail
  code=$(docker inspect -f '{{.State.ExitCode}}' "$SIMCLUSTER_NAME" 2>/dev/null || true)
  tail=$(docker logs --tail 30 "$SIMCLUSTER_NAME" 2>&1 || true)
  sim_log "container $SIMCLUSTER_NAME exited with status ${code:-unknown}; the last lines of its log:"
  printf '  %s\n' "${tail//$'\n'/$'\n'  }" >&2
  if grep -q 'inotify.*too many open files\|too many open files.*inotify' <<<"$tail"; then
    sim_log "k3s ran out of inotify instances: every k3s container on this host shares fs.inotify.max_user_instances ($(cat /proc/sys/fs/inotify/max_user_instances 2>/dev/null || echo unknown)); raise it or run fewer clusters"
  fi
  return 0
}

# sim_wait_running <secs> <desc> <cmd...> is sim_wait for a step that needs
# the cluster's container: it fails at once, with the container's exit and
# log (sim_container_down), when the container stops running.
sim_wait_running() {
  local secs=$1 desc=$2
  shift 2
  local deadline=$((SECONDS + secs))
  until "$@" >/dev/null 2>&1; do
    if sim_container_down; then
      sim_log "stopped waiting for $desc: the container is not running"
      return 1
    fi
    if ((SECONDS >= deadline)); then
      sim_log "timed out after ${secs}s waiting for $desc; last attempt:"
      "$@" >&2 || true
      return 1
    fi
    sleep 2
  done
}

# sim_save_container_logs <dir> writes the container's log (k3s.log) and
# containerd's (containerd.log) into dir, from a running or a stopped
# container; it never fails.
sim_save_container_logs() {
  local dir=$1
  [[ -n $(sim_container_state) ]] || return 0
  mkdir -p "$dir"
  docker logs "$SIMCLUSTER_NAME" >"$dir/k3s.log" 2>&1 || true
  docker cp "$SIMCLUSTER_NAME:/var/lib/rancher/k3s/agent/containerd/containerd.log" "$dir/containerd.log" >/dev/null 2>&1 || true
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

# sim_cluster_images prints every image reference the cluster's containerd
# lists, one per line: the one listing a run takes before it imports, which
# sim_import_image then reads from SIM_CLUSTER_IMAGES.
sim_cluster_images() {
  docker exec "$SIMCLUSTER_NAME" ctr -n k8s.io images ls -q
}

# sim_save_args sets SIM_SAVE_ARGS to the docker save command, with
# --platform where docker save has it: with Docker's containerd image store,
# a plain save of a multi-platform image wants every platform's content,
# which a pull did not fetch. Called once, before imports run in parallel.
sim_save_args() {
  SIM_SAVE_ARGS=(docker save)
  if docker save --help 2>/dev/null | grep -q -- '--platform'; then
    SIM_SAVE_ARGS+=(--platform "linux/$SIM_IMAGE_ARCH")
  fi
}

# The tries of one image import, and the seconds between them.
SIM_IMPORT_TRIES=3
SIM_IMPORT_RETRY_DELAY=5

# sim_import_image <ref> copies a local Docker image into the cluster's
# containerd, unless containerd lists it already: in SIM_CLUSTER_IMAGES when
# the caller listed them (sim_cluster_images), else by asking for it. One
# import runs at a time per cluster, under $SIMCLUSTER_DIR/import.lock, so
# the parallel pipelines of images.sh, and two images.sh at once, never
# stream into containerd together; the builds, pulls and saves still run in
# parallel. A failed import is tried again, SIM_IMPORT_TRIES times in all,
# while the container runs; once it stops, the import fails at once with
# the container's exit and log (sim_container_down).
sim_import_image() {
  local ref=$1 norm listed
  norm=$(sim_normalize_ref "$ref")
  if [[ -n ${SIM_CLUSTER_IMAGES+set} ]]; then
    grep -qxF "$norm" <<<"$SIM_CLUSTER_IMAGES" && listed=$norm
  else
    listed=$(docker exec "$SIMCLUSTER_NAME" ctr -n k8s.io images ls -q "name==$norm")
  fi
  if [[ -n ${listed:-} ]]; then
    sim_log "$ref: already in the cluster"
    return 0
  fi
  [[ -n ${SIM_SAVE_ARGS+set} ]] || sim_save_args
  mkdir -p "$SIMCLUSTER_DIR"
  local try
  for ((try = 1; try <= SIM_IMPORT_TRIES; try++)); do
    if (
      flock 8
      sim_log "$ref: importing"
      "${SIM_SAVE_ARGS[@]}" "$ref" | docker exec -i "$SIMCLUSTER_NAME" ctr -n k8s.io images import --platform "linux/$SIM_IMAGE_ARCH" - >/dev/null
    ) 8>"$SIMCLUSTER_DIR/import.lock"; then
      return 0
    fi
    if sim_container_down; then
      sim_log "$ref: the import failed and the container is not running"
      return 1
    fi
    if ((try < SIM_IMPORT_TRIES)); then
      sim_log "$ref: the import failed (try $try of $SIM_IMPORT_TRIES); trying again in ${SIM_IMPORT_RETRY_DELAY}s"
      sleep "$SIM_IMPORT_RETRY_DELAY"
    fi
  done
  sim_log "$ref: the import failed $SIM_IMPORT_TRIES times"
  return 1
}

# sim_wait_k3s_imports waits, at most 300 s, until k3s has imported every
# tarball of $SIMCLUSTER_DIR/images (the air-gap bundle), which it does in
# the background as it starts: an import of ours into containerd waits
# for it, so the two never stream at once. k3s logs "Imported images from
# <file>" when one is done, and "Error encountered while importing <file>"
# when one failed; a wait that runs out only logs, since the bundle's
# images are k3s's own and the imports that follow do not need them.
sim_wait_k3s_imports() {
  local files=() f
  for f in "$SIMCLUSTER_DIR"/images/*.tar "$SIMCLUSTER_DIR"/images/*.tar.*; do
    [[ -f $f ]] && files+=("/var/lib/rancher/k3s/agent/images/${f##*/}")
  done
  ((${#files[@]})) || return 0
  # shellcheck disable=SC2317 # called by sim_wait_running
  k3s_imported() {
    local seen f
    seen=$(docker logs "$SIMCLUSTER_NAME" 2>&1 | grep -oE '(Imported images from|while importing) /var/lib/rancher/k3s/agent/images/[^ ":]+' || true)
    for f in "${files[@]}"; do
      grep -qE "(from|importing) ${f//./\\.}\$" <<<"$seen" || return 1
    done
  }
  k3s_imported && return 0
  sim_log "waiting for k3s to import ${files[*]##*/} before importing images into containerd"
  if ! sim_wait_running 300 "k3s to import ${files[*]##*/}" k3s_imported; then
    [[ $(sim_container_state) == true ]] || return 1
    sim_log "importing anyway"
  fi
}

# sim_image_ids <ref> prints, comma separated, the image ID and the repo
# digests the kubelet may report as a container's imageID, once the
# kubelet's image service lists the image (at most 60 s).
sim_image_ids() {
  local deadline=$((SECONDS + 60)) out
  until out=$(docker exec "$SIMCLUSTER_NAME" crictl inspecti -o json "$1" 2>/dev/null); do
    if ((SECONDS >= deadline)); then
      sim_log "timed out after 60s waiting for the kubelet's image service to list $1"
      return 1
    fi
    sleep 1
  done
  jq -r '[.status.id] + (.status.repoDigests // []) | map(select(. != null and . != "")) | join(",")' <<<"$out"
}

# sim_wait_pids <what> <pid>... waits for every one of the background jobs
# and fails, naming what, when one of them failed.
sim_wait_pids() {
  local what=$1 pid failed=0
  shift
  for pid in "$@"; do
    wait "$pid" || failed=1
  done
  ((failed == 0)) || sim_die "$what failed"
}

# The rollout wait: SIM_ROLLOUT_BASE_SECONDS, plus SIM_ROLLOUT_SECONDS_PER_OBJECT
# for each object, since the infra Node's one kubelet starts the pods of
# every object, one emulated agent each.
SIM_ROLLOUT_BASE_SECONDS=300
SIM_ROLLOUT_SECONDS_PER_OBJECT=5

# The jq of a rolled out Deployment, DaemonSet or StatefulSet, with the
# checks kubectl rollout status makes: the controller saw the latest spec,
# every replica runs it, no older one is left, and every one is available.
SIM_ROLLED_OUT_JQ='
  def rolled_out:
    ((.status.observedGeneration // 0) >= (.metadata.generation // 0)) and
    if .kind == "Deployment" then
      ((.status.updatedReplicas // 0) >= (.spec.replicas // 1)) and
      ((.status.replicas // 0) == (.status.updatedReplicas // 0)) and
      ((.status.availableReplicas // 0) >= (.status.updatedReplicas // 0))
    elif .kind == "DaemonSet" then
      ((.status.updatedNumberScheduled // 0) >= (.status.desiredNumberScheduled // 0)) and
      ((.status.numberAvailable // 0) >= (.status.desiredNumberScheduled // 0))
    elif .kind == "StatefulSet" then
      ((.status.readyReplicas // 0) >= (.spec.replicas // 1)) and
      ((.status.updatedReplicas // 0) >= (.spec.replicas // 1))
    else true end;
  def key: "\(.kind | ascii_downcase)/\(.metadata.name)";'

# sim_rollout_pending <namespace> <object>... prints each object (as
# kind/name, kind/name.apps or kind.apps/name) not rolled out yet, missing
# ones included, and fails when it cannot list them. One list per call,
# however many objects.
sim_rollout_pending() {
  local ns=$1 list
  shift
  list=$(kubectl -n "$ns" get deployments,daemonsets,statefulsets -o json) || return 1
  jq -r --args "$SIM_ROLLED_OUT_JQ"'
    ([.items[] | select(rolled_out) | key]) as $done
    | $ARGS.positional[] | . as $obj
    | (sub("\\.apps/"; "/") | ascii_downcase) as $k
    | select(($done | index($k)) == null) | $obj' "$@" <<<"$list"
}

# sim_rollout_report <namespace> <object>... prints the status of each object
# not rolled out, and every pod of the namespace that is not ready with the
# reason its containers wait.
sim_rollout_report() {
  local ns=$1 list pods
  shift
  list=$(kubectl -n "$ns" get deployments,daemonsets,statefulsets -o json 2>/dev/null) || list='{"items":[]}'
  pods=$(kubectl -n "$ns" get pods -o json 2>/dev/null) || pods='{"items":[]}'
  jq -r --args "$SIM_ROLLED_OUT_JQ"'
    $ARGS.positional[] as $obj
    | ($obj | sub("\\.apps/"; "/") | ascii_downcase) as $k
    | ([.items[] | select(key == $k)][0]) as $o
    | if $o == null then "  \($obj): not found"
      elif ($o | rolled_out) then empty
      else "  \($obj): generation \($o.metadata.generation // 0), observed \($o.status.observedGeneration // 0), status \($o.status | del(.conditions) | tojson)" end' "$@" <<<"$list" >&2
  jq -r '.items[] | select(([.status.conditions[]? | select(.type == "Ready" and .status == "True")] | length) == 0)
    | "  pod/\(.metadata.name): \(.status.phase // "unknown")\([.status.containerStatuses[]?.state.waiting.reason // empty] | if length > 0 then ", " + join(", ") else "" end)"' <<<"$pods" >&2
}

# sim_rollouts <namespace> <object>... waits for the rollout of every object
# at once, polling one list of the namespace's workloads every 2 s rather
# than watching each object, for SIM_ROLLOUT_BASE_SECONDS plus
# SIM_ROLLOUT_SECONDS_PER_OBJECT per object. On a timeout it prints what is
# not rolled out and why, and fails.
sim_rollouts() {
  local ns=$1
  shift
  (($#)) || return 0
  local secs=$((SIM_ROLLOUT_BASE_SECONDS + SIM_ROLLOUT_SECONDS_PER_OBJECT * $#))
  if ! sim_wait_running "$secs" "the rollout in $ns of $*" sim_none sim_rollout_pending "$ns" "$@"; then
    sim_log "not rolled out in $ns:"
    sim_rollout_report "$ns" "$@"
    sim_die "the rollout in $ns of $* failed"
  fi
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
    --set-file emulate.clusterSpec="$SIMCLUSTER_DIR/spec.yaml" --set "$(sim_image_tag_key simulator)=$(sim_tag "$IMAGE_SIMULATOR")" >/dev/null
}

# sim_helm_joulie <values file>... installs or upgrades Joulie with the images
# of images.env; sim_apply_crds applies the CRDs. Run by deploy.sh and
# reload.sh.
sim_helm_joulie() {
  local args=() f
  for f in "$@"; do args+=(-f "$f"); done
  helm upgrade --install "$SIM_RELEASE_JOULIE" "$SIM_REPO/charts/joulie" -n "$SIM_NS_JOULIE" "${args[@]}" --skip-crds \
    --set "$(sim_image_tag_key agent)=$(sim_tag "$IMAGE_AGENT")" \
    --set "$(sim_image_tag_key controller-manager)=$(sim_tag "$IMAGE_CONTROLLER_MANAGER")" \
    --set "$(sim_image_tag_key scheduler)=$(sim_tag "$IMAGE_SCHEDULER")" >/dev/null
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
    --image-arch "$SIM_IMAGE_ARCH" --root "$SIM_HWEMU_ROOT" "${SIM_PROFILES[@]}" >"$out.tmp" 2>"$excl"; then
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

# sim_apply_config <manifest> <configmap> <key>=<file>... applies a backend's
# ConfigMap from the files, then its manifest, and restarts the manifest's
# Deployment only when an earlier configuration changed: Prometheus and the
# Collector read their configuration only at start, and a Deployment created
# here starts with the ConfigMap in place.
sim_apply_config() {
  local manifest=$1 cm=$2 before after objs dep kv
  shift 2
  local files=()
  for kv in "$@"; do files+=(--from-file="$kv"); done
  before=$(kubectl -n "$SIM_NS_MONITORING" get configmap "$cm" -o json 2>/dev/null | jq -cS '.data // {}') || before=
  after=$(kubectl -n "$SIM_NS_MONITORING" create configmap "$cm" "${files[@]}" --dry-run=client -o json)
  kubectl apply --server-side --field-manager="$SIM_FIELD_MANAGER" --force-conflicts -f - <<<"$after" >/dev/null
  objs=$(kubectl apply --server-side --field-manager="$SIM_FIELD_MANAGER" --force-conflicts -f "$manifest" -o name)
  dep=$(grep -m1 '^deployment' <<<"$objs" || true)
  [[ -n $dep ]] || sim_die "$manifest holds no Deployment"
  if [[ -n $before && $before != "$(jq -cS '.data // {}' <<<"$after")" ]]; then
    kubectl -n "$SIM_NS_MONITORING" rollout restart "$dep" >/dev/null
  fi
  sim_rollouts "$SIM_NS_MONITORING" "$dep"
}

# sim_apply_backend sets up Prometheus, and the Collector of the modes that
# run one, for the current telemetry mode and timing. A core Collector left
# from mode otel-collector stays, since nothing sends to it or scrapes it;
# the bridge Collector is deleted outside mode otel-bridge, since it would
# go on pushing hw.* series of its own into Prometheus.
sim_apply_backend() {
  local render=$SIMCLUSTER_DIR/render
  mkdir -p "$render"
  sim_cli telemetry "${SIM_TELEMETRY_FLAGS[@]}" --part prom-config >"$render/prometheus.yml"
  sim_apply_config "$SIM_MANIFESTS/prometheus.yaml" prometheus-config "$SIM_PROM_CONFIG_KEY=$render/prometheus.yml"
  case $SIMCLUSTER_TELEMETRY in
    otel-collector)
      sim_cli telemetry "${SIM_TELEMETRY_FLAGS[@]}" --part otelcol-config >"$render/otelcol.yaml"
      sim_apply_config "$SIM_MANIFESTS/otelcol.yaml" otelcol-config "$SIM_OTELCOL_CONFIG_KEY=$render/otelcol.yaml"
      ;;
    otel-bridge)
      sim_cli telemetry "${SIM_TELEMETRY_FLAGS[@]}" --part otelcol-config >"$render/otelcol-bridge.yaml"
      sim_apply_config "$SIM_MANIFESTS/otelcol-bridge.yaml" otelcol-bridge-config \
        "$SIM_BRIDGE_EXAMPLE_KEY=$SIM_BRIDGE_EXAMPLE" "$SIM_OTELCOL_CONFIG_KEY=$render/otelcol-bridge.yaml"
      ;;
  esac
  if [[ $SIMCLUSTER_TELEMETRY != otel-bridge && -n $(kubectl -n "$SIM_NS_MONITORING" get deployment otelcol-bridge -o name --ignore-not-found) ]]; then
    kubectl delete --ignore-not-found -f "$SIM_MANIFESTS/otelcol-bridge.yaml" >/dev/null
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
  sim_minus <(printf '%s\n' "$@") <(printf '%s\n' "$have")
}

# sim_minus <file> <file> prints, sorted, the lines of the first that the
# second lacks: one set difference, not one search per line.
sim_minus() {
  LC_ALL=C comm -23 <(LC_ALL=C sort -u "$1" | sed '/^$/d') <(LC_ALL=C sort -u "$2")
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
  sim_minus <(printf '%s\n' "$nodes") <(printf '%s\n' "$twins")
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
# whose imageID is not the one images.env recorded for it, and fails
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
