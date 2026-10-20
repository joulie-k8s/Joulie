#!/usr/bin/env bash
#
# hwmeasure.sh - run one real-hardware measurement of experiment 05 on one
# node, in a privileged pod pinned to it, and keep the raw outputs and a
# summary outside the repository.
#
# Usage:
#   hwmeasure.sh list
#   hwmeasure.sh MEASUREMENT --kubeconfig FILE --context NAME --node NAME
#                --device DEV [--peer DEV] [options] [measurement options]
#
# Required: --kubeconfig (refused when its path, or the file it resolves
# to, contains "prod" in any case), --context (must be in that file),
# --node, --device (cpuN or all-cpus for the CPU measurements; a PCI bus id
# such as 0000:3b:00.0, or all where the measurement says so, for the GPU
# ones). The receiver tests also need --peer, the GPU the copies come from.
# KUBECONFIG is never read: every kubectl call names the file and context.
#
# Before anything is created the node is checked: a cordoned node
# (spec.unschedulable, or the node.kubernetes.io/unschedulable taint) is
# refused unless --allow-cordoned; a NoSchedule or NoExecute taint unless
# --allow-taint KEY names it; a pod on the node that no DaemonSet owns and
# that is not a static pod unless an --expect-pod REGEX matches its
# namespace/name, or --allow-other-pods. Then the plan is printed, and the
# run starts only when the node name is typed back (or with --yes).
#
# Options:
#   --namespace NS          namespace of the pod and its ConfigMap (default);
#                           it must admit privileged pods with hostPath
#   --image REF|tools|cuda|rocm
#                           the image: one of images.env, or a reference
#                           with @sha256: (default: the measurement's)
#   --image-pull-secret S   a pull secret for --image
#   --runtime-class NAME    the pod's runtimeClassName (for example nvidia,
#                           when the NVIDIA runtime is not the default)
#   --gpu-request RES=N|none
#                           extended resource to request (default: every
#                           allocatable nvidia.com/gpu or amd.com/gpu of the
#                           node for a GPU measurement, so no other GPU pod
#                           lands there while it runs)
#   --host-root DIR         host directory mounted as the node's root (/);
#                           its sys/ is mounted as /sys. The simulated
#                           cluster keeps an emulated node's tree under
#                           /var/lib/joulie-hwemu/<node> on its infra Node
#   --out DIR               parent of the run directory
#                           (${XDG_CACHE_HOME:-~/.cache}/joulie/experiments/05/hardware)
#   --start-timeout S       seconds to wait for the pod to run (1800)
#   --dry-run               checks and plan only: create nothing
#   --yes                   do not ask for the node name
#
# Measurement options, each accepted only by the measurements that use it
# (hwmeasure.sh list names them): --sample-s N, --settle-s N,
# --measure-s N, --loads P,P, --governors G,G, --lock MIN,MAX, --watts W,
# --arms A,A, --bytes N, --iters N, --issuer src|dst, --cold-s N,
# --load vram|none, --levels L,L, --clock graphics|memory, --load-s N,
# --deinit-s N, --offload-arch A,A, --read-only, --with-lmc.
#
# Results: <out>/<UTC>-<measurement>/ holds plan.txt, pod.yaml, node.json,
# pods.json, run.log, probe.log (the pod log, with every journal line), raw/
# (everything the probe wrote: the baseline snapshot, the journal, the
# restore and verify reports, the samples and tool outputs) and summary.md.
#
# Ctrl-C, TERM or a failure stop the measurement through the probe's own
# trap, which restores what it changed and verifies the restore; this
# script then copies the results and deletes the pod and its ConfigMap.
# Exit status: 0 done, 1 the measurement failed, 2 refused (nothing
# changed), 3 the restore or its verification failed, 130 interrupted.
#
set -euo pipefail
# bash 5.2 makes & in the replacement of ${v//pattern/replacement} the match;
# the template rendering below means it literally.
shopt -u patsub_replacement 2>/dev/null || true
unset KUBECONFIG
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source-path=SCRIPTDIR source=images.env
source "$here/images.env"

say() {
  printf '[hwmeasure %s] %s\n' "$(date -u +%H:%M:%S)" "$*" >&2
  if [[ -n ${run_dir:-} && -d ${run_dir:-} ]]; then
    printf '[hwmeasure %s] %s\n' "$(date -u +%H:%M:%S)" "$*" >>"$run_dir/run.log"
  fi
}

refuse() {
  say "refused: $*"
  exit 2
}

usage() { awk 'NR > 2 && /^#/ { sub(/^# ?/, ""); print; next } NR > 2 { exit }' "$0"; }

# ------------------------------------------------------------ measurements

MEASUREMENTS="hwp-epb amdgpu-cap-range smi-formats nvidia-pl-mig nvidia-pl-na nvidia-lock-floor-idle nvidia-lock-lifetime hopper-receiver-gate l40s-receiver-gate instinct-receiver-gate w7900-level-power fabric-floor cpu-governor-energy schedutil-steps conservative-steps"

