#!/usr/bin/env bash
#
# down.sh - delete the simulated cluster.
#
# Usage: scripts/simcluster/down.sh
#
# Removes the container $SIMCLUSTER_NAME with its anonymous volumes (the k3s
# image declares four), the named volumes $SIMCLUSTER_NAME-k3s and
# $SIMCLUSTER_NAME-hwemu, and the files under $SIMCLUSTER_DIR that describe
# the cluster. Keeps $SIMCLUSTER_DIR/images (image tarballs), runs (smoke
# results) and bin (build outputs). Only a name matching
# ^joulie-sim(-[a-z0-9]+)*$ is accepted, so nothing else on the host is
# touched.
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
  *) sim_die "unknown argument $1 (see --help); the cluster is named by SIMCLUSTER_NAME" ;;
esac
sim_init
sim_require docker

N=$SIMCLUSTER_NAME
if docker inspect "$N" >/dev/null 2>&1; then
  sim_log "removing container $N"
  docker rm -f -v "$N" >/dev/null
fi
for v in "$N-k3s" "$N-hwemu"; do
  if docker volume inspect "$v" >/dev/null 2>&1; then
    sim_log "removing volume $v"
    docker volume rm "$v" >/dev/null
  fi
done
D=$SIMCLUSTER_DIR
rm -rf "$D/kubeconfig" "$D/kube-cache" "$D/cluster.env" "$D/telemetry.env" "$D/spec.yaml" \
  "$D/images.env" "$D/deploy.env" "$D/crds.sha" "$D/values" "$D/render" "$D/build"
sim_log "cluster $N removed; kept $D/images and $D/runs"
