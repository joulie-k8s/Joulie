#!/usr/bin/env bash
#
# smoke.sh - create a simulated cluster from the smoke spec, deploy, run the
# end-to-end test and collect the results.
#
# Usage: scripts/simcluster/smoke.sh
#
# Runs up.sh, deploy.sh -f tests/simcluster/smoke-values.yaml and
# TestSimclusterE2E with SIMCLUSTER_RUN_DIR set to
# $SIMCLUSTER_DIR/runs/<UTC>-<spec>-<mode>-<sha12>[-dirty], and a test
# timeout of ten rate windows of the timing, at least 20 minutes (20 m in
# dev timing, 50 m in production): the checks wait on the rate window, up to
# three of them for a NodeTwin to follow a change of power. While up.sh
# creates the cluster, the host tools are built once for every script, the
# test is compiled (go test -c -tags simcluster) and the Joulie images are
# built and imported as soon as the container runs (images.sh
# --wait-container), so deploy.sh skips images.sh. Then collects the
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
# SIMCLUSTER_SPEC=tests/simcluster/specs/smoke.yaml; SIMCLUSTER_TEST_TIMEOUT,
# a Go duration, replaces the test timeout; every other setting as for up.sh
# and deploy.sh.
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
# The test timeout, from the timing sim_init read.
test_timeout=${SIMCLUSTER_TEST_TIMEOUT:-}
if [[ -z $test_timeout ]]; then
  test_timeout=$((10 * $(sim_seconds "$RATE_WINDOW")))
  ((test_timeout >= 1200)) || test_timeout=1200
  test_timeout=${test_timeout}s
fi
runs=$SIMCLUSTER_DIR/runs
run_name=$(date -u +%Y%m%dT%H%M%SZ)-$spec_name-$SIMCLUSTER_TELEMETRY-$(sim_git_state)
run_dir=$runs/$run_name
mkdir -p "$run_dir"
exec > >(tee -a "$run_dir/smoke.log") 2>&1

# collect copies what explains the run into the run directory; it never
# fails the run. The container's and containerd's logs come first, from a
# stopped container too, since a k3s that exited explains everything after.
collect() {
  local k=$run_dir/k8s l=$run_dir/logs res ns pod
  mkdir -p "$k" "$l"
  sim_save_container_logs "$l"
  [[ -s $KUBECONFIG ]] || return 0
  for res in nodes nodehardwares nodetwins pods events; do
    kubectl get "$res" -A -o yaml >"$k/$res-end.yaml" 2>/dev/null || true
  done
  local pids=()
  for ns in "$SIM_NS_JOULIE" "$SIM_NS_SIM" "$SIM_NS_MONITORING"; do
    for pod in $(kubectl -n "$ns" get pods -o name 2>/dev/null || true); do
      kubectl -n "$ns" logs "$pod" --all-containers --prefix >"$l/$ns.${pod#pod/}.log" 2>&1 &
      pids+=($!)
    done
  done
  # Wait only for these jobs: with no PID, bash 5.1 and later also wait for
  # the process substitution of the log tee, which never exits.
  if ((${#pids[@]})); then wait "${pids[@]}" || true; fi
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

# shellcheck disable=SC2317 # called by the EXIT trap
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

# The host tools once, for every script below; the test binary and the
# Joulie images while up.sh creates the cluster: the images go in as soon as
# the container runs.
sim_host_tools
test_bin=$SIM_HOST_BIN/simcluster.test
(cd "$SIM_REPO" && "$GO" test -c -tags simcluster -o "$test_bin" ./tests/simcluster/) &
compile=$!
bash "$SIM_SCRIPTS/images.sh" --joulie --wait-container &
joulie_images=$!
up=0
bash "$SIM_SCRIPTS/up.sh" || up=$?
if ((up != 0)); then
  kill "$joulie_images" "$compile" 2>/dev/null || true
  exit "$up"
fi
wait "$joulie_images" || sim_die "building and importing the Joulie images failed"
bash "$SIM_SCRIPTS/deploy.sh" --skip-images -f "$SIM_REPO/tests/simcluster/smoke-values.yaml"
wait "$compile" || sim_die "compiling the end-to-end test failed"
sim_log "running TestSimclusterE2E (timeout $test_timeout)"
rc=0
# The test reads its package's files by relative path, as go test runs it.
(cd "$SIM_REPO/tests/simcluster" && SIMCLUSTER_RUN_DIR=$run_dir "$test_bin" -test.count=1 -test.timeout "$test_timeout" -test.run TestSimclusterE2E -test.v) || rc=$?
if [[ ${SIMCLUSTER_SCENARIOS:-0} == 1 ]]; then
  collect_once || true
  sim_log "running the workload scenario smoke on the same cluster (SIMCLUSTER_SCENARIOS=1)"
  src=0
  SCENARIO_RUN_DIR=$run_dir/scenario-smoke bash "$SIM_SCRIPTS/scenario.sh" "$SIM_REPO/tests/simcluster/scenarios/smoke.yaml" || src=$?
  ((rc)) || rc=$src
fi
exit "$rc"