# spec MEASUREMENT sets what the plan and the pod need: M_IMAGE (tools,
# cuda, rocm), M_DEV (what --device takes) and M_DEVRE (its pattern),
# M_PEER (1 when --peer is required), M_GPU (nvidia, amd or none), M_OPTS
# (the measurement options it accepts), M_ANSWERS, M_SETTLES, M_CHANGES,
# M_RESTORE.
spec() {
  M_PEER=0 M_GPU=none M_OPTS=
  local bdf='([0-9a-fA-F]{4,8}:)?[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-7]'
  case $1 in
    hwp-epb)
      M_IMAGE=tools M_DEV="cpuN or all-cpus" M_DEVRE="^(cpu[0-9]+|all-cpus)$"
      M_ANSWERS="whether the CPU has HWP (hwp, hwp_epp flags) and an EPP file, and its energy_perf_bias"
      M_SETTLES="cpufreq.hwp of the profile; whether EPP can ever be a knob on that CPU"
      M_CHANGES="nothing (read only)"
      M_RESTORE="nothing to restore"
      ;;
    amdgpu-cap-range)
      M_IMAGE=tools M_DEV="an amdgpu PCI bus id or all" M_DEVRE="^($bdf|all)$" M_GPU=amd
      M_ANSWERS="power1_cap_min, power1_cap_max, power1_cap_default and power1_cap of each amdgpu device (the MI300X power1_cap_min)"
      M_SETTLES="gpus.limits.minW of the MI300X profile (assumed 0)"
      M_CHANGES="nothing (read only)"
      M_RESTORE="nothing to restore"
      ;;
    smi-formats)
      M_IMAGE=tools M_DEV="an NVIDIA PCI bus id, or all for the read-only commands" M_DEVRE="^($bdf|all)$" M_GPU=nvidia
      M_OPTS="read-only with-lmc"
      M_ANSWERS="the exact text of rocm-smi --showclocks [--json], --showperflevel, nvidia-smi -q -d SUPPORTED_CLOCKS, --help-query-gpu, the readback query and the -lgc and -rgc confirmations"
      M_SETTLES="the output texts the fake tools of simulator/pkg/hwemu/smi mark as assumed"
      M_CHANGES="on one NVIDIA GPU (not with all or --read-only): nvidia-smi -lgc <min>,<max> of its supported graphics clocks, then -rgc; with --with-lmc also -lmc and -rmc"
      M_RESTORE="nvidia-smi -rgc (and -rmc), journaled before the lock, run in the trap on any exit"
      ;;
    nvidia-pl-mig)
      M_IMAGE=tools M_DEV="the PCI bus id of an NVIDIA GPU with MIG enabled" M_DEVRE="^$bdf$" M_GPU=nvidia
      M_OPTS="watts"
      M_ANSWERS="the exit status and the effect of nvidia-smi -pl on a GPU with MIG mode Enabled"
      M_SETTLES="what the agent and the fake nvidia-smi do with -pl on a MIG-enabled GPU"
      M_CHANGES="nvidia-smi -i <bus> -pl <90 % of the current limit, at least the minimum, or --watts>"
      M_RESTORE="nvidia-smi -i <bus> -pl <the power.limit read before>, then the limit is read back"
      ;;
    nvidia-pl-na)
      M_IMAGE=tools M_DEV="the PCI bus id of an NVIDIA GPU whose power limits print [N/A]" M_DEVRE="^$bdf$" M_GPU=nvidia
      M_OPTS="watts"
      M_ANSWERS="the exit status of a refused -pl, with and without --error-on-warning, on a GPU whose min and max limits print a token"
      M_SETTLES="the exit status the fake nvidia-smi gives a refused -pl on such a GPU, with and without --error-on-warning (nvSetPowerLimit)"
      M_CHANGES="nvidia-smi -i <bus> -pl <the current limit, or --watts>, twice; a set to the current limit changes nothing even when accepted"
      M_RESTORE="nvidia-smi -i <bus> -pl <power.limit, else power.default_limit, read before>"
      ;;
    nvidia-lock-floor-idle)
      M_IMAGE=tools M_DEV="an NVIDIA PCI bus id" M_DEVRE="^$bdf$" M_GPU=nvidia
      M_OPTS="lock sample-s settle-s clock"
      M_ANSWERS="idle power and clocks under a clock lock floor against no lock, before and after; --clock memory locks the memory clock at its maximum, as sustained does on GDDR parts"
      M_SETTLES="gpus.clocks.lockFloorAtIdle and the idle cost of a floor (peak, low); with --clock memory, physics.gpu.dataPathHoldW of the L40S"
      M_CHANGES="nvidia-smi -i <bus> -lgc <supported clock near 75 % of max>,<max> (or --lock; -lmc <max>,<max> with --clock memory) for one idle period"
      M_RESTORE="nvidia-smi -i <bus> -rgc (or -rmc), journaled before the lock; then the idle clock must fall below the lock's floor"
      ;;
    nvidia-lock-lifetime)
      M_IMAGE=cuda M_DEV="an NVIDIA PCI bus id" M_DEVRE="^$bdf$" M_GPU=nvidia
      M_OPTS="lock load-s deinit-s"
      M_ANSWERS="clocks_event_reasons under -lgc with a load, and whether the lock survives every client leaving the GPU"
      M_SETTLES="gpus.clocks.locksResetOnDeinit and how a held lock reads back (simulator/pkg/hwemu/node_nvclock.go)"
      M_CHANGES="nvidia-smi -i <bus> -lgc <F>,<F>, F the supported clock near 60 % of max (or --lock), and a memory-bound load"
      M_RESTORE="nvidia-smi -i <bus> -rgc, journaled before the lock; then a 5 s load must clock above the lock"
      ;;
    hopper-receiver-gate | l40s-receiver-gate)
      M_IMAGE=cuda M_DEV="the receiver GPU's PCI bus id" M_DEVRE="^$bdf$" M_PEER=1 M_GPU=nvidia
      M_OPTS="arms bytes iters issuer cold-s"
      if [[ $1 == hopper-receiver-gate ]]; then
        M_ANSWERS="64 MiB cudaMemcpyPeer x12 into a receiver that is idle, busy, locked at max and at min graphics clock, with its clocks sampled every 10 ms"
        M_SETTLES="gpus.transfer.receiverGate (enabled, slowFraction) of the H100 profiles; whether sustained needs a graphics floor on Hopper"
        M_CHANGES="arms lgc-max and lgc-min: nvidia-smi -i <receiver> -lgc <clock>,<clock> during that arm"
      else
        M_ANSWERS="the matched L40S receiver test: 64 MiB copies x12 into a cold receiver, under default and -lmc <max>,<max>, its memory clock sampled every 10 ms"
        M_SETTLES="gpus.transfer.receiverGate of the L40S profile; the condition for an L40S sustained opt-in"
        M_CHANGES="arm lmc-max: nvidia-smi -i <receiver> -lmc <max>,<max> during that arm"
      fi
      M_RESTORE="nvidia-smi -rgc or -rmc on the receiver after each arm and in the trap"
      ;;
    instinct-receiver-gate)
      M_IMAGE=rocm M_DEV="the receiver GPU's PCI bus id" M_DEVRE="^$bdf$" M_PEER=1 M_GPU=amd
      M_OPTS="arms bytes iters issuer cold-s offload-arch"
      M_ANSWERS="64 MiB hipMemcpyPeer x12 into an AMD Instinct receiver idle under auto against one running a memory-bound kernel"
      M_SETTLES="gpus.transfer.receiverGate of the MI300X profile: whether the W7900 gate generalises"
      M_CHANGES="nothing with the default arms (idle, busy); arm high writes high to the receiver's power_dpm_force_performance_level"
      M_RESTORE="the level read before, written back and read back"
      ;;
    w7900-level-power)
      M_IMAGE=rocm M_DEV="an amdgpu PCI bus id or all" M_DEVRE="^($bdf|all)$" M_GPU=amd
      M_OPTS="sample-s settle-s load levels offload-arch"
      M_ANSWERS="GPU power at level high (--levels) against auto, idle and under a memory-bound kernel, sampled every second"
      M_SETTLES="physics.gpu.dataPathHoldW of the W7900 profile (assumed 10 W); with --levels high,profile_peak also gatingOffIdleW (assumed 20 W)"
      M_CHANGES="power_dpm_force_performance_level of each device: each level of --levels for one idle and one loaded period"
      M_RESTORE="auto (the level read before), written back and read back"
      ;;
    fabric-floor)
      M_IMAGE=rocm M_DEV="the receiver GPU's PCI bus id" M_DEVRE="^$bdf$" M_PEER=1 M_GPU=amd
      M_OPTS="sample-s bytes iters offload-arch"
      M_ANSWERS="idle power and peer copies into a receiver at auto, high, and manual with only the top fabric clock forced"
      M_SETTLES="a cheaper sustained mapping for the W7900 (pkg/governor PlanAMD)"
      M_CHANGES="the receiver's power_dpm_force_performance_level (high, then manual) and pp_dpm_fclk (the top level)"
      M_RESTORE="the level read before (auto), which also drops the forced fclk mask, written back and read back"
      ;;
    cpu-governor-energy)
      M_IMAGE=tools M_DEV="all-cpus" M_DEVRE="^all-cpus$"
      M_OPTS="governors loads settle-s measure-s"
      M_ANSWERS="package energy (RAPL energy_uj) and stress-ng work per governor at steady loads, idle included"
      M_SETTLES="physics.cpu.idleGovernorDeltaW and Q1 on that driver (EPYC acpi-cpufreq first)"
      M_CHANGES="scaling_governor of every cpufreq policy, one governor at a time"
      M_RESTORE="each policy's scaling_governor read before, written back and read back"
      ;;
    schedutil-steps)
      M_IMAGE=tools M_DEV="cpuN" M_DEVRE="^cpu[0-9]+$"
      M_OPTS="loads measure-s"
      M_ANSWERS="the frequencies schedutil picks for one CPU at a steady load (trace of power:cpu_frequency and scaling_cur_freq)"
      M_SETTLES="the 3-entry step rule of hwemu's GovernorStep for schedutil"
      M_CHANGES="that CPU's policy scaling_governor (only when it is not schedutil already), a tracefs instance"
      M_RESTORE="the governor read before; the tracefs instance removed"
      ;;
    conservative-steps)
      M_IMAGE=tools M_DEV="cpuN" M_DEVRE="^cpu[0-9]+$"
      M_OPTS="loads measure-s"
      M_ANSWERS="whether conservative's 5 % steps move between the entries of a short table or stall, at 30 and 70 %"
      M_SETTLES="the conservative rule of hwemu's GovernorStep on a 3-entry table"
      M_CHANGES="that CPU's policy scaling_governor (conservative), a tracefs instance"
      M_RESTORE="the governor read before; the tracefs instance removed"
      ;;
    *) return 1 ;;
  esac
}

