#!/usr/bin/env bash
#
# deploy.sh - install the simulator and Joulie into the simulated cluster and
# wait until every component reports.
#
# Usage: scripts/simcluster/deploy.sh [--skip-images] [-f values.yaml]...
#
#   --skip-images  use the images images.sh imported last
#   -f FILE        a Joulie values file, after the generated ones and before
#                  those in SIMCLUSTER_EXTRA_VALUES (space separated)
#
# Steps: build and import the images (images.sh); apply the CRDs; install the
# simulator in emulate mode with the cluster's spec; render the agent
# DaemonSet with the same values files as the install and give each Node it
# would reach an emulated agent; install Joulie; wait for the rollouts, one
# NodeHardware per emulated agent, the telemetry of every measured Node in
# Prometheus and a written NodeTwin per managed Node; then check that every
# pod runs the image images.sh imported. Writes $SIMCLUSTER_DIR/deploy.env,
# whose JOULIE_VALUES and SIM_VALUES reload.sh installs with again; SIM_VALUES
# ends with the SIMCLUSTER_SIM_EXTRA_VALUES files, so a reload keeps a
# scenario's simulator.set.
#
# Settings (environment):
#   SIMCLUSTER_NAME          the cluster up.sh created (joulie-sim)
#   SIMCLUSTER_DIR           state directory (~/.cache/joulie/simcluster/<name>)
#   SIMCLUSTER_TELEMETRY     telemetry mode (otel); Prometheus is set up again
#                            when the mode or the timing changed since up.sh
#   SIMCLUSTER_POWER_SCOPE   node power scope Joulie reads: components or host
#                            (components)
#   SIMCLUSTER_TIMING        scripts/simcluster/timing/<name>.env (dev)
#   SIMCLUSTER_PROFILES_DIR  local hardware profiles, read by the host tools
#                            only: the in-cluster simulator reads its
#                            built-in profiles, and up.sh refuses a spec that
#                            needs a local one
#   SIMCLUSTER_EXTRA_VALUES  Joulie values files, space separated
#   SIMCLUSTER_SIM_EXTRA_VALUES  simulator values files, space separated, after
#                            the generated ones (scenario.sh sets it for a
#                            scenario's simulator.set)
#
set -euo pipefail
# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

skip_images=0
extra=()
while (($#)); do
  case $1 in
    --skip-images)
      skip_images=1
      shift
      ;;
    -f | --values)
      (($# >= 2)) || sim_die "$1 needs a file"
      extra+=("$(sim_path "$2")")
      shift 2
      ;;
    -h | --help)
      sim_usage "$0"
      exit 0
      ;;
    *) sim_die "unknown argument $1 (see --help)" ;;
  esac
done

sim_init
sim_require docker kubectl helm jq sha256sum
for f in ${SIMCLUSTER_EXTRA_VALUES:-}; do extra+=("$(sim_path "$f")"); done
# sim_helm_sim adds these itself; deploy.env records them for reload.sh.
sim_extra=()
for f in ${SIMCLUSTER_SIM_EXTRA_VALUES:-}; do sim_extra+=("$(sim_path "$f")"); done
sim_require_cluster
D=$SIMCLUSTER_DIR
spec=$D/spec.yaml
[[ -s $spec ]] || sim_die "$spec is missing; run make simcluster-up"
vals=$D/values
mkdir -p "$vals"
start=$SECONDS

# 1. Images.
if ((!skip_images)); then
  bash "$SIM_SCRIPTS/images.sh"
fi
sim_load_images
if ! sim_backend_current; then
  sim_log "setting Prometheus up for telemetry $SIMCLUSTER_TELEMETRY, timing $SIMCLUSTER_TIMING"
  sim_apply_backend
fi

# 2. The CRDs, before any helm call.
sim_apply_crds

# 3. The simulator in emulate mode.
sim_cli telemetry "${SIM_TELEMETRY_FLAGS[@]}" --part sim-values >"$vals/sim-values.yaml"
sim_log "installing the simulator ($(sim_tag "$IMAGE_SIMULATOR"))"
sim_files=("$SIM_REPO/values/simcluster-simulator.yaml" "$vals/sim-values.yaml")
sim_helm_sim "${sim_files[@]}"

