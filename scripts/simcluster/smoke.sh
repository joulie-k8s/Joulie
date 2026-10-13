#!/usr/bin/env bash
#
# smoke.sh - create a simulated cluster from the smoke spec, deploy, run the
# end-to-end test and collect the results.
#
# Usage: scripts/simcluster/smoke.sh
#
# Runs up.sh, deploy.sh -f tests/simcluster/smoke-values.yaml and
# TestSimclusterE2E (go test -tags simcluster) with SIMCLUSTER_RUN_DIR set to
# $SIMCLUSTER_DIR/runs/<UTC>-<spec>-<mode>-<sha12>[-dirty]. Then collects the
# Nodes, NodeHardware and NodeTwin objects, the pods and events, the logs,
# images.env and the Helm values into the run directory, points runs/latest
# at it and deletes the cluster, unless SIMCLUSTER_KEEP=1. The run directory
# also gets smoke.log, everything this script printed. With
# SIMCLUSTER_SCENARIOS=1, the workload scenario
# tests/simcluster/scenarios/smoke.yaml runs on the same cluster after the
# collection and before the cluster is deleted (scenario.sh), into the run
# directory's scenario-smoke/. Exits with the test's status, else the
# scenario's, or 1 when the cluster could not be set up.
#
# Defaults: SIMCLUSTER_NAME=joulie-sim-smoke,
# SIMCLUSTER_SPEC=tests/simcluster/specs/smoke.yaml; every other setting as
# for up.sh and deploy.sh.
#
set -euo pipefail
export SIMCLUSTER_NAME=${SIMCLUSTER_NAME:-joulie-sim-smoke}
export SIMCLUSTER_SPEC=${SIMCLUSTER_SPEC:-tests/simcluster/specs/smoke.yaml}
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
sim_require docker kubectl helm jq
[[ -n $GO ]] || sim_die "no Go toolchain found: install Go or set GO"

if [[ -n ${SIMCLUSTER_NODES:-} ]]; then
  spec_name=scale-$SIMCLUSTER_NODES
else
  spec_name=$(basename "$SIMCLUSTER_SPEC" .yaml)
fi
# Exactly 12 characters, as scenario.RunDirName writes them (--short=12 may
# print more to stay unambiguous).
sha=$(git -C "$SIM_REPO" rev-parse HEAD 2>/dev/null | cut -c1-12) || true
sha=${sha:-nogit}
dirty=
if [[ $sha != nogit && -n $(git -C "$SIM_REPO" status --porcelain 2>/dev/null) ]]; then
  dirty=-dirty
fi
runs=$SIMCLUSTER_DIR/runs
run_name=$(date -u +%Y%m%dT%H%M%SZ)-$spec_name-$SIMCLUSTER_TELEMETRY-$sha$dirty
run_dir=$runs/$run_name
mkdir -p "$run_dir"
exec > >(tee -a "$run_dir/smoke.log") 2>&1

# collect copies what explains the run into the run directory; it never
# fails the run.
collect() {
  local k=$run_dir/k8s l=$run_dir/logs res ns pod
  mkdir -p "$k" "$l"
  if [[ $(sim_container_state) == true ]]; then
    docker logs --tail 2000 "$SIMCLUSTER_NAME" >"$l/k3s.log" 2>&1 || true
  fi
  [[ -s $KUBECONFIG ]] || return 0
  for res in nodes nodehardwares nodetwins pods events; do
    kubectl get "$res" -A -o yaml >"$k/$res-end.yaml" 2>/dev/null || true
  done
  for ns in "$SIM_NS_JOULIE" "$SIM_NS_SIM" "$SIM_NS_MONITORING"; do
    for pod in $(kubectl -n "$ns" get pods -o name 2>/dev/null || true); do
      kubectl -n "$ns" logs "$pod" --all-containers --prefix >"$l/$ns.${pod#pod/}.log" 2>&1 || true
    done
  done
  helm get values "$SIM_RELEASE_JOULIE" -n "$SIM_NS_JOULIE" -o yaml >"$run_dir/values.effective.yaml" 2>/dev/null || true
  helm get values "$SIM_RELEASE_SIM" -n "$SIM_NS_SIM" -o yaml >"$run_dir/values.simulator.effective.yaml" 2>/dev/null || true
}

# collect_once collects the end-to-end run's results, and the env files of
# its cluster, once: before the workload scenario redeploys the cluster
# (SIMCLUSTER_SCENARIOS=1), else at the end.
collected=0
collect_once() {
  ((collected)) && return 0
  collected=1
  sim_log "collecting the results into $run_dir"
  collect
  local f
  for f in images.env deploy.env cluster.env; do
    if [[ -s $SIMCLUSTER_DIR/$f ]]; then cp "$SIMCLUSTER_DIR/$f" "$run_dir/$f"; fi
  done
}

finish() {
  local rc=$?
  set +e
  collect_once
  ln -sfn "$run_name" "$runs/latest"
  if [[ ${SIMCLUSTER_KEEP:-0} == 1 ]]; then
    sim_log "keeping cluster $SIMCLUSTER_NAME (SIMCLUSTER_KEEP=1); export KUBECONFIG=$KUBECONFIG"
  else
    bash "$SIM_SCRIPTS/down.sh"
  fi
  sim_log "smoke run $run_name finished with status $rc"
  exit "$rc"
}
trap finish EXIT

bash "$SIM_SCRIPTS/up.sh"
bash "$SIM_SCRIPTS/deploy.sh" -f "$SIM_REPO/tests/simcluster/smoke-values.yaml"
sim_log "running TestSimclusterE2E"
rc=0
(cd "$SIM_REPO" && SIMCLUSTER_RUN_DIR=$run_dir "$GO" test -tags simcluster -count=1 -timeout 20m ./tests/simcluster/ -run TestSimclusterE2E -v) || rc=$?
if [[ ${SIMCLUSTER_SCENARIOS:-0} == 1 ]]; then
  collect_once || true
  sim_log "running the workload scenario smoke on the same cluster (SIMCLUSTER_SCENARIOS=1)"
  src=0
  SCENARIO_RUN_DIR=$run_dir/scenario-smoke bash "$SIM_SCRIPTS/scenario.sh" "$SIM_REPO/tests/simcluster/scenarios/smoke.yaml" || src=$?
  ((rc)) || rc=$src
fi
exit "$rc"
