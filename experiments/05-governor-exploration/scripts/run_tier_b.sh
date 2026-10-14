#!/usr/bin/env bash
#
# run_tier_b.sh - Tier B of experiment 05: one arm on a fresh simulated
# cluster (scripts/simcluster), Joulie in the loop, then the governor
# governor checks of tests/simcluster/e2e_test.go against it.
#
# Usage: experiments/05-governor-exploration/scripts/run_tier_b.sh ARM
#        experiments/05-governor-exploration/scripts/run_tier_b.sh --check-context
#
#   ARM  caps-only, cpu, gpu, all or all-sustained: Joulie installed with
#        configs/tier-b-common.yaml and configs/tier-b-ARM.yaml on the
#        cluster spec tests/simcluster/specs/governor.yaml;
#        all-sustained-gate-on: all-sustained, with the simulator's receiver
#        gate on for every GPU (emulate.receiverGate, SIM_GPU_RECEIVER_GATE)
#        instead of each profile's own, off in every builtin profile;
#        off: Joulie deployed as caps-only, then removed with what it left
#        (as scripts/simcluster/scenario.sh does for joulie.enabled false),
#        the baseline the other arms are compared with
#
# The script creates its own cluster: SIMCLUSTER_NAME (default
# joulie-sim-gov) must match ^joulie-sim(-[a-z0-9]+)*$ and name no container
# that exists yet. Right after up.sh it records the new container's id in
# $SIMCLUSTER_DIR/tier-b.created, and before every step that talks to a
# cluster it refuses to go on unless the kubeconfig it uses is
# $SIMCLUSTER_DIR/kubeconfig, holds exactly one context and one cluster, and
# that cluster's server is the port the recorded container publishes on
# 127.0.0.1. KUBECONFIG and ~/.kube are never read. --check-context runs
# that check alone and exits 0 or 1.
#
# Then: deploy.sh with the arm's values, TestSimclusterE2E restricted to the
# GS checks (they skip themselves unless GOVERNOR_CONTROL is on; the off arm
# has no Joulie to check and runs none), a copy of the NodeTwins and
# NodeHardware into the run directory
# (${XDG_CACHE_HOME:-~/.cache}/joulie/experiments/05/<UTC>-tier-b-ARM), and
# down.sh, unless SIMCLUSTER_KEEP=1, which keeps the cluster for a workload
# run. Exits with the test's status.
#
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
exp=$(dirname "$here")
export SIMCLUSTER_NAME=${SIMCLUSTER_NAME:-joulie-sim-gov}
export SIMCLUSTER_SPEC=${SIMCLUSTER_SPEC:-tests/simcluster/specs/governor.yaml}
# shellcheck source=../../../scripts/simcluster/lib.sh
source "$exp/../../scripts/simcluster/lib.sh"

check_only=0
arm=
case ${1:-} in
  -h | --help)
    sim_usage "$0"
    exit 0
    ;;
  --check-context) check_only=1 ;;
  off | caps-only | cpu | gpu | all | all-sustained | all-sustained-gate-on) arm=$1 ;;
  "") sim_die "name an arm: off, caps-only, cpu, gpu, all, all-sustained or all-sustained-gate-on (see --help)" ;;
  *) sim_die "unknown arm $1: off, caps-only, cpu, gpu, all, all-sustained or all-sustained-gate-on (see --help)" ;;
esac
# The Joulie values file of the arm; off deploys caps-only before removing
# Joulie, so deploy.sh's waits for NodeHardware and NodeTwins hold.
case $arm in
  off) joulie_arm=caps-only ;;
  all-sustained-gate-on) joulie_arm=all-sustained ;;
  *) joulie_arm=$arm ;;
esac
sim_init
marker=$SIMCLUSTER_DIR/tier-b.created