# minutes MEASUREMENT prints the expected run time in whole minutes, from
# the options (opt_*), without the image pull.
minutes() {
  local s
  case $1 in
    hwp-epb | amdgpu-cap-range | smi-formats | nvidia-pl-mig | nvidia-pl-na) s=30 ;;
    nvidia-lock-floor-idle) s=$((3 * ${opt_sample_s:-60} + 2 * ${opt_settle_s:-5})) ;;
    nvidia-lock-lifetime) s=$((2 * ${opt_load_s:-20} + ${opt_deinit_s:-15} + 90)) ;;
    hopper-receiver-gate | l40s-receiver-gate | instinct-receiver-gate)
      s=$((4 * (${opt_cold_s:-10} + ${opt_iters:-12} + 5) + 90))
      ;;
    w7900-level-power)
      local n
      n=$(tr ',' '\n' <<<"${opt_levels:-high}" | grep -c .)
      s=$((2 * (n + 1) * (${opt_sample_s:-120} + 2 * ${opt_settle_s:-10}) + 90))
      ;;
    fabric-floor) s=$((3 * (${opt_sample_s:-60} + 25) + 90)) ;;
    cpu-governor-energy)
      local g l
      g=$(tr ',' '\n' <<<"${opt_governors:-performance,schedutil,conservative}" | grep -c .)
      l=$(tr ',' '\n' <<<"${opt_loads:-0,30,70,100}" | grep -c .)
      s=$((g * l * (${opt_settle_s:-10} + ${opt_measure_s:-60} + 3)))
      ;;
    schedutil-steps) s=$(($(tr ',' '\n' <<<"${opt_loads:-35}" | grep -c .) * (${opt_measure_s:-30} + 3))) ;;
    conservative-steps) s=$(($(tr ',' '\n' <<<"${opt_loads:-30,70}" | grep -c .) * (${opt_measure_s:-30} + 3))) ;;
  esac
  echo $(((s + 59) / 60))
}

