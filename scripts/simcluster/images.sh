#!/usr/bin/env bash
#
# images.sh - build the Joulie images from the working tree and import them,
# and the pinned third-party images, into the simulated cluster.
#
# Usage: scripts/simcluster/images.sh [--joulie [component]... | --third-party] [--wait-container]
#
# Joulie images: one go build of the agent, controller manager, scheduler
# extender, simulator and fakesmi (linux/amd64, CGO off, no VCS stamp), each
# binary wrapped FROM $DISTROLESS_STATIC at the path its release image uses.
# A tag is joulie.local/joulie-<component>:dev-<first 12 hex of the binary's
# sha256> (the simulator image, which also carries fakesmi, hashes both), so
# an unchanged binary keeps its tag and is neither rebuilt nor imported again.
# --joulie followed by components (agent, controller-manager, scheduler,
# simulator) builds, imports and records only those; the others keep their
# images.env entries, the images their pods run (reload.sh relies on this).
#
# Third-party images (Prometheus, KWOK, and the Collector of the telemetry
# mode): pulled by the digest versions.env pins, checked, then imported by
# tag. An image the cluster's containerd already lists is never imported
# again; containerd is listed once per run.
#
# Each image is built or pulled, imported and inspected in a pipeline of its
# own, and the pipelines run at once. --wait-container builds the binaries
# first and then waits for the cluster's container to run, so the build
# overlaps the cluster's start (smoke.sh runs it next to up.sh); without it
# a container that does not run is an error.
#
# Writes $SIMCLUSTER_DIR/images.env: every image reference, the image IDs the
# kubelet may report for the Joulie ones (IMAGE_IDS_*, comma separated), and
# the Go release that built them. The file is updated under a lock, keys
# this run does not set keeping their values, so two runs at once (up.sh's
# third-party images and smoke.sh's Joulie ones) both land.
#
set -euo pipefail
# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
sim_init
sim_require docker jq sha256sum flock

what=all
wait_container=0
named=()
for arg in "$@"; do
  case $arg in
    --joulie) what=joulie ;;
    --third-party) what='third-party' ;;
    --wait-container) wait_container=1 ;;
    -h | --help)
      sim_usage "$0"
      exit 0
      ;;
    agent | controller-manager | scheduler | simulator)
      [[ $what == joulie ]] || sim_die "a component ($arg) follows --joulie only (see --help)"
      named+=("$arg")
      ;;
    --*) sim_die "unknown argument $arg (see --help)" ;;
    *) sim_die "component '$arg' is not agent, controller-manager, scheduler or simulator" ;;
  esac
done
((wait_container)) || sim_require_container

# The keys of images.env this run had before it started, for wanted.
declare -A had=()
if [[ -s $SIMCLUSTER_DIR/images.env ]]; then
  while IFS='=' read -r k v; do
    if [[ $k =~ ^[A-Z][A-Z0-9_]*$ ]]; then
      had[$k]=$v
    fi
  done <"$SIMCLUSTER_DIR/images.env"
fi

sha12() {
  sha256sum "$1" | cut -c1-12
}