# gov_check_context refuses any cluster but the one this script created.
gov_check_context() {
  local why= id= port= servers contexts server
  if [[ $KUBECONFIG != "$SIMCLUSTER_DIR/kubeconfig" ]]; then
    why="KUBECONFIG is $KUBECONFIG, not $SIMCLUSTER_DIR/kubeconfig"
  elif [[ ! -s $marker ]]; then
    why="$marker is missing: this script did not create $SIMCLUSTER_NAME"
  elif [[ ! -s $KUBECONFIG ]]; then
    why="$KUBECONFIG is missing"
  else
    id=$(docker inspect -f '{{.Id}}' "$SIMCLUSTER_NAME" 2>/dev/null || true)
    port=$(docker port "$SIMCLUSTER_NAME" 6443/tcp 2>/dev/null | head -n1 || true)
    port=${port##*:}
    servers=$(grep -cE '^[[:space:]]*server:' "$KUBECONFIG" || true)
    contexts=$(grep -cE '^[[:space:]]*-?[[:space:]]*context:' "$KUBECONFIG" || true)
    server=$(awk '$1 == "server:" { print $2; exit }' "$KUBECONFIG")
    if [[ -z $id || $id != "$(cat "$marker")" ]]; then
      why="container $SIMCLUSTER_NAME is not the one this script created (id ${id:-none}, recorded $(cat "$marker"))"
    elif [[ $servers != 1 || $contexts != 1 ]]; then
      why="$KUBECONFIG holds $servers clusters and $contexts contexts, not exactly one of each"
    elif [[ ! $port =~ ^[0-9]+$ || $server != "https://127.0.0.1:$port" ]]; then
      why="the kubeconfig's server ${server:-none} is not the container's published port 127.0.0.1:${port:-none}"
    fi
  fi
  if [[ -n $why ]]; then
    sim_log "refusing: $why"
    return 1
  fi
}

if ((check_only)); then
  sim_require docker
  gov_check_context
  exit
fi

sim_require docker kubectl helm jq
[[ -z $(sim_container_state) ]] ||
  sim_die "a container named $SIMCLUSTER_NAME exists already; Tier B runs on a cluster it creates (remove it with make simcluster-down SIMCLUSTER_NAME=$SIMCLUSTER_NAME)"
run_dir=${XDG_CACHE_HOME:-$HOME/.cache}/joulie/experiments/05/$(date -u +%Y%m%dT%H%M%SZ)-tier-b-$arm
mkdir -p "$run_dir"
exec > >(tee -a "$run_dir/tier-b.log") 2>&1
rm -f "$marker"
started=0

finish() {
  local rc=$?
  set +e
  if gov_check_context; then
    kubectl get nodetwins,nodehardwares -o yaml >"$run_dir/governor-objects.yaml" 2>/dev/null
  fi
  # Whatever exists under the name since up.sh started is this run's: the
  # name was free before it.
  if [[ ${SIMCLUSTER_KEEP:-0} == 1 ]]; then
    sim_log "keeping cluster $SIMCLUSTER_NAME (SIMCLUSTER_KEEP=1)"
  elif ((started)); then
    bash "$SIM_SCRIPTS/down.sh"
    rm -f "$marker"
  fi
  sim_log "Tier B arm $arm finished with status $rc; results in $run_dir"
  exit "$rc"
}
trap finish EXIT

no_joulie_pods() {
  [[ -z $(kubectl -n "$SIM_NS_JOULIE" get pods -o name) ]]
}

# remove_joulie takes Joulie off the cluster as scenario.sh's disable_joulie
# does: the release, its NodeTwins and NodeHardwares, the power-profile
# labels, and every limit its agents wrote, which the hwemu trees keep across
# restarts, by rendering the spec's trees again with the simulator stopped.
remove_joulie() {
  local labelled nodes
  sim_log "arm off: removing Joulie, its objects and the limits its agents wrote"
  helm uninstall "$SIM_RELEASE_JOULIE" -n "$SIM_NS_JOULIE" >/dev/null
  sim_wait 300 "no Joulie pod" no_joulie_pods
  kubectl delete nodetwins --all --wait=true >/dev/null
  kubectl delete nodehardwares --all --wait=true >/dev/null
  labelled=$(kubectl get nodes -l joulie.io/power-profile -o name)
  if [[ -n $labelled ]]; then
    # shellcheck disable=SC2086
    kubectl label $labelled joulie.io/power-profile- >/dev/null
  fi
  kubectl -n "$SIM_NS_SIM" scale "deployment/$SIM_SIM_DEPLOYMENT" --replicas=0 >/dev/null
  sim_wait 300 "the simulator pod to stop" sim_none kubectl -n "$SIM_NS_SIM" get pods -o name
  nodes=$(sed -n 's#^node/##p' "$SIMCLUSTER_DIR/render/nodes.names")
  # shellcheck disable=SC2086
  docker exec "$SIMCLUSTER_NAME" sh -c 'root=$1; shift; for n in "$@"; do rm -rf "${root:?}/$n"; done' sh "$SIM_HWEMU_ROOT" $nodes
  kubectl -n "$SIM_NS_SIM" scale "deployment/$SIM_SIM_DEPLOYMENT" --replicas=1 >/dev/null
  kubectl -n "$SIM_NS_SIM" rollout status "deployment/$SIM_SIM_DEPLOYMENT" --timeout=300s >/dev/null
  sim_wait 300 "the simulator's /readyz" sim_sim readyz
}

if [[ $arm == all-sustained-gate-on ]]; then
  # A simulator values file, after the generated ones (deploy.sh), so the
  # gate holds on every GPU profile with the profile's fractions.
  printf 'emulate:\n  receiverGate: "on"\n' >"$run_dir/simulator-receiver-gate-on.yaml"
  export SIMCLUSTER_SIM_EXTRA_VALUES="${SIMCLUSTER_SIM_EXTRA_VALUES:+$SIMCLUSTER_SIM_EXTRA_VALUES }$run_dir/simulator-receiver-gate-on.yaml"
fi

started=1
bash "$SIM_SCRIPTS/up.sh"
docker inspect -f '{{.Id}}' "$SIMCLUSTER_NAME" >"$marker"
gov_check_context
bash "$SIM_SCRIPTS/deploy.sh" -f "$exp/configs/tier-b-common.yaml" -f "$exp/configs/tier-b-$joulie_arm.yaml"
gov_check_context
if [[ $arm == off ]]; then
  remove_joulie
  gov_check_context
  sim_log "arm off: Joulie is removed; the governor checks have nothing to check"
  exit 0
fi
sim_log "running the governor checks"
(cd "$SIM_REPO" && SIMCLUSTER_RUN_DIR=$run_dir "$GO" test -tags simcluster -count=1 -timeout 30m ./tests/simcluster/ -run 'TestSimclusterE2E/GS0[1-4]' -v)
