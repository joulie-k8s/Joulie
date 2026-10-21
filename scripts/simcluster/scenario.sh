#!/usr/bin/env bash
#
# scenario.sh - run a workload scenario on the simulated cluster and evaluate
# it.
#
# Usage: scripts/simcluster/scenario.sh <scenario.yaml>
#
# Builds simscenario and workloadgen for the host, validates the scenario
# (tests/simcluster/scenarios/<name>.yaml) and plans its run directory
# $SIMCLUSTER_DIR/runs/<UTC>-<name>-<sha12>[-dirty]: the effective scenario,
# and per variant its trace and variant.env. Then, per variant:
#
#   1. the cluster: down.sh first when the scenario isolates its variants (it
#      has several, or sets isolation: cluster), then up.sh, which reuses a
#      running cluster and switches it to the variant's spec;
#   2. deploy.sh with the variant's spec, timing, telemetry and Joulie values;
#      joulie.set becomes a Joulie values file and simulator.set a simulator
#      one (SIMCLUSTER_SIM_EXTRA_VALUES), each written by helm from the --set
#      arguments, so they mean what --set means;
#   3. with joulie.enabled false: Joulie is uninstalled, its NodeTwins and
#      NodeHardwares deleted, the power-profile labels removed, and once no
#      Joulie pod is left the hwemu trees are rendered again, so no limit an
#      agent wrote during the deploy outlives it;
#   4. the run's namespaces are created afresh and the trace is copied into
#      the simulator's run directory <SIM_HWEMU_ROOT>/_runs/<run id>/;
#   5. simscenario run, which drives the run through the API server; then the
#      simulator's otlp/ files are copied out, and simscenario evaluate writes
#      the variant's summary.json.
#
# Last, simscenario summarize writes the run's summary.json and the
# comparisons, and runs/latest points at the run directory. The cluster is
# kept; make simcluster-down deletes it. The run directory also gets
# scenario.log, everything this script printed.
#
# Settings (environment), besides those of up.sh and deploy.sh, which each
# variant's variant.env overrides for the spec, timing, telemetry and scope:
#   SCENARIO_VARIANTS         run only these variants, space separated; the
#                             run is then not summarized, so a later
#                             simscenario summarize can cover every variant
#   SCENARIO_RUN_DIR          the run directory, instead of a new one under
#                             $SIMCLUSTER_DIR/runs; runs/latest is then left
#                             alone
#   SIMCLUSTER_VALIDATE_OTLP  1: POST every OTLP line of each variant to an
#                             OpenTelemetry Collector ($OTELCOL_IMAGE pinned
#                             to $OTELCOL_DIGEST, OTLP receiver and file
#                             exporter, in Docker) and require HTTP 200, no
#                             partialSuccess, and as many data points and
#                             log records in its output as were sent
#
# Exits with simscenario's code: 0 when every assertion of severity fail
# passed, 1 when one failed, 2 when the scenario is invalid or a variant
# could not be run.
#
set -euo pipefail
# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
case ${1:-} in
  -h | --help)
    sim_usage "$0"
    exit 0
    ;;
  "") sim_die "no scenario file given (see --help)" ;;