# 4. Ready once its first snapshot is published, so every hwemu tree exists
# before an agent mounts one.
kubectl -n "$SIM_NS_SIM" rollout status "deployment/$SIM_SIM_DEPLOYMENT" --timeout=300s >/dev/null
sim_wait 300 "the simulator's /readyz" sim_sim readyz

# 5. The emulated agents: the Nodes the real DaemonSet would reach with
# exactly the install's values files. agent-nodes.yaml is among them; it only
# sets agent.emulated.nodes, which the DaemonSet render does not read.
sim_cli telemetry "${SIM_TELEMETRY_FLAGS[@]}" --part joulie-values >"$vals/joulie-values.yaml"
[[ -s $vals/agent-nodes.yaml ]] || printf '{}\n' >"$vals/agent-nodes.yaml"
files=("$SIM_REPO/values/simcluster.yaml" "$vals/agent-nodes.yaml" "$vals/joulie-values.yaml" "${extra[@]}")
sim_agents "$vals/agent-nodes.yaml" "${files[@]}"
agents=$(sim_count_agents "$vals/agent-nodes.yaml")
sim_check_pod_capacity "$agents"

# 6. Joulie.
sim_log "installing Joulie with $agents emulated agents"
sim_helm_joulie "${files[@]}"

# 7. Rollouts, then what each component publishes.
for ns in "$SIM_NS_JOULIE" "$SIM_NS_SIM"; do
  for obj in $(kubectl -n "$ns" get deployments,daemonsets,statefulsets -o name); do
    kubectl -n "$ns" rollout status "$obj" --timeout=300s >/dev/null
  done
done
mapfile -t nodes < <(sim_agent_nodes)
((${#nodes[@]} == agents)) ||
  sim_die "the Joulie chart rendered ${#nodes[@]} emulated agents for the $agents of agent-nodes.yaml"
agent_s=$(sim_seconds "$AGENT_RECONCILE_INTERVAL")
controller_s=$(sim_seconds "$CONTROLLER_RECONCILE_INTERVAL")
series_s=$(($(sim_seconds "$RATE_WINDOW") + (4 * OTLP_EXPORT_INTERVAL_MS + 999) / 1000))
if ((${#nodes[@]})); then
  sim_wait $((120 + 3 * agent_s)) "a NodeHardware for every emulated agent" sim_none sim_missing_nodehardware "${nodes[@]}"
fi
wait_json=$(sim_cli telemetry "${SIM_TELEMETRY_FLAGS[@]}" --part wait --spec "$spec" "${SIM_PROFILES_CLI[@]}")
sim_wait $((120 + series_s)) "every measured Node's series in Prometheus" sim_none sim_missing_series "$wait_json"
sim_wait $((120 + 3 * controller_s)) "a written NodeTwin for every managed Node" sim_none sim_missing_twins

# 8. Every pod runs what images.sh imported (DR-21).
sim_verify_images
join() {
  local IFS=,
  printf '%s' "$*"
}
{
  printf 'DEPLOYED_AT=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  printf 'TELEMETRY=%s\nPOWER_SCOPE=%s\nTIMING=%s\n' "$SIMCLUSTER_TELEMETRY" "$SIMCLUSTER_POWER_SCOPE" "$SIMCLUSTER_TIMING"
  printf 'AGENTS=%s\n' "$agents"
  printf 'JOULIE_VALUES=%s\n' "$(join "${files[@]}")"
  printf 'SIM_VALUES=%s\n' "$(join "${sim_files[@]}" "${sim_extra[@]}")"
  printf 'CRDS_SHA256=%s\n' "$(cat "$D/crds.sha")"
} >"$D/deploy.env"
sim_log "deployed in $((SECONDS - start)) s: $agents emulated agents; export KUBECONFIG=$KUBECONFIG"
