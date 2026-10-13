#!/usr/bin/env bash
#
# status.sh - print the state of the simulated cluster and check it.
#
# Usage: scripts/simcluster/status.sh
#
# Prints every Node with its pool, profile and NodeTwin power reading, then
# checks, and exits 1 when one fails:
#   - every Node is Ready;
#   - no pod without the joulie.io/workload-class annotation is bound to a
#     fake Node (infrastructure belongs on the infra Node);
#   - every Joulie and simulator container runs the image images.sh imported;
#   - every emulated agent's Node has a NodeHardware;
#   - every measured Node has series in Prometheus.
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
  *) sim_die "unknown argument $1 (see --help)" ;;
esac
sim_init
sim_require docker kubectl jq
sim_require_cluster

problems=()
# Large lists go through files: one command-line argument holds at most
# 128 KiB on Linux.
tmp=$SIMCLUSTER_DIR/render
mkdir -p "$tmp"
kubectl get nodes -o json >"$tmp/status-nodes.json"
kubectl get nodetwins -o json >"$tmp/status-nodetwins.json" 2>/dev/null || echo '{"items":[]}' >"$tmp/status-nodetwins.json"

printf '%-28s %-6s %-14s %-26s %-20s %s\n' NODE READY POOL PROFILE TWIN-SOURCE TWIN-W
jq -r --slurpfile twins "$tmp/status-nodetwins.json" '
  ($twins[0].items | map({key: .spec.nodeName, value: (.status.powerMeasurement // {})}) | from_entries) as $pm
  | .items[] | .metadata as $m
  | [$m.name,
     ([.status.conditions[]? | select(.type == "Ready") | .status][0] // "Unknown"),
     ($m.labels["sim.joulie.io/pool"] // "-"),
     ($m.annotations["sim.joulie.io/profile"] // "-"),
     ($pm[$m.name].source // "-"),
     ($pm[$m.name].measuredNodePowerW // "-" | tostring)]
  | @tsv' "$tmp/status-nodes.json" | while IFS=$'\t' read -r n r p prof src w; do
  printf '%-28s %-6s %-14s %-26s %-20s %s\n' "$n" "$r" "$p" "$prof" "$src" "$w"
done

notready=$(jq -r '.items[] | select(([.status.conditions[]? | select(.type == "Ready") | .status][0] // "") != "True") | .metadata.name' "$tmp/status-nodes.json")
for n in $notready; do problems+=("Node $n is not Ready"); done

fake=$(jq -c '[.items[] | select(.metadata.annotations["kwok.x-k8s.io/node"] == "fake") | .metadata.name]' "$tmp/status-nodes.json")
stray=$(kubectl get pods -A -o json | jq -r --argjson fake "$fake" '
  .items[] | select(.spec.nodeName as $n | $fake | index([$n]))
  | select((.metadata.annotations["joulie.io/workload-class"] // "") == "")
  | "\(.metadata.namespace)/\(.metadata.name) on \(.spec.nodeName)"')
while IFS= read -r p; do
  if [[ -n $p ]]; then
    problems+=("pod $p has no joulie.io/workload-class annotation and runs on a fake Node")
  fi
done <<<"$stray"

if ! sim_verify_images; then
  problems+=("a container does not run the image images.sh imported")
fi

mapfile -t agent_nodes < <(sim_agent_nodes)
if ((${#agent_nodes[@]})); then
  for n in $(sim_missing_nodehardware "${agent_nodes[@]}"); do
    problems+=("Node $n has an emulated agent and no NodeHardware")
  done
fi

if [[ -s $SIMCLUSTER_DIR/spec.yaml ]]; then
  wait_json=$(sim_cli telemetry "${SIM_TELEMETRY_FLAGS[@]}" --part wait --spec "$SIMCLUSTER_DIR/spec.yaml" "${SIM_PROFILES_CLI[@]}")
  if missing=$(sim_missing_series "$wait_json"); then
    for n in $missing; do problems+=("Node $n has no series in Prometheus ($(jq -r .query <<<"$wait_json"))"); done
  else
    problems+=("Prometheus did not answer through the API server's service proxy")
  fi
else
  problems+=("$SIMCLUSTER_DIR/spec.yaml is missing; run make simcluster-up")
fi

printf '\n%s emulated agents\n' "${#agent_nodes[@]}"
if ((${#problems[@]})); then
  printf '\n%s problem(s):\n' "${#problems[@]}"
  printf '  %s\n' "${problems[@]}"
  exit 1
fi
printf 'no problems\n'
