#!/usr/bin/env bash
#
# tree.sh - show the hwemu tree of an emulated Node, the files its agent
# reads and writes.
#
# Usage: scripts/simcluster/tree.sh [node [path]]
#
# Without a Node, lists the Nodes that have a tree. With a Node, prints every
# file of its tree with its first line, the values the agent sees (energy
# counters, limits, frequencies, GPU state). With a path inside the tree,
# prints that file. The Node may also come from NODE.
#
set -euo pipefail
# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
case ${1:-} in
  -h | --help)
    sim_usage "$0"
    exit 0
    ;;
esac
sim_init
sim_require docker
sim_require_container

node=${1:-${NODE:-}}
path=${2:-}
if [[ -z $node ]]; then
  docker exec "$SIMCLUSTER_NAME" ls -1 "$SIM_HWEMU_ROOT"
  exit 0
fi
[[ $node =~ ^[a-z0-9]([a-z0-9-]*[a-z0-9])?$ ]] || sim_die "$node is not a Node name"
[[ $path != *..* ]] || sim_die "$path leaves the tree"
dir=$SIM_HWEMU_ROOT/$node
if [[ -n $path ]]; then
  docker exec "$SIMCLUSTER_NAME" cat "$dir/${path#/}"
  exit 0
fi
docker exec "$SIMCLUSTER_NAME" sh -c '
  cd "$1" || exit 1
  find . -type f | sort | while read -r f; do
    printf "%s = %s\n" "${f#./}" "$(head -c 200 "$f" | head -n 1)"
  done' sh "$dir"
