#!/usr/bin/env bash
#
# images.sh - build the Joulie images from the working tree and import them,
# and the pinned third-party images, into the simulated cluster.
#
# Usage: scripts/simcluster/images.sh [--joulie [component]... | --third-party]
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
# Third-party images: pulled by the digest versions.env pins, checked, then
# imported by tag. An image the cluster's containerd already lists is never
# imported again.
#
# Writes $SIMCLUSTER_DIR/images.env: every image reference, the image IDs the
# kubelet may report for the Joulie ones (IMAGE_IDS_*, comma separated), and
# the Go release that built them.
#
set -euo pipefail
# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
sim_init
sim_require docker jq sha256sum

what=all
case ${1:-} in
  "") ;;
  --joulie) what=joulie ;;
  --third-party) what=third-party ;;
  -h | --help)
    sim_usage "$0"
    exit 0
    ;;
  *) sim_die "unknown argument $1 (see --help)" ;;
esac
named=()
if [[ $what == joulie ]]; then
  for c in "${@:2}"; do
    case $c in
      agent | controller-manager | scheduler | simulator) named+=("$c") ;;
      *) sim_die "component '$c' is not agent, controller-manager, scheduler or simulator" ;;
    esac
  done
fi
sim_require_container

# images.env is rewritten whole; keys this run does not touch keep their
# values.
declare -A out=()
if [[ -s $SIMCLUSTER_DIR/images.env ]]; then
  while IFS='=' read -r k v; do
    if [[ $k =~ ^[A-Z][A-Z0-9_]*$ ]]; then
      out[$k]=$v
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
  ((${#named[@]} == 0)) || [[ -z ${out[$2]:-} || " ${named[*]} " == *" $1 "* ]]
}

# build_image <ref> <context dir>: the context holds the Dockerfile.
build_image() {
  if docker image inspect "$1" >/dev/null 2>&1; then
    return 0
  fi
  sim_log "$1: building"
  docker build --quiet --platform "linux/$SIM_IMAGE_ARCH" -t "$1" "$2" >/dev/null
}

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
    build_image "$ref" "$ctx/$c"
    sim_import_image "$ref"
    out[IMAGE_$key]=$ref
    out[IMAGE_IDS_$key]=$(sim_image_ids "$ref")
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
    build_image "$ref" "$ctx/simulator"
    sim_import_image "$ref"
    out[IMAGE_SIMULATOR]=$ref
    out[IMAGE_IDS_SIMULATOR]=$(sim_image_ids "$ref")
  fi

  out[GOTOOLCHAIN]=$GOTOOLCHAIN
  out[GO_VERSION]=$(cd "$SIM_REPO" && "$GO" env GOVERSION)
fi

# third_party <image:tag> <digest>
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

if [[ $what != joulie ]]; then
  third_party "$PROMETHEUS_IMAGE" "$PROMETHEUS_DIGEST"
  out[IMAGE_PROMETHEUS]=$PROMETHEUS_IMAGE@$PROMETHEUS_DIGEST
  if [[ $SIMCLUSTER_TELEMETRY == otel-collector ]]; then
    third_party "$OTELCOL_IMAGE" "$OTELCOL_DIGEST"
    out[IMAGE_OTELCOL]=$OTELCOL_IMAGE@$OTELCOL_DIGEST
  fi
fi

{
  printf '# Written by scripts/simcluster/images.sh; read by deploy.sh, status.sh and the end-to-end test.\n'
  for k in $(printf '%s\n' "${!out[@]}" | LC_ALL=C sort); do
    printf '%s=%s\n' "$k" "${out[$k]}"
  done
} >"$SIMCLUSTER_DIR/images.env.tmp"
mv "$SIMCLUSTER_DIR/images.env.tmp" "$SIMCLUSTER_DIR/images.env"