cmd_list() {
  local m
  printf '%-24s %-6s %-10s %s\n' MEASUREMENT IMAGE OPTIONS ANSWERS
  for m in $MEASUREMENTS; do
    spec "$m"
    printf '%-24s %-6s %-10s %s\n' "$m" "$M_IMAGE" "${M_OPTS// /,}" "$M_ANSWERS"
  done
}

# ----------------------------------------------------------------- options

measurement=${1:-}
case $measurement in
  "" | -h | --help)
    usage
    exit 0
    ;;
  list)
    cmd_list
    exit 0
    ;;
esac
spec "$measurement" || refuse "unknown measurement $measurement (hwmeasure.sh list)"
shift

kubeconfig='' context='' node='' device='' peer='' namespace=default image=''
pull_secret='' runtime_class='' gpu_request='' host_root=/ start_timeout=1800
out_root=${XDG_CACHE_HOME:-$HOME/.cache}/joulie/experiments/05/hardware
dry_run=0 yes=0 allow_cordoned=0 allow_other=0
expect=() allow_taints=() opts_set=()

# setopt NAME VALUE PATTERN checks a measurement option and keeps it.
setopt() {
  [[ " $M_OPTS " == *" $1 "* ]] || refuse "$measurement takes no --$1 (it takes: ${M_OPTS:-no measurement option})"
  [[ $2 =~ $3 ]] || refuse "--$1 $2 does not match $3"
  local v=opt_${1//-/_}
  printf -v "$v" '%s' "$2"
  opts_set+=("$1")
}

need_value() { (($2 >= 2)) || refuse "$1 needs a value"; }

while (($#)); do
  case $1 in
    --kubeconfig | --context | --node | --device | --peer | --namespace | --image | --image-pull-secret | \
      --runtime-class | --gpu-request | --host-root | --out | --start-timeout | --expect-pod | --allow-taint)
      need_value "$1" $#
      ;;
  esac
  case $1 in
    --kubeconfig) kubeconfig=$2 ;;
    --context) context=$2 ;;
    --node) node=$2 ;;
    --device) device=$2 ;;
    --peer) peer=$2 ;;
    --namespace) namespace=$2 ;;
    --image) image=$2 ;;
    --image-pull-secret) pull_secret=$2 ;;
    --runtime-class) runtime_class=$2 ;;
    --gpu-request) gpu_request=$2 ;;
    --host-root) host_root=$2 ;;
    --out) out_root=$2 ;;
    --start-timeout) start_timeout=$2 ;;
    --expect-pod) expect+=("$2") ;;
    --allow-taint) allow_taints+=("$2") ;;
    --allow-cordoned) allow_cordoned=1 ;;
    --allow-other-pods) allow_other=1 ;;
    --dry-run) dry_run=1 ;;
    --yes) yes=1 ;;
    --read-only | --with-lmc) setopt "${1#--}" 1 '^1$' ;;
    --sample-s | --settle-s | --measure-s | --load-s | --deinit-s | --cold-s | --iters)
      need_value "$1" $#
      setopt "${1#--}" "$2" '^[0-9]{1,5}$'
      ;;
    --bytes)
      need_value "$1" $#
      setopt bytes "$2" '^[0-9]{1,12}$'
      ;;
    --watts)
      need_value "$1" $#
      setopt watts "$2" '^[0-9]{1,4}(\.[0-9]+)?$'
      ;;
    --lock)
      need_value "$1" $#
      setopt lock "$2" '^[0-9]{2,5},[0-9]{2,5}$'
      ;;
    --loads)
      need_value "$1" $#
      setopt loads "$2" '^(100|[1-9]?[0-9])(,(100|[1-9]?[0-9]))*$'
      ;;
    --governors)
      need_value "$1" $#
      setopt governors "$2" '^[a-z_]+(,[a-z_]+)*$'
      ;;
    --arms)
      need_value "$1" $#
      setopt arms "$2" '^[a-z-]+(,[a-z-]+)*$'
      ;;
    --issuer)
      need_value "$1" $#
      setopt issuer "$2" '^(src|dst)$'
      ;;
    --load)
      need_value "$1" $#
      setopt load "$2" '^(vram|none)$'
      ;;
    --levels)
      need_value "$1" $#
      setopt levels "$2" '^(high|low|profile_[a-z_]+)(,(high|low|profile_[a-z_]+))*$'
      ;;
    --clock)
      need_value "$1" $#
      setopt clock "$2" '^(graphics|memory)$'
      ;;
    --offload-arch)
      need_value "$1" $#
      setopt offload-arch "$2" '^gfx[0-9a-f]+(,gfx[0-9a-f]+)*$'
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *) refuse "unknown argument $1 (see --help)" ;;
  esac
  case $1 in
    --allow-cordoned | --allow-other-pods | --dry-run | --yes | --read-only | --with-lmc) shift ;;
    *) shift 2 ;;
  esac
