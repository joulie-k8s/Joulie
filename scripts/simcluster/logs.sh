#!/usr/bin/env bash
#
# logs.sh - print the logs of a component of the simulated cluster.
#
# Usage: scripts/simcluster/logs.sh <component> [node] [kubectl logs flags...]
#
# Components: agent (the emulated agent of a Node; without a Node, every
# agent), controller-manager, scheduler, simulator, prometheus, otelcol,
# kwok, k3s (the container's own log). The component may also come from
# COMPONENT or C and the Node from NODE. Without flags, prints the last 200
# lines; pass -f to follow.
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
sim_require docker kubectl jq
sim_require_cluster

component=${COMPONENT:-${C:-}}
if (($#)) && [[ $1 != -* ]]; then
  component=$1
  shift
fi
node=${NODE:-}
if (($#)) && [[ $1 != -* ]]; then
  node=$1
  shift
fi
flags=("$@")
((${#flags[@]})) || flags=(--tail=200)

# monitoring_deployment <name pattern> prints the monitoring Deployment
# whose name matches.
monitoring_deployment() {
  kubectl -n "$SIM_NS_MONITORING" get deployments -o json |
    jq -r --arg re "$1" '.items[] | select(.metadata.name | test($re)) | "deployment/" + .metadata.name' | head -n1
}

case $component in
  agent)
    if [[ -n $node ]]; then
      dep=$(kubectl -n "$SIM_NS_JOULIE" get deployments -o json | jq -r --arg n "$node" '
        .items[] | select((.metadata.annotations["sim.joulie.io/node"] // .spec.template.metadata.annotations["sim.joulie.io/node"]) == $n)
        | "deployment/" + .metadata.name' | head -n1)
      [[ -n $dep ]] || sim_die "no emulated agent for Node $node"
      kubectl -n "$SIM_NS_JOULIE" logs "$dep" "${flags[@]}"
    else
      kubectl -n "$SIM_NS_JOULIE" logs -l app.kubernetes.io/name=joulie-agent --prefix --max-log-requests=50 "${flags[@]}"
    fi
    ;;
  controller-manager) kubectl -n "$SIM_NS_JOULIE" logs deployment/joulie-controller-manager "${flags[@]}" ;;
  scheduler) kubectl -n "$SIM_NS_JOULIE" logs deployment/joulie-scheduler-extender "${flags[@]}" ;;
  simulator) kubectl -n "$SIM_NS_SIM" logs "deployment/$SIM_SIM_DEPLOYMENT" "${flags[@]}" ;;
  prometheus | otelcol)
    re=prometheus
    [[ $component == otelcol ]] && re='otelcol|collector'
    dep=$(monitoring_deployment "$re")
    [[ -n $dep ]] || sim_die "no $component Deployment in $SIM_NS_MONITORING"
    kubectl -n "$SIM_NS_MONITORING" logs "$dep" "${flags[@]}"
    ;;
  kwok) kubectl -n kube-system logs deployment/kwok-controller "${flags[@]}" ;;
  k3s) docker logs "${flags[@]}" "$SIMCLUSTER_NAME" ;;
  *) sim_die "component '${component}' is not agent, controller-manager, scheduler, simulator, prometheus, otelcol, kwok or k3s" ;;
esac