# wanted <component> <images.env key> succeeds when the component's image is
# to be built and recorded: no component was named, this one was, or
# images.env does not have it yet.
wanted() {
  ((${#named[@]} == 0)) || [[ -z ${had[$2]:-} || " ${named[*]} " == *" $1 "* ]]
}

# build_image <ref> <context dir>: the context holds the Dockerfile.
build_image() {
  if docker image inspect "$1" >/dev/null 2>&1; then
    return 0
  fi
  sim_log "$1: building"
  docker build --quiet --platform "linux/$SIM_IMAGE_ARCH" -t "$1" "$2" >/dev/null
}

# third_party <image:tag> <digest>: pulled by digest unless the local image
# has it, then imported.
third_party() {
  local img=$1 digest=$2
  local repo=${img%:*}
  has_digest() {
    docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$img" 2>/dev/null | grep -q "@$digest\$"
  }
  if ! has_digest; then
    sim_log "$img: pulling $digest"
    docker pull "$repo@$digest" >/dev/null
    docker tag "$repo@$digest" "$img"
    has_digest || sim_die "$img: its RepoDigests do not contain $digest"
  fi
  sim_import_image "$img"
}

results=$(mktemp -d)
# shellcheck disable=SC2317 # called by the EXIT trap
cleanup() { rm -rf "$results"; }
trap cleanup EXIT

# The Joulie binaries and build contexts, before the container is needed.
joulie=()
if [[ $what != third-party ]]; then
  [[ -n $GO ]] || sim_die "no Go toolchain found: install Go or set GO"
  bin=$SIMCLUSTER_DIR/bin
  ctx=$SIMCLUSTER_DIR/build
  mkdir -p "$bin"
  sim_log "building the binaries with $GOTOOLCHAIN"
  # Without -buildvcs=false a binary carries the git revision and the dirty
  # flag, so a commit or a first edit would change every tag.
  (cd "$SIM_REPO" && CGO_ENABLED=0 GOOS=linux GOARCH="$SIM_IMAGE_ARCH" "$GO" build -trimpath -buildvcs=false -o "$bin/" \
    ./cmd/agent ./cmd/controller-manager ./cmd/scheduler ./simulator/cmd/simulator ./simulator/cmd/fakesmi)

  # Components of the Joulie chart: the release Dockerfile puts the binary at
  # /joulie and keeps distroless' nonroot user.
  for c in agent controller-manager scheduler; do
    key=${c//-/_}
    key=${key^^}
    wanted "$c" "IMAGE_$key" || continue
    ref=$SIM_IMAGE_REGISTRY/joulie-$c:dev-$(sha12 "$bin/$c")
    rm -rf "${ctx:?}/$c"
    mkdir -p "$ctx/$c"
    cp "$bin/$c" "$ctx/$c/joulie"
    printf 'FROM %s\nCOPY joulie /joulie\nENTRYPOINT ["/joulie"]\n' "$DISTROLESS_STATIC" >"$ctx/$c/Dockerfile"
    joulie+=("$key" "$ref" "$ctx/$c")
  done

  # The simulator, at the path of simulator/Dockerfile, with fakesmi where
  # emulate mode copies it from.
  if wanted simulator IMAGE_SIMULATOR; then
    ref=$SIM_IMAGE_REGISTRY/joulie-simulator:dev-$(printf '%s %s\n' "$(sha12 "$bin/simulator")" "$(sha12 "$bin/fakesmi")" | sha256sum | cut -c1-12)
    rm -rf "${ctx:?}/simulator"
    mkdir -p "$ctx/simulator"
    cp "$bin/simulator" "$ctx/simulator/joulie-simulator"
    cp "$bin/fakesmi" "$ctx/simulator/fakesmi"
    printf 'FROM %s\nCOPY joulie-simulator /joulie-simulator\nCOPY fakesmi /usr/local/lib/joulie/fakesmi\nUSER 65532:65532\nENTRYPOINT ["/joulie-simulator"]\n' \
      "$DISTROLESS_STATIC" >"$ctx/simulator/Dockerfile"
    joulie+=(SIMULATOR "$ref" "$ctx/simulator")
  fi
  printf 'GOTOOLCHAIN=%s\nGO_VERSION=%s\n' "$GOTOOLCHAIN" "$(cd "$SIM_REPO" && "$GO" env GOVERSION)" >"$results/go.env"
fi

if ((wait_container)); then
  # shellcheck disable=SC2317 # called by sim_wait
  container_runs() { [[ $(sim_container_state) == true ]]; }
  sim_wait 300 "container $SIMCLUSTER_NAME to run" container_runs
fi

# containerd starts a moment after the container. One containerd listing
# and one docker save probe for every pipeline.
sim_wait 180 "containerd in $SIMCLUSTER_NAME" docker exec "$SIMCLUSTER_NAME" ctr -n k8s.io version
SIM_CLUSTER_IMAGES=$(sim_cluster_images)
sim_save_args

pids=()
for ((i = 0; i < ${#joulie[@]}; i += 3)); do
  key=${joulie[i]} ref=${joulie[i + 1]} dir=${joulie[i + 2]}
  (
    build_image "$ref" "$dir"
    sim_import_image "$ref"
    printf 'IMAGE_%s=%s\nIMAGE_IDS_%s=%s\n' "$key" "$ref" "$key" "$(sim_image_ids "$ref")" >"$results/$key.env"
  ) &
  pids+=($!)
done
if [[ $what != joulie ]]; then
  third=(PROMETHEUS "$PROMETHEUS_IMAGE" "$PROMETHEUS_DIGEST" KWOK "$KWOK_IMAGE" "$KWOK_DIGEST")
  case $SIMCLUSTER_TELEMETRY in
    otel-collector) third+=(OTELCOL "$OTELCOL_IMAGE" "$OTELCOL_DIGEST") ;;
    otel-bridge) third+=(OTELCOL_CONTRIB "$OTELCOL_CONTRIB_IMAGE" "$OTELCOL_CONTRIB_DIGEST") ;;
  esac
  for ((i = 0; i < ${#third[@]}; i += 3)); do
    key=${third[i]} img=${third[i + 1]} digest=${third[i + 2]}
    (
      third_party "$img" "$digest"
      printf 'IMAGE_%s=%s@%s\n' "$key" "$img" "$digest" >"$results/$key.env"
    ) &
    pids+=($!)
  done
fi
sim_wait_pids "building and importing the images" ${pids[@]+"${pids[@]}"}

# images.env, updated under its lock: read again, the keys of this run set.
(
  flock 9
  declare -A out=()
  for f in "$SIMCLUSTER_DIR/images.env" "$results"/*.env; do
    [[ -s $f ]] || continue
    while IFS='=' read -r k v; do
      if [[ $k =~ ^[A-Z][A-Z0-9_]*$ ]]; then
        out[$k]=$v
      fi
    done <"$f"
  done
  {
    printf '# Written by scripts/simcluster/images.sh; read by deploy.sh, status.sh and the end-to-end test.\n'
    for k in $(printf '%s\n' "${!out[@]}" | LC_ALL=C sort); do
      printf '%s=%s\n' "$k" "${out[$k]}"
    done
  } >"$SIMCLUSTER_DIR/images.env.tmp"
  mv "$SIMCLUSTER_DIR/images.env.tmp" "$SIMCLUSTER_DIR/images.env"
) 9>"$SIMCLUSTER_DIR/images.env.lock"