done

[[ -n $kubeconfig ]] || refuse "--kubeconfig is required"
[[ -n $context ]] || refuse "--context is required"
[[ -n $node ]] || refuse "--node is required"
[[ -n $device ]] || refuse "--device is required: $M_DEV"
[[ $device =~ $M_DEVRE ]] || refuse "--device $device: $measurement takes $M_DEV"
if ((M_PEER)); then
  [[ -n $peer ]] || refuse "--peer is required: the PCI bus id of the GPU the copies come from"
  [[ $peer =~ $M_DEVRE ]] || refuse "--peer $peer is not a PCI bus id"
elif [[ -n $peer ]]; then
  refuse "$measurement takes no --peer"
fi
[[ $node =~ ^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$ ]] || refuse "--node $node is not a node name"
[[ $namespace =~ ^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$ ]] || refuse "--namespace $namespace is not a namespace name"
[[ $start_timeout =~ ^[0-9]{1,5}$ ]] || refuse "--start-timeout $start_timeout is not a number of seconds"
[[ $host_root == /* && $host_root != *..* ]] || refuse "--host-root $host_root is not an absolute path"

# The kubeconfig: never one whose name, or the file it resolves to, says
# prod.
kc_lower=$(tr '[:upper:]' '[:lower:]' <<<"$kubeconfig")
[[ $kc_lower != *prod* ]] || refuse "--kubeconfig $kubeconfig: a kubeconfig whose path contains \"prod\" is never used"
[[ -f $kubeconfig ]] || refuse "--kubeconfig $kubeconfig is not a file"
kc_real=$(readlink -f "$kubeconfig")
[[ $(tr '[:upper:]' '[:lower:]' <<<"$kc_real") != *prod* ]] ||
  refuse "--kubeconfig $kubeconfig resolves to $kc_real, whose path contains \"prod\""

case $image in
  "") image_kind=$M_IMAGE ;;
  tools | cuda | rocm) image_kind=$image ;;
  *) image_kind=custom ;;
esac
if [[ $measurement == w7900-level-power && ${opt_load:-vram} == none && -z $image ]]; then image_kind=tools; fi
case $image_kind in
  tools) image_ref=$HW_IMAGE_TOOLS ;;
  cuda) image_ref=$HW_IMAGE_CUDA ;;
  rocm) image_ref=$HW_IMAGE_ROCM ;;
  custom) image_ref=$image ;;
esac
[[ $image_ref == *@sha256:* ]] || refuse "--image $image_ref has no @sha256: digest; name the image by digest"

command -v kubectl >/dev/null || refuse "kubectl is not on PATH"
command -v jq >/dev/null || refuse "jq is not on PATH"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
export KUBECACHEDIR=$tmp/kube-cache
kc() { kubectl --kubeconfig "$kubeconfig" --context "$context" --request-timeout=60s "$@"; }
kc_long() { kubectl --kubeconfig "$kubeconfig" --context "$context" "$@"; }

# ------------------------------------------------------------- node checks

kubectl --kubeconfig "$kubeconfig" config get-contexts -o name 2>/dev/null | grep -qxF "$context" ||
  refuse "context $context is not in $kubeconfig"

kc get node "$node" -o json >"$tmp/node.json" 2>"$tmp/err" || refuse "cannot read node $node: $(cat "$tmp/err")"
kc get pods -A --field-selector "spec.nodeName=$node" -o json >"$tmp/pods.json" 2>"$tmp/err" ||
  refuse "cannot list the pods of node $node: $(cat "$tmp/err")"

unsched=$(jq -r '.spec.unschedulable // false' "$tmp/node.json")
cordon_taint=$(jq -r '[.spec.taints // [] | .[] | select(.key == "node.kubernetes.io/unschedulable")] | length' "$tmp/node.json")
if [[ $unsched == true || $cordon_taint != 0 ]]; then
  ((allow_cordoned)) || refuse "node $node is cordoned (spec.unschedulable $unsched, unschedulable taint $cordon_taint); --allow-cordoned runs anyway, only on a node you cordoned yourself"
  say "node $node is cordoned; going on because of --allow-cordoned"
fi
tolerations=()
taints_line=
# Fields are joined by 0x1f: a tab is IFS white space, and read would merge
# the empty value of a taint such as nvidia.com/gpu:NoSchedule.
while IFS=$'\x1f' read -r key value effect; do
  [[ -n $key ]] || continue
  taints_line+="${taints_line:+, }$key${value:+=$value}:$effect"
  [[ $key == node.kubernetes.io/unschedulable ]] && continue
  if [[ $effect == NoSchedule || $effect == NoExecute ]]; then
    ok=0
    for t in "${allow_taints[@]}"; do [[ $t == "$key" ]] && ok=1; done
    ((ok)) || refuse "node $node has the taint $key${value:+=$value}:$effect; --allow-taint $key runs anyway, with a toleration for it"
    tolerations+=("$key")
  fi
done < <(jq -r '.spec.taints // [] | .[] | [.key, (.value // ""), .effect] | join("\u001f")' "$tmp/node.json")

expected_pods=() other_pods=() agent_pods=()
while IFS=$'\x1f' read -r ns name owners mirror app; do
  [[ -n $name ]] || continue
  [[ $app == joulie-agent || $name == joulie-agent-* ]] && agent_pods+=("$ns/$name")
  if [[ $owners == *DaemonSet* || $mirror == true ]]; then
    expected_pods+=("$ns/$name")
    continue
  fi
  hit=0
  for re in "${expect[@]}"; do
    if [[ $ns/$name =~ ^($re)$ ]]; then hit=1; fi
  done
  if ((hit)); then expected_pods+=("$ns/$name"); else other_pods+=("$ns/$name"); fi
done < <(jq -r '.items[] | select(.status.phase != "Succeeded" and .status.phase != "Failed") |
  [.metadata.namespace, .metadata.name, ((.metadata.ownerReferences // []) | map(.kind) | join("+")),
   (.metadata.annotations["kubernetes.io/config.mirror"] != null | tostring),
   (.metadata.labels["app.kubernetes.io/name"] // "")] | join("\u001f")' "$tmp/pods.json")
if ((${#other_pods[@]})); then
  if ((allow_other)); then
    say "node $node runs ${#other_pods[@]} pods no DaemonSet owns; going on because of --allow-other-pods"
  else
    refuse "node $node runs pods it was not told to expect: ${other_pods[*]} (--expect-pod REGEX for each, matched against namespace/name, or --allow-other-pods)"
  fi
fi

# The GPU request: every allocatable whole GPU of the measurement's vendor.
gpu_res='' gpu_n=0
if [[ -n $gpu_request && $gpu_request != none ]]; then
  [[ $gpu_request =~ ^([a-z0-9.-]+/[a-zA-Z0-9._-]+)=([0-9]+)$ ]] || refuse "--gpu-request $gpu_request is not RESOURCE=N or none"
  gpu_res=${BASH_REMATCH[1]} gpu_n=${BASH_REMATCH[2]}
elif [[ -z $gpu_request && $M_GPU != none ]]; then
  r=nvidia.com/gpu
  [[ $M_GPU == amd ]] && r=amd.com/gpu
  n=$(jq -r --arg r "$r" '.status.allocatable[$r] // "0"' "$tmp/node.json")
  if [[ $n =~ ^[0-9]+$ ]] && ((n > 0)); then gpu_res=$r gpu_n=$n; fi
fi
gpu_alloc=$(jq -r '.status.allocatable // {} | to_entries | map(select(.key | test("gpu|mig-|amd.com"))) | map("\(.key)=\(.value)") | join(", ")' "$tmp/node.json")

# ---------------------------------------------------------------- the plan

stamp=$(date -u +%Y%m%dT%H%M%SZ)
pod=joulie-hw-$measurement-$(tr '[:upper:]' '[:lower:]' <<<"$stamp")
cm=$pod
run_dir=$out_root/$stamp-$measurement
mins=$(minutes "$measurement")
pull_s=$start_timeout
deadline_s=$((mins * 60 * 3 + 900 + pull_s))
opt_line=
for o in "${opts_set[@]}"; do
  v=opt_${o//-/_}
  opt_line+="${opt_line:+ }--$o ${!v}"
done

plan() {
  cat <<EOF
Measurement   $measurement
  answers     $M_ANSWERS
  settles     $M_SETTLES
Cluster       context $context of $kubeconfig
Node          $node (unschedulable: $unsched; taints: ${taints_line:-none}); $(jq -r '.status.nodeInfo | "\(.osImage), kernel \(.kernelVersion)"' "$tmp/node.json")
Device        $device${peer:+ (copies from $peer)}
GPUs on node  ${gpu_alloc:-none allocatable}
Pod           $namespace/$pod, privileged, root; host root read only at /host ($host_root), host sys at /host-sys (${host_root%/}/sys)
Image         $image_ref
GPU request   ${gpu_res:+$gpu_res: $gpu_n}${gpu_res:-none}
Other pods    ${#expected_pods[@]} expected (DaemonSet, static or --expect-pod)${other_pods[*]:+; not expected but allowed: ${other_pods[*]}}
Changes       $M_CHANGES
Restore       $M_RESTORE; on success, failure, Ctrl-C or pod deletion; then every knob is compared with the baseline
Options       ${opt_line:-defaults}
Duration      about $mins min after the pod starts (image pull and build not included); pod deadline $((deadline_s / 60)) min
Results       $run_dir
EOF
  if ((${#agent_pods[@]})); then
    echo "Warning       a Joulie agent runs here (${agent_pods[*]}): with GOVERNOR_WRITES=true or caps planned for this node it may write the knobs this measurement changes; turn Joulie off for the node first"
  fi
}

plan_text=$(plan)
printf '%s\n' "$plan_text" >&2

json_str() { # json_str S prints S as a JSON string
  jq -Rn --arg s "$1" '$s'
}

render() {
  local args env res tol secrets rc line
  args=$(jq -cn --arg m "$measurement" --arg d "$device" --arg p "$peer" '[$m, $d] + (if $p == "" then [] else [$p] end)')
  env=$(
    {
      printf 'HW_LINGER_S\t900\n'
      printf 'HW_STOP_S\t120\n'
      # Another host root is not the kernel's /sys (an emulated node's
      # tree): the kernel's own tracefs would trace the wrong CPU.
      [[ $host_root == / ]] || printf 'HW_TRACEFS\tnone\n'
      for o in "${opts_set[@]}"; do
        v=opt_${o//-/_}
        printf 'HW_OPT_%s\t%s\n' "$(tr '[:lower:]-' '[:upper:]_' <<<"$o")" "${!v}"
      done
    } | jq -Rsc 'split("\n") | map(select(length > 0) | split("\t") | {name: .[0], value: .[1]})'
  )
  if [[ -n $gpu_res ]]; then
    res=$(jq -cn --arg r "$gpu_res" --arg n "$gpu_n" '{limits: {($r): $n}}')
  else
    res='{}'
  fi
  tol=$(printf '%s\n' "${tolerations[@]}" | jq -Rsc 'split("\n") | map(select(length > 0) | {key: ., operator: "Exists"})')
  if [[ -n $pull_secret ]]; then secrets=$(jq -cn --arg s "$pull_secret" '[{name: $s}]'); else secrets='[]'; fi
  if [[ -n $runtime_class ]]; then rc=$(json_str "$runtime_class"); else rc=null; fi
  local j_pod j_ns j_m j_node j_image j_cm j_root j_sys
  j_pod=$(json_str "$pod") j_ns=$(json_str "$namespace") j_m=$(json_str "$measurement")
  j_node=$(json_str "$node") j_image=$(json_str "$image_ref") j_cm=$(json_str "$cm")
  j_root=$(json_str "$host_root") j_sys=$(json_str "${host_root%/}/sys")
  while IFS= read -r line || [[ -n $line ]]; do
    if [[ $line == *@* && $line != \#* ]]; then
      line=${line//@POD@/$j_pod}
      line=${line//@NAMESPACE@/$j_ns}
      line=${line//@MEASUREMENT@/$j_m}
      line=${line//@NODE@/$j_node}
      line=${line//@GRACE_S@/150}
      line=${line//@DEADLINE_S@/$deadline_s}
      line=${line//@TOLERATIONS@/$tol}
      line=${line//@PULL_SECRETS@/$secrets}
      line=${line//@RUNTIME_CLASS@/$rc}
      line=${line//@IMAGE@/$j_image}
      line=${line//@ARGS@/$args}
      line=${line//@ENV@/$env}
      line=${line//@RESOURCES@/$res}
      line=${line//@CONFIGMAP@/$j_cm}
      line=${line//@HOST_ROOT@/$j_root}
      line=${line//@HOST_SYS@/$j_sys}
    fi
    printf '%s\n' "$line"
  done <"$here/pod.yaml"
}

render >"$tmp/pod.yaml"
if grep -q '@[A-Z_]*@' "$tmp/pod.yaml"; then
  refuse "the pod template left a placeholder: $(grep -o '@[A-Z_]*@' "$tmp/pod.yaml" | sort -u | tr '\n' ' ')"
fi

if ((dry_run)); then
  say "dry run: the pod would be"
  cat "$tmp/pod.yaml"
  exit 0
fi

if ((!yes)); then
  printf 'Type the node name (%s) to start, anything else to stop: ' "$node" >&2
  answer=
  read -r answer || true
  [[ $answer == "$node" ]] || refuse "not confirmed"
fi

mkdir -p "$run_dir/raw"
printf '%s\n' "$plan_text" >"$run_dir/plan.txt"
cp "$tmp/pod.yaml" "$tmp/node.json" "$tmp/pods.json" "$run_dir/"
say "run directory $run_dir"

# ------------------------------------------------------------------- run

created_cm=0 created_pod=0 running=0 collected=0 interrupted=0 logs_pid='' shown=0
probe_status='' verify_rc='' rc=0

kexec() { kc exec -n "$namespace" "$pod" -c probe -- bash /probe/probe.sh "$@"; }

show_log() {
  local n
  [[ -f $run_dir/probe.log ]] || return 0
  n=$(wc -l <"$run_dir/probe.log")
  if ((n > shown)); then
    tail -n "+$((shown + 1))" "$run_dir/probe.log" | head -n "$((n - shown))" >&2
    shown=$n
  fi
}

collect() {
  ((collected)) && return 0
  say "copying the results"
  if kc_long --request-timeout=300s exec -n "$namespace" "$pod" -c probe -- bash /probe/probe.sh pack >"$tmp/out.tar" 2>"$tmp/err"; then
    tar -C "$run_dir/raw" -xf "$tmp/out.tar" && collected=1
  else
    say "could not copy the results: $(cat "$tmp/err")"
  fi
  if kexec verify >"$run_dir/verify-after.txt" 2>&1; then verify_rc=0; else verify_rc=$?; fi
  kexec release >/dev/null 2>&1 || true
}

# cleanup runs on every exit once the run started (the EXIT trap).
# shellcheck disable=SC2317
cleanup() {
  local code=$?
  set +e
  trap '' INT TERM HUP
  ((interrupted)) && code=130
  # The probe may run before wait_running sees it (it polls): whether to
  # stop and collect is asked of the container itself.
  if ((created_pod && !running)); then
    case $(kexec status 2>/dev/null) in running* | done*) running=1 ;; esac
  fi
  if ((created_pod && running)); then
    if [[ $probe_status != done* ]]; then
      say "stopping the measurement: its trap restores what it changed (up to 150 s; Ctrl-C is ignored until the pod is gone)"
      probe_status=$(kc_long --request-timeout=300s exec -n "$namespace" "$pod" -c probe -- bash /probe/probe.sh stop 2>&1 | tail -n1) || true
      say "probe: ${probe_status:-no answer}"
    fi
    collect || true
    show_log
  fi
  if ((created_pod)); then
    say "deleting pod $namespace/$pod"
    kc_long --request-timeout=300s delete pod -n "$namespace" "$pod" --wait=true --timeout=240s >/dev/null 2>&1 ||
      say "could not delete pod $namespace/$pod: delete it by hand (kubectl delete pod -n $namespace $pod); its TERM trap restores"
  fi
  if ((created_cm)); then
    kc delete configmap -n "$namespace" "$cm" >/dev/null 2>&1 || say "could not delete configmap $namespace/$cm"
  fi
  if [[ -n $logs_pid ]]; then
    kill "$logs_pid" 2>/dev/null || true
    wait "$logs_pid" 2>/dev/null || true
  fi
  show_log
  if [[ -f $run_dir/raw/summary.md ]]; then
    {
      cat "$run_dir/raw/summary.md"
      echo
      echo "Run: $stamp, image $image_ref, options: ${opt_line:-defaults}, probe status: ${probe_status#done }."
    } >"$run_dir/summary.md"
  fi
  local restore=unknown verify=unknown
  if [[ $probe_status =~ restore=([a-z]+) ]]; then restore=${BASH_REMATCH[1]}; fi
  if [[ $probe_status =~ verify=([a-z,]+) ]]; then verify=${BASH_REMATCH[1]}; fi
  if [[ $restore == failed || $verify == failed* || ${verify_rc:-0} != 0 ]]; then
    say "RESTORE NOT VERIFIED: see $run_dir/raw/restore.txt and $run_dir/verify-after.txt; the journal lines in $run_dir/probe.log hold every value read before a change"
    code=3
  elif ((running)) && [[ $restore == unknown ]]; then
    say "RESTORE NOT CONFIRMED: the probe ran but could not be asked; deleting its pod sent it TERM, whose trap restores, and the journal lines in $run_dir/probe.log hold every value read before a change"
    code=3
  elif [[ $verify == *doubt* ]]; then
    say "restore verified by readback, with a doubt: $(cat "$run_dir/raw/restore-doubt.txt" 2>/dev/null)"
  fi
  if ((created_pod)); then
    say "results in $run_dir (restore: $restore, verify: $verify)"
    [[ -f $run_dir/summary.md ]] && cat "$run_dir/summary.md" >&2
  fi
  rm -rf "$tmp"
  exit "$code"
}
trap cleanup EXIT
trap 'interrupted=1; exit 130' INT TERM HUP

# wait_running waits for the pod's phase Running, and gives up early when
# the pod ended (an admission the kubelet refused, such as GPUs in use).
wait_running() {
  local deadline=$((SECONDS + start_timeout)) st last=''
  while ((SECONDS < deadline)); do
    st=$(kc get pod -n "$namespace" "$pod" -o jsonpath='{.status.phase} {.status.reason} {.status.containerStatuses[0].state.waiting.reason}' 2>/dev/null) || st=''
    case $st in
      Running*) return 0 ;;
      Failed* | Succeeded*)
        say "the pod ended before it ran: $st"
        return 1
        ;;
    esac
    if [[ -n $st && $st != "$last" ]]; then say "pod: $st"; fi
    last=$st
    sleep 2
  done
  say "the pod did not run within $start_timeout s (last: ${last:-no status})"
  return 1
}

say "creating configmap and pod $namespace/$pod on $node"
kc create configmap -n "$namespace" "$cm" --from-file=probe.sh="$here/probe.sh" --from-file=peercopy.cu="$here/peercopy.cu" >/dev/null
created_cm=1
kc label configmap -n "$namespace" "$cm" app.kubernetes.io/name=joulie-hw-measurement app.kubernetes.io/part-of=joulie-experiment-05 >/dev/null
kc create -f "$run_dir/pod.yaml" >/dev/null
created_pod=1

say "waiting up to $start_timeout s for the pod to run (image pull)"
if ! wait_running; then
  kc get pod -n "$namespace" "$pod" -o wide >&2 || true
  kc get events -n "$namespace" --field-selector "involvedObject.name=$pod" >&2 || true
  say "the probe never ran, so nothing was changed"
  exit 1
fi
running=1
kc_long logs -f -n "$namespace" "$pod" -c probe >"$run_dir/probe.log" 2>&1 &
logs_pid=$!

misses=0
while :; do
  if st=$(kexec status 2>/dev/null); then
    misses=0
    if [[ $st == done* ]]; then
      probe_status=$st
      break
    fi
  else
    misses=$((misses + 1))
    if ((misses >= 10)); then
      say "the pod no longer answers: $(kc get pod -n "$namespace" "$pod" -o jsonpath='{.status.phase}' 2>&1)"
      exit 1
    fi
  fi
  show_log
  sleep 2
done
show_log
say "probe: $probe_status"
collect
if [[ $probe_status =~ rc=([0-9]+) ]]; then
  case ${BASH_REMATCH[1]} in
    0) rc=0 ;;
    2) rc=2 ;;
    *) rc=1 ;;
  esac
else
  rc=1
fi
exit "$rc"