esac
(($# == 1)) || sim_die "one scenario file, got $# arguments (see --help)"
sim_init
sim_require docker kubectl helm jq
[[ -n $GO ]] || sim_die "no Go toolchain found: install Go or set GO"
scenario_file=$(sim_path "$1")
scenario_name=$(basename "$scenario_file" .yaml)

# The run directory, named as scenario.RunDirName names it.
runs=$SIMCLUSTER_DIR/runs
link_latest=0
if [[ -n ${SCENARIO_RUN_DIR:-} ]]; then
  run_dir=$SCENARIO_RUN_DIR
  [[ $run_dir == /* ]] || run_dir=$PWD/$run_dir
else
  run_dir=$runs/$(date -u +%Y%m%dT%H%M%SZ)-$scenario_name-$(sim_git_state)
  link_latest=1
fi
mkdir -p "$run_dir"
exec > >(tee -a "$run_dir/scenario.log") 2>&1
work=$(mktemp -d)
otlpcheck=$SIMCLUSTER_NAME-otlpcheck
# shellcheck disable=SC2317 # called by the EXIT trap
cleanup() {
  local rc=$?
  set +e
  rm -rf "$work"
  docker rm -f "$otlpcheck" >/dev/null 2>&1
  exit "$rc"
}
trap cleanup EXIT

# 1. The host tools, the validation and the plan. simcluster and
# clusterspec are built once here, for every up.sh and deploy.sh below.
sim_host_tools
mkdir -p "$SIM_HOST_BIN"
(cd "$SIM_REPO" && "$GO" build -trimpath -o "$SIM_HOST_BIN/" ./simulator/cmd/simscenario ./simulator/cmd/workloadgen) ||
  sim_die "building the host tools (simscenario, workloadgen) failed"
simscenario() { "$SIM_HOST_BIN/simscenario" "$@"; }
simscenario validate "$scenario_file" -repo "$SIM_REPO"
simscenario plan "$scenario_file" -repo "$SIM_REPO" -out "$run_dir" -workloadgen "$SIM_HOST_BIN/workloadgen" | tee "$work/plan.txt"
mapfile -t planned < <(awk '/^variant / { v = $2; sub(/:$/, "", v); print v }' "$work/plan.txt")
((${#planned[@]})) || sim_die "simscenario plan listed no variant"
variants=()
for v in "${planned[@]}"; do
  if [[ -z ${SCENARIO_VARIANTS:-} ]] || [[ " $SCENARIO_VARIANTS " == *" $v "* ]]; then
    variants+=("$v")
  fi
done
((${#variants[@]})) || sim_die "SCENARIO_VARIANTS=$SCENARIO_VARIANTS names none of the variants: ${planned[*]}"

# set_values <out> <key=value>... writes Helm --set arguments as a values
# file. helm renders it from the arguments, so the file means exactly what
# --set means: the same types, list syntax and escapes.
set_values() {
  local out=$1 chart=$work/setvalues
  shift
  mkdir -p "$chart/templates"
  printf 'apiVersion: v2\nname: setvalues\nversion: 0.0.0\n' >"$chart/Chart.yaml"
  printf '{{ toYaml .Values }}\n' >"$chart/templates/values.yaml"
  local args=() kv
  for kv in "$@"; do args+=(--set "$kv"); done
  helm template setvalues "$chart" "${args[@]}" --show-only templates/values.yaml | sed '/^---$/d; /^# Source: /d' >"$out"
}

# workload_namespaces <variant dir> prints the namespaces the run's pods go
# to: the scenario's workload.namespace when it sets one, else those of the
# trace's job records.
workload_namespaces() {
  local ns
  ns=$(awk '/^[^ ]/ { w = ($0 == "workload:") } w && /^  namespace: / { sub(/^  namespace: /, ""); gsub(/["'\'']/, ""); print; exit }' "$run_dir/scenario.effective.yaml")
  if [[ -n $ns ]]; then
    printf '%s\n' "$ns"
  else
    jq -r 'select(.type == "job") | .namespace // "default"' "$1/trace.jsonl" | sort -u
  fi
}

# recreate_namespaces <variant dir> deletes and creates the run's
# namespaces, so no object of an earlier run is adopted. default and the
# system namespaces cannot be deleted; they are only checked to exist.
recreate_namespaces() {
  local ns
  while IFS= read -r ns; do
    [[ -n $ns ]] || continue
    case $ns in
      default | kube-* | "$SIM_NS_JOULIE" | "$SIM_NS_SIM" | "$SIM_NS_MONITORING") ;;
      *) kubectl delete namespace "$ns" --ignore-not-found --wait=true --timeout=300s >/dev/null ;;
    esac
    kubectl create namespace "$ns" --dry-run=client -o yaml |
      kubectl apply --server-side --field-manager="$SIM_FIELD_MANAGER" -f - >/dev/null
  done < <(workload_namespaces "$1")
}

# shellcheck disable=SC2317 # called by sim_wait
no_joulie_pods() {
  [[ -z $(kubectl -n "$SIM_NS_JOULIE" get pods -o name) ]]
}

# disable_joulie removes Joulie and what it left on the cluster. An agent
# writes limits into its Node's hwemu tree and never restores them, and the
# tree keeps them across restarts; so the simulator is stopped, the trees of
# the spec's Nodes are removed and the simulator renders them again from
# their profiles, with every limit at its default.
disable_joulie() {
  sim_log "joulie.enabled is false: removing Joulie, its objects and the limits its agents wrote"
  helm uninstall "$SIM_RELEASE_JOULIE" -n "$SIM_NS_JOULIE" >/dev/null
  sim_wait 300 "no Joulie pod" no_joulie_pods
  kubectl delete nodetwins --all --wait=true >/dev/null
  kubectl delete nodehardwares --all --wait=true >/dev/null
  local labelled
  labelled=$(kubectl get nodes -l joulie.io/power-profile -o name)
  if [[ -n $labelled ]]; then
    # shellcheck disable=SC2086
    kubectl label $labelled joulie.io/power-profile- >/dev/null
  fi
  kubectl -n "$SIM_NS_SIM" scale "deployment/$SIM_SIM_DEPLOYMENT" --replicas=0 >/dev/null
  sim_wait 300 "the simulator pod to stop" sim_none kubectl -n "$SIM_NS_SIM" get pods -o name
  local nodes
  nodes=$(sed -n 's#^node/##p' "$SIMCLUSTER_DIR/render/nodes.names")
  # shellcheck disable=SC2086
  docker exec "$SIMCLUSTER_NAME" sh -c 'root=$1; shift; for n in "$@"; do rm -rf "${root:?}/$n"; done' sh "$SIM_HWEMU_ROOT" $nodes
  kubectl -n "$SIM_NS_SIM" scale "deployment/$SIM_SIM_DEPLOYMENT" --replicas=1 >/dev/null
  sim_rollouts "$SIM_NS_SIM" "deployment/$SIM_SIM_DEPLOYMENT"
  sim_wait 300 "the simulator's /readyz" sim_sim readyz
}

# OTLP validation (SIMCLUSTER_VALIDATE_OTLP=1): one Collector for the
# whole scenario, started before the first variant so that every variant's
# subshell posts to it, its output in $work/otlpcheck. Pulled by digest, as
# images.sh pulls the third-party images.
otlp_port=
start_otlpcheck() {
  local dir=$work/otlpcheck
  mkdir -p "$dir/out"
  cat >"$dir/config.yaml" <<'EOF'
receivers:
  otlp:
    protocols:
      http:
        endpoint: 0.0.0.0:4318
exporters:
  file/metrics:
    path: /out/metrics.jsonl
    flush_interval: 100ms
  file/logs:
    path: /out/logs.jsonl
    flush_interval: 100ms
service:
  pipelines:
    metrics: {receivers: [otlp], exporters: [file/metrics]}
    logs: {receivers: [otlp], exporters: [file/logs]}
EOF
  local image=${OTELCOL_IMAGE%:*}@$OTELCOL_DIGEST
  sim_log "starting the OTLP check Collector ($OTELCOL_IMAGE, $OTELCOL_DIGEST)"
  docker rm -f "$otlpcheck" >/dev/null 2>&1 || true
  docker run -d --name "$otlpcheck" --user "$(id -u):$(id -g)" -p 127.0.0.1::4318 \
    -v "$dir/config.yaml:/etc/otelcol/config.yaml:ro" -v "$dir/out:/out" \
    "$image" --config=/etc/otelcol/config.yaml >/dev/null
  otlp_port=$(docker port "$otlpcheck" 4318/tcp | head -n1)
  otlp_port=${otlp_port##*:}
  [[ $otlp_port =~ ^[0-9]+$ ]] || sim_die "the OTLP check Collector publishes no port"
  sim_wait 60 "the OTLP check Collector" curl -fsS -o /dev/null -X POST -H 'Content-Type: application/json' \
    --data '{}' "http://127.0.0.1:$otlp_port/v1/metrics"
}

# otlp_count <metrics|logs> <file> prints the data points or log records
# of an OTLP JSON Lines file, 0 for a file that does not exist yet.
otlp_count() {
  if [[ ! -e $2 ]]; then
    echo 0
  elif [[ $1 == metrics ]]; then
    jq -s '[.[] | .resourceMetrics[]?.scopeMetrics[]?.metrics[]? | (.gauge // .sum // .histogram // .exponentialHistogram // .summary // {}) | (.dataPoints // []) | length] | add // 0' "$2"
  else
    jq -s '[.[] | .resourceLogs[]?.scopeLogs[]? | (.logRecords // []) | length] | add // 0' "$2"
  fi
}

# validate_otlp <variant dir> posts every line of its OTLP files and checks
# the answers and what the Collector wrote.
validate_otlp() {
  local vdir=$1 out=$work/otlpcheck/out f base signal code bad=0 sent_m=0 sent_l=0 n line
  [[ -n $otlp_port ]] || start_otlpcheck
  # The Collector appends: what it wrote for an earlier variant is counted
  # first.
  local had_m had_l
  had_m=$(otlp_count metrics "$out/metrics.jsonl")
  had_l=$(otlp_count logs "$out/logs.jsonl")
  for f in "$vdir"/otlp/*.jsonl; do
    [[ -e $f ]] || continue
    case $f in
      *.metrics.jsonl) signal=metrics ;;
      *.logs.jsonl) signal=logs ;;
      *) continue ;;
    esac
    n=0
    base=$(basename "$f")
    while IFS= read -r line || [[ -n $line ]]; do
      n=$((n + 1))
      [[ -n $line ]] || continue
      printf '%s' "$line" >"$work/line.json"
      code=$(curl -sS -o "$work/answer.json" -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
        --data-binary "@$work/line.json" "http://127.0.0.1:$otlp_port/v1/$signal") || code=000
      if [[ $code != 200 ]]; then
        sim_log "OTLP check: $base line $n: HTTP $code: $(head -c 300 "$work/answer.json")"
        bad=1
      elif ! jq -e '(.partialSuccess // {}) | ((.rejectedDataPoints // .rejectedLogRecords // "0") | tonumber) == 0 and ((.errorMessage // "") == "")' "$work/answer.json" >/dev/null 2>&1; then
        sim_log "OTLP check: $base line $n: partial success: $(head -c 300 "$work/answer.json")"
        bad=1
      fi
    done <"$f"
    if [[ $signal == metrics ]]; then
      sent_m=$((sent_m + $(otlp_count metrics "$f")))
    else
      sent_l=$((sent_l + $(otlp_count logs "$f")))
    fi
  done
  # shellcheck disable=SC2317 # called by sim_wait
  otlp_received() {
    [[ $(otlp_count metrics "$out/metrics.jsonl") == $((had_m + sent_m)) && $(otlp_count logs "$out/logs.jsonl") == $((had_l + sent_l)) ]]
  }
  if ! sim_wait 60 "the Collector to write $sent_m data points and $sent_l log records" otlp_received; then
    sim_log "OTLP check: the Collector wrote $(($(otlp_count metrics "$out/metrics.jsonl") - had_m)) data points and $(($(otlp_count logs "$out/logs.jsonl") - had_l)) log records"
    bad=1
  fi
  if ((bad)); then
    sim_log "OTLP check of $vdir failed"
    return 1
  fi
  sim_log "OTLP check of $vdir: every line accepted, $sent_m data points and $sent_l log records written back"
}

# variant_dir <name> prints the directory of a variant: the run directory
# itself for the unnamed one.
variant_dir() {
  if [[ -n $1 ]]; then printf '%s/%s\n' "$run_dir" "$1"; else printf '%s\n' "$run_dir"; fi
}

# run_variant <name> runs one variant and returns what simscenario evaluate
# returned, after touching $work/evaluated. It runs in a subshell with -e,
# so any step that fails ends it before the evaluation.
run_variant() {
  local name=$1 vdir
  vdir=$(variant_dir "$name")
  [[ -s $vdir/variant.env ]] || sim_die "$vdir/variant.env is missing"
  local SCENARIO_RUN_ID SCENARIO_ISOLATION JOULIE_ENABLED JOULIE_SET JOULIE_VALUES SIM_SET
  # shellcheck source=/dev/null
  source "$vdir/variant.env"
  export SIMCLUSTER_SPEC SIMCLUSTER_TIMING SIMCLUSTER_TELEMETRY SIMCLUSTER_POWER_SCOPE
  sim_log "variant ${name:-(unnamed)}: spec $SIMCLUSTER_SPEC, timing $SIMCLUSTER_TIMING, telemetry $SIMCLUSTER_TELEMETRY, run id $SCENARIO_RUN_ID"

  # 1. The cluster.
  if [[ $SCENARIO_ISOLATION == cluster ]]; then
    bash "$SIM_SCRIPTS/down.sh"
  fi
  bash "$SIM_SCRIPTS/up.sh"

  # 2. The simulator and Joulie.
  local deploy=() f
  for f in "${JOULIE_VALUES[@]}"; do deploy+=(-f "$(sim_path "$f")"); done
  if ((${#JOULIE_SET[@]})); then
    set_values "$vdir/joulie-set.yaml" "${JOULIE_SET[@]}"
    deploy+=(-f "$vdir/joulie-set.yaml")
  fi
  local sim_extra=
  if ((${#SIM_SET[@]})); then
    set_values "$vdir/simulator-set.yaml" "${SIM_SET[@]}"
    sim_extra=$vdir/simulator-set.yaml
  fi
  SIMCLUSTER_SIM_EXTRA_VALUES=$sim_extra bash "$SIM_SCRIPTS/deploy.sh" "${deploy[@]}"

  # 3. Without Joulie.
  if [[ $JOULIE_ENABLED != true ]]; then
    disable_joulie
  fi
  cp "$SIMCLUSTER_DIR/images.env" "$vdir/images.env"
  if [[ $JOULIE_ENABLED == true ]]; then
    helm get values "$SIM_RELEASE_JOULIE" -n "$SIM_NS_JOULIE" -o yaml >"$vdir/values.effective.yaml"
  else
    printf '# joulie.enabled is false: Joulie was uninstalled before the run.\n{}\n' >"$vdir/values.effective.yaml"
  fi
  helm get values "$SIM_RELEASE_SIM" -n "$SIM_NS_SIM" -o yaml >"$vdir/values.simulator.effective.yaml"

  # 4. The namespaces and the trace.
  recreate_namespaces "$vdir"
  local sim_dir=$SIM_HWEMU_ROOT/_runs/$SCENARIO_RUN_ID
  docker exec -i "$SIMCLUSTER_NAME" sh -c 'mkdir -p "$1" && cat >"$1/trace.jsonl"' sh "$sim_dir" <"$vdir/trace.jsonl"

  # 5. The run, its files, the evaluation.
  local rc=0
  simscenario run -variant-dir "$vdir" -kubeconfig "$KUBECONFIG" || rc=$?
  if docker exec "$SIMCLUSTER_NAME" test -d "$sim_dir/otlp"; then
    docker exec "$SIMCLUSTER_NAME" tar -C "$sim_dir" -cf - otlp | tar -C "$vdir" -xf -
  else
    sim_log "the simulator wrote no $sim_dir/otlp"
  fi
  local erc=0
  simscenario evaluate "$vdir" || erc=$?
  : >"$work/evaluated"
  ((rc == 0)) || erc=$scenario_exit_error
  if [[ ${SIMCLUSTER_VALIDATE_OTLP:-0} == 1 ]] && ! validate_otlp "$vdir"; then
    ((erc)) || erc=1
  fi
  return "$erc"
}

# simscenario's exit code for a variant that could not be run or evaluated,
# scenario.ExitError; TestSC19ScenarioExitErrorIsTheLibrarys holds them equal.
scenario_exit_error=2
if [[ ${SIMCLUSTER_VALIDATE_OTLP:-0} == 1 ]]; then
  sim_require curl
  start_otlpcheck
fi
worst=0
for v in "${variants[@]}"; do
  [[ $v == "(unnamed)" ]] && v=
  rm -f "$work/evaluated"
  # Not in an || list: there bash ignores -e inside the function.
  set +e
  (
    set -e
    run_variant "$v"
  )
  vrc=$?
  set -e
  if [[ ! -e $work/evaluated ]]; then
    sim_log "variant ${v:-(unnamed)} could not be run (status $vrc); writing its summary as aborted, with the cluster's logs in $(variant_dir "$v")/logs"
    sim_save_container_logs "$(variant_dir "$v")/logs"
    simscenario evaluate "$(variant_dir "$v")" >/dev/null || true
    vrc=$scenario_exit_error
  fi
  sim_log "variant ${v:-(unnamed)} finished with status $vrc"
  ((vrc > worst)) && worst=$vrc
done

if ((link_latest)); then
  ln -sfn "$(basename "$run_dir")" "$runs/latest"
fi
if [[ -n ${SCENARIO_VARIANTS:-} ]]; then
  sim_log "ran only the variants ${variants[*]}; summarize later with: simscenario summarize $run_dir"
  exit "$worst"
fi
rc=0
simscenario summarize "$run_dir" || rc=$?
((worst == scenario_exit_error)) && rc=$scenario_exit_error
sim_log "scenario $scenario_name finished with status $rc; results in $run_dir"
exit "$rc"
