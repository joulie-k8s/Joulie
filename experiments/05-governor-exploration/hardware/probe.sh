#!/usr/bin/env bash
#
# probe.sh - the half of hwmeasure.sh that runs inside the measurement pod.
#
# Usage (as the pod's command, PID 1):
#   probe.sh run MEASUREMENT DEVICE [PEER]
# Usage (through kubectl exec, by hwmeasure.sh):
#   probe.sh status    prints "done <STATUS>", "running <stage>" or "absent"
#   probe.sh stop      sends TERM to the running measurement and waits for its
#                      restore (up to HW_STOP_S seconds, 150); the stopped
#                      probe still waits for its results to be collected
#   probe.sh verify    compares the knobs with the baseline snapshot again
#   probe.sh pack      writes the output directory as a tar to stdout
#   probe.sh release   lets the finished measurement exit
#
# A measurement first checks the device and its tools and reads every knob
# it may change (the preflight; it changes nothing and exits 2 when a check
# fails). Before each change it appends the value read to a journal
# ($HW_OUT/journal, and the pod log), so an EXIT trap, which also runs on a
# failure and on TERM, INT or HUP, can put every knob back in reverse order.
# Then it reads the knobs again and compares them with the snapshot taken
# before the first change ($HW_OUT/verify.txt), writes $HW_OUT/STATUS, and
# waits up to HW_LINGER_S seconds (900) for hwmeasure.sh to collect the
# results.
#
# Paths (environment; the pod sets none of them): HW_OUT (/out), HW_SYS
# (/host-sys, the host /sys), HW_HOST (/host, the host root, read only),
# HW_TRACEFS (auto: the host's tracefs, else one mounted here; none: no
# trace), HW_NVSMI and HW_ROCMSMI (a command; default: the tool in the
# image, else the host's through chroot), HW_PEERCOPY (a built peercopy;
# default: built from peercopy.cu with nvcc or hipcc). Options are HW_OPT_*
# variables, set by hwmeasure.sh from its flags.
#
set -uo pipefail

HW_OUT=${HW_OUT:-/out}
HW_SYS=${HW_SYS:-/host-sys}
HW_HOST=${HW_HOST:-/host}
HW_TRACEFS=${HW_TRACEFS:-auto}
HW_PROBE_DIR=${HW_PROBE_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)}
HW_LINGER_S=${HW_LINGER_S:-900}
HW_STOP_S=${HW_STOP_S:-150}

JOURNAL=$HW_OUT/journal
SUMMARY=$HW_OUT/summary.md
BASE_SNAP=$HW_OUT/baseline.snap
BG_PIDS=()
SNAP_PATHS=()
SNAP_NV_BUSES=()
NVSMI=()
ROCMSMI=()
PEERCOPY=
TRACE_DIR=
STOP_REQ=0
IN_EXIT=0
CHANGED=0

# ---------------------------------------------------------------- helpers

# log prints to stderr, so a function whose output a caller captures can log.
log() {
  printf '[probe %s] %s\n' "$(date -u +%H:%M:%S)" "$*" | tee -a "$HW_OUT/probe.log" >&2
}

die() {
  log "error: $*"
  exit 1
}

# refuse: a preflight check failed; nothing has been changed.
refuse() {
  log "refused: $*"
  exit 2
}

now() { date +%s.%N; }

stage() { printf '%s\n' "$*" >"$HW_OUT/stage"; }

# rd FILE prints the first line of FILE without surrounding blanks; it fails
# when the file cannot be read.
rd() {
  local v
  { IFS= read -r v || [[ -n $v ]]; } 2>/dev/null <"$1" || return 1
  v=${v#"${v%%[![:space:]]*}"}
  printf '%s\n' "${v%"${v##*[![:space:]]}"}"
}

opt() { # opt NAME DEFAULT prints HW_OPT_NAME or the default
  local n=HW_OPT_$1
  printf '%s\n' "${!n:-$2}"
}

# nap S sleeps S seconds; TERM interrupts it at once.
nap() {
  sleep "$1" &
  local p=$!
  wait "$p" 2>/dev/null
}

# bg CMD... starts CMD in its own session and process group, so stopping it
# stops every process it forked; prints nothing, sets BG_LAST.
bg() {
  setsid "$@" &
  BG_LAST=$!
  BG_PIDS+=("$BG_LAST")
}

# bgf FUNCTION... starts a shell function in the background (a sampler).
bgf() {
  "$@" &
  BG_LAST=$!
  BG_PIDS+=("$BG_LAST")
}

bg_stop() {
  local p=$1 i
  kill -TERM -- "-$p" 2>/dev/null || kill -TERM "$p" 2>/dev/null
  for ((i = 0; i < 50; i++)); do
    kill -0 "$p" 2>/dev/null || break
    sleep 0.1
  done
  kill -KILL -- "-$p" 2>/dev/null || kill -KILL "$p" 2>/dev/null
  wait "$p" 2>/dev/null
  return 0
}

bg_stop_all() {
  local p
  for p in "${BG_PIDS[@]}"; do
    kill -0 "$p" 2>/dev/null && bg_stop "$p"
  done
  BG_PIDS=()
}

# rec NAME CMD... runs CMD, keeping its command line, stdout, stderr and exit
# status as NAME.cmd, NAME.out, NAME.err and NAME.rc; returns the status.
rec() {
  local name=$1 rc
  shift
  printf '%q ' "$@" >"$HW_OUT/$name.cmd"
  echo >>"$HW_OUT/$name.cmd"
  "$@" >"$HW_OUT/$name.out" 2>"$HW_OUT/$name.err"
  rc=$?
  echo "$rc" >"$HW_OUT/$name.rc"
  return "$rc"
}

summary() { printf '%s\n' "$*" >>"$SUMMARY"; }

# stats FILE COL prints "n=.. mean=.. median=.. min=.. max=.." of the
# numeric values in column COL of a comma separated FILE.
stats() {
  cut -d, -f"$2" "$1" 2>/dev/null | tr -d ' ' | grep -E '^-?[0-9]+(\.[0-9]+)?$' | sort -g |
    awk '{ v[NR] = $1; s += $1 }
      END {
        if (NR == 0) { print "n=0"; exit }
        m = (NR % 2) ? v[(NR + 1) / 2] : (v[NR / 2] + v[NR / 2 + 1]) / 2
        printf "n=%d mean=%.3f median=%.3f min=%.3f max=%.3f\n", NR, s / NR, m, v[1], v[NR]
      }'
}

# field KEY STATS prints one value of a stats line.
field() { sed -n "s/.*\\b$1=\\([^ ]*\\).*/\\1/p" <<<"$2"; }

# --------------------------------------------------------- journal, restore

# jrec KIND TARGET VALUE appends a journal entry; restore_all undoes it.
jrec() {
  local n
  : >>"$JOURNAL"
  n=$(($(wc -l <"$JOURNAL") + 1))
  printf '%s\t%s\t%s\t%s\n' "$n" "$1" "$2" "$3" >>"$JOURNAL"
  CHANGED=1
  log "journal $n: $1 $2 (restore to: $3)"
}

# journaled KIND TARGET reports whether a change of TARGET is journaled
# and not undone yet; once undone, the next change journals the value again.
journaled() {
  local n
  [[ -s $JOURNAL ]] || return 1
  touch "$HW_OUT/journal.done"
  while read -r n; do
    grep -qxF "$n" "$HW_OUT/journal.done" || return 0
  done < <(awk -F'\t' -v k="$1" -v t="$2" '$2 == k && $3 == t { print $1 }' "$JOURNAL")
  return 1
}

# set_sysfs FILE VALUE writes VALUE to a sysfs attribute, after journaling
# the value it had, and reads it back.
set_sysfs() {
  local f=$1 v=$2 old got
  old=$(rd "$f") || die "cannot read $f"
  journaled sysfs "$f" || jrec sysfs "$f" "$old"
  printf '%s\n' "$v" >"$f" 2>"$HW_OUT/write.err" || die "write of '$v' to $f refused: $(cat "$HW_OUT/write.err")"
  got=$(rd "$f")
  [[ $got == "$v" ]] || die "wrote '$v' to $f, read back '$got'"
}

# write_only FILE VALUE writes an attribute whose read back is not the value
# written (pp_dpm_* masks); its restore goes through another journaled file.
write_only() {
  printf '%s\n' "$2" >"$1" 2>"$HW_OUT/write.err" || die "write of '$2' to $1 refused: $(cat "$HW_OUT/write.err")"
}

restore_one() {
  local kind=$1 target=$2 value=$3 got
  case $kind in
    sysfs)
      printf '%s\n' "$value" >"$target" 2>/dev/null || return 1
      got=$(rd "$target") && [[ $got == "$value" ]]
      ;;
    nvrgc) "${NVSMI[@]}" -i "$target" -rgc >>"$HW_OUT/restore.out" 2>&1 ;;
    nvrmc) "${NVSMI[@]}" -i "$target" -rmc >>"$HW_OUT/restore.out" 2>&1 ;;
    nvpl)
      "${NVSMI[@]}" -i "$target" -pl "$value" >>"$HW_OUT/restore.out" 2>&1 || return 1
      got=$("${NVSMI[@]}" --query-gpu=power.limit --format=csv,noheader,nounits -i "$target" 2>/dev/null | tr -d ' ')
      awk -v a="$got" -v b="$value" 'BEGIN { exit !(a != "" && (a - b) * (a - b) < 1) }'
      ;;
    tracefs)
      [[ -d $target ]] || return 0
      echo 0 >"$target/events/power/cpu_frequency/enable" 2>/dev/null
      rmdir "$target" 2>/dev/null
      [[ ! -d $target ]]
      ;;
    mount)
      mountpoint -q "$target" || return 0
      umount "$target"
      ;;
    *) return 1 ;;
  esac
}

# restore_all undoes every journal entry not undone yet, last first, and
# returns 1 when one of them fails.
restore_all() {
  local n kind target value bad=0
  [[ -s $JOURNAL ]] || return 0
  touch "$HW_OUT/journal.done"
  while IFS=$'\t' read -r n kind target value; do
    grep -qxF "$n" "$HW_OUT/journal.done" && continue
    if restore_one "$kind" "$target" "$value"; then
      echo "$n" >>"$HW_OUT/journal.done"
      log "restored $n: $kind $target -> $value"
      printf 'ok\t%s\t%s\t%s\t%s\n' "$n" "$kind" "$target" "$value" >>"$HW_OUT/restore.txt"
    else
      bad=1
      log "RESTORE FAILED $n: $kind $target -> $value"
      printf 'FAILED\t%s\t%s\t%s\t%s\n' "$n" "$kind" "$target" "$value" >>"$HW_OUT/restore.txt"
    fi
  done < <(tac "$JOURNAL")
  return "$bad"
}

# undo KIND TARGET runs the restore of one journal entry now, as a step of
# the measurement, and marks it done.
undo() {
  local n value
  n=$(awk -F'\t' -v k="$1" -v t="$2" '$2 == k && $3 == t { print $1 }' "$JOURNAL" | tail -n1)
  [[ -n $n ]] || return 0
  touch "$HW_OUT/journal.done"
  grep -qxF "$n" "$HW_OUT/journal.done" && return 0
  value=$(awk -F'\t' -v n="$n" '$1 == n { print $4 }' "$JOURNAL")
  if ! restore_one "$1" "$2" "$value"; then
    printf 'FAILED\t%s\t%s\t%s\t%s\n' "$n" "$1" "$2" "$value" >>"$HW_OUT/restore.txt"
    die "undo of $1 $2 failed"
  fi
  echo "$n" >>"$HW_OUT/journal.done"
  printf 'ok\t%s\t%s\t%s\t%s\n' "$n" "$1" "$2" "$value" >>"$HW_OUT/restore.txt"
  log "undone $n: $1 $2 -> $value"
}

# ------------------------------------------------------ snapshot, verify

snap_add() { SNAP_PATHS+=("$@"); }

snap_write() {
  local f v
  : >"$1"
  for f in "${SNAP_PATHS[@]}"; do
    if [[ ! -e $f ]]; then
      v='<absent>'
    else
      v=$(rd "$f") || v='<unreadable>'
    fi
    printf '%s\t%s\n' "$f" "$v" >>"$1"
  done
  if ((${#SNAP_NV_BUSES[@]})); then
    printf '%s\n' "${SNAP_NV_BUSES[@]}" >"$HW_OUT/snapshot.nvbuses"
    nv_stable_query "${SNAP_NV_BUSES[@]}" >"$1.nvidia" 2>&1
  fi
}

# begin_changes marks the end of the preflight: the knobs are read as the
# baseline every later comparison uses.
begin_changes() {
  snap_write "$BASE_SNAP"
  log "baseline of ${#SNAP_PATHS[@]} files and ${#SNAP_NV_BUSES[@]} NVIDIA GPUs in baseline.snap"
}

verify() {
  local path want got bad=0
  : >"$HW_OUT/verify.txt"
  [[ -f $BASE_SNAP ]] || {
    echo "skipped: no baseline (nothing was changed)" >>"$HW_OUT/verify.txt"
    return 0
  }
  while IFS=$'\t' read -r path want; do
    if [[ ! -e $path ]]; then
      got='<absent>'
    else
      got=$(rd "$path") || got='<unreadable>'
    fi
    if [[ $got == "$want" ]]; then
      printf 'ok\t%s\t%s\n' "$path" "$got" >>"$HW_OUT/verify.txt"
    else
      bad=1
      printf 'DIFFERS\t%s\tbaseline=%s\tnow=%s\n' "$path" "$want" "$got" >>"$HW_OUT/verify.txt"
    fi
  done <"$BASE_SNAP"
  if [[ -f $BASE_SNAP.nvidia ]]; then
    mapfile -t SNAP_NV_BUSES <"$HW_OUT/snapshot.nvbuses"
    if nv_stable_query "${SNAP_NV_BUSES[@]}" 2>&1 | cmp -s - "$BASE_SNAP.nvidia"; then
      echo "ok	nvidia-smi query of power limit, persistence, compute and MIG mode" >>"$HW_OUT/verify.txt"
    else
      bad=1
      {
        echo "DIFFERS	nvidia-smi query; baseline:"
        cat "$BASE_SNAP.nvidia"
        echo "now:"
        nv_stable_query "${SNAP_NV_BUSES[@]}" 2>&1
      } >>"$HW_OUT/verify.txt"
    fi
  fi
  if [[ -s $JOURNAL ]] && awk -F'\t' '$2 == "nvrgc" || $2 == "nvrmc"' "$JOURNAL" | grep -q .; then
    echo "note	clock locks have no getter: their reset is checked by its exit status (restore.txt) and the clocks after it" >>"$HW_OUT/verify.txt"
  fi
  return "$bad"
}

# ---------------------------------------------------------------- devices

# pci_norm ID prints a PCI bus id as sysfs spells it (0000:c1:00.0).
pci_norm() {
  local id=${1,,} dom
  [[ $id =~ ^(([0-9a-f]{4,8}):)?([0-9a-f]{2}:[0-9a-f]{2}\.[0-7])$ ]] || return 1
  dom=${BASH_REMATCH[2]:-0000}
  printf '%04x:%s\n' "$((16#$dom))" "${BASH_REMATCH[3]}"
}

# nv_bus ID prints a PCI bus id as nvidia-smi spells it (00000000:C1:00.0).
nv_bus() {
  local s
  s=$(pci_norm "$1") || return 1
  printf '%08X:%s\n' "$((16#${s%%:*}))" "$(tr '[:lower:]' '[:upper:]' <<<"${s#*:}")"
}

pcidir() { printf '%s/bus/pci/devices/%s\n' "$HW_SYS" "$1"; }

is_amdgpu() {
  local d
  d=$(pcidir "$1")
  [[ $(rd "$d/vendor" 2>/dev/null) == 0x1002 ]] || return 1
  if [[ -L $d/driver ]]; then
    [[ $(basename "$(readlink "$d/driver")") == amdgpu ]] || return 1
  fi
  [[ -e $d/power_dpm_force_performance_level ]] || compgen -G "$d/hwmon/hwmon*/power1_cap" >/dev/null
}

amd_all() {
  local d b
  for d in "$HW_SYS"/bus/pci/devices/*; do
    b=$(basename "$d")
    is_amdgpu "$b" && printf '%s\n' "$b"
  done
}

amd_hwmon() {
  local h
  for h in "$(pcidir "$1")"/hwmon/hwmon*; do
    [[ $(rd "$h/name" 2>/dev/null) == amdgpu ]] && {
      printf '%s\n' "$h"
      return 0
    }
  done
  return 1
}

# amd_power_file BDF prints the hwmon file with the GPU's power, in uW.
amd_power_file() {
  local h
  h=$(amd_hwmon "$1") || return 1
  if [[ -e $h/power1_average ]]; then
    printf '%s\n' "$h/power1_average"
  elif [[ -e $h/power1_input ]]; then
    printf '%s\n' "$h/power1_input"
  else
    return 1
  fi
}

amd_level() { printf '%s/power_dpm_force_performance_level\n' "$(pcidir "$1")"; }

amd_product() {
  local d
  d=$(pcidir "$1")
  rd "$d/product_name" 2>/dev/null || printf 'amdgpu device %s\n' "$(rd "$d/device" 2>/dev/null)"
}

# amd_devices DEVICE prints the bus ids DEVICE names: one, or every amdgpu
# device for "all"; refuses a device that is not an amdgpu device.
amd_devices() {
  local b
  if [[ $1 == all ]]; then
    b=$(amd_all)
    [[ -n $b ]] || refuse "no amdgpu device under $HW_SYS/bus/pci/devices"
    printf '%s\n' "$b"
    return
  fi
  b=$(pci_norm "$1") || refuse "device $1 is not a PCI bus id (0000:c1:00.0) or all"
  is_amdgpu "$b" || refuse "$b is not an amdgpu device (vendor 0x1002, driver amdgpu, a level or power cap file)"
  printf '%s\n' "$b"
}

# amd_star FILE prints the MHz of the starred (current DPM target) line of a
# pp_dpm_* file.
amd_star() { awk '/\*/ { for (i = 1; i <= NF; i++) if ($i ~ /[Mm]hz$/) { sub(/[Mm]hz$/, "", $i); print $i; exit } }' "$1" 2>/dev/null; }

cpu_dir() { printf '%s/devices/system/cpu\n' "$HW_SYS"; }

cpu_policies() {
  local p
  for p in "$(cpu_dir)"/cpufreq/policy*; do
    [[ -e $p/scaling_governor ]] && printf '%s\n' "$p"
  done
}

# cpu_policy_of N prints the cpufreq policy directory of CPU N.
cpu_policy_of() {
  local d
  d=$(readlink -f "$(cpu_dir)/cpu$1/cpufreq" 2>/dev/null) || return 1
  [[ -e $d/scaling_governor ]] || return 1
  printf '%s\n' "$d"
}

# cpu_one DEVICE prints N for a device cpuN, after checking it has cpufreq.
cpu_one() {
  [[ $1 =~ ^cpu([0-9]+)$ ]] || refuse "device $1 is not cpuN"
  cpu_policy_of "${BASH_REMATCH[1]}" >/dev/null || refuse "$1 has no cpufreq policy under $(cpu_dir)"
  printf '%s\n' "${BASH_REMATCH[1]}"
}

cpuinfo_file() {
  if [[ -r $HW_HOST/proc/cpuinfo ]]; then
    printf '%s\n' "$HW_HOST/proc/cpuinfo"
  elif [[ -r $HW_HOST/cpuinfo ]]; then # an emulated node's tree
    printf '%s\n' "$HW_HOST/cpuinfo"
  else
    printf '/proc/cpuinfo\n'
  fi
}

cpu_model() { awk -F': ' '/^model name/ { print $2; exit }' "$(cpuinfo_file)"; }

# rapl_zones prints "dir name" for each RAPL package zone and psys.
rapl_zones() {
  local z n
  for z in "$HW_SYS"/class/powercap/intel-rapl:*; do
    [[ $(basename "$z") =~ ^intel-rapl:[0-9]+$ ]] || continue
    n=$(rd "$z/name" 2>/dev/null) || continue
    [[ $n == package-* || $n == psys ]] && printf '%s %s\n' "$z" "$n"
  done
}

# -------------------------------------------------------------- GPU tools

nv_init() {
  if [[ -n ${HW_NVSMI:-} ]]; then
    read -r -a NVSMI <<<"$HW_NVSMI"
  elif command -v nvidia-smi >/dev/null 2>&1; then
    NVSMI=(nvidia-smi)
  elif [[ -x $HW_HOST/usr/bin/nvidia-smi ]]; then
    NVSMI=(chroot "$HW_HOST" /usr/bin/nvidia-smi)
  elif [[ -x $HW_HOST/run/nvidia/driver/usr/bin/nvidia-smi ]]; then
    NVSMI=(chroot "$HW_HOST/run/nvidia/driver" /usr/bin/nvidia-smi)
  else
    return 1
  fi
  printf 'nvidia-smi: %s\n' "${NVSMI[*]}" >>"$HW_OUT/tools.txt"
}

rocm_init() {
  if [[ -n ${HW_ROCMSMI:-} ]]; then
    read -r -a ROCMSMI <<<"$HW_ROCMSMI"
  elif command -v rocm-smi >/dev/null 2>&1; then
    ROCMSMI=(rocm-smi)
  elif [[ -x $HW_HOST/opt/rocm/bin/rocm-smi ]]; then
    ROCMSMI=(chroot "$HW_HOST" /opt/rocm/bin/rocm-smi)
  elif [[ -x $HW_HOST/usr/bin/rocm-smi ]]; then
    ROCMSMI=(chroot "$HW_HOST" /usr/bin/rocm-smi)
  else
    return 1
  fi
  printf 'rocm-smi: %s\n' "${ROCMSMI[*]}" >>"$HW_OUT/tools.txt"
}

nv_q() { # nv_q FIELDS BUS
  "${NVSMI[@]}" --query-gpu="$1" --format=csv,noheader,nounits -i "$2"
}

nv_stable_query() {
  local b
  for b in "$@"; do
    "${NVSMI[@]}" --query-gpu=pci.bus_id,power.limit,persistence_mode,compute_mode,mig.mode.current --format=csv,noheader -i "$b" 2>/dev/null ||
      "${NVSMI[@]}" --query-gpu=pci.bus_id,power.limit,persistence_mode --format=csv,noheader -i "$b"
  done
}

# nv_device DEVICE prints the nvidia-smi bus id of an NVIDIA GPU, refusing
# one nvidia-smi does not list.
nv_device() {
  local b
  b=$(nv_bus "$1") || refuse "device $1 is not a PCI bus id (0000:3b:00.0)"
  nv_q pci.bus_id "$b" >/dev/null 2>&1 || refuse "nvidia-smi lists no GPU $b"
  printf '%s\n' "$b"
}

# nv_gfx_clocks BUS prints the supported graphics clocks, one per line,
# sorted and unique.
nv_gfx_clocks() {
  "${NVSMI[@]}" -q -d SUPPORTED_CLOCKS -i "$1" 2>/dev/null |
    awk '/Graphics/ && /MHz/ { for (i = 1; i <= NF; i++) if ($i ~ /^[0-9]+$/) print $i }' | sort -n -u
}

nv_mem_clocks() {
  "${NVSMI[@]}" -q -d SUPPORTED_CLOCKS -i "$1" 2>/dev/null |
    awk '/^[[:space:]]*Memory/ && /MHz/ { for (i = 1; i <= NF; i++) if ($i ~ /^[0-9]+$/) print $i }' | sort -n -u
}

# nearest FRACTION prints the value of stdin nearest FRACTION x the maximum.
nearest() {
  sort -n | awk -v f="$1" '{ v[NR] = $1 } END {
    if (NR == 0) exit 1
    t = v[NR] * f; b = v[1]
    for (i = 1; i <= NR; i++) if ((v[i] - t) ^ 2 < (b - t) ^ 2) b = v[i]
    print b }'
}

nv_compute_apps() { "${NVSMI[@]}" --query-compute-apps=pid --format=csv,noheader -i "$1" 2>/dev/null | grep -c '[0-9]'; }

nv_lock() { # nv_lock gc|mc BUS MIN,MAX
  local kind=$1 bus=$2 range=$3
  journaled "nvr$kind" "$bus" || jrec "nvr$kind" "$bus" -
  rec "lock-$kind-$(date +%s%N)" "${NVSMI[@]}" -i "$bus" "-l$kind" "$range" || die "nvidia-smi -i $bus -l$kind $range failed (see lock-$kind-*.err)"
  log "locked $kind of $bus to $range"
}

nv_unlock() { undo "nvr$1" "$2"; }

# nv_sample_to OUT PERIOD_MS FIELDS IDS starts an nvidia-smi sampler that
# writes OUT.
nv_sample_to() {
  local out=$1 ms=$2 fields=$3 ids=$4
  setsid "${NVSMI[@]}" --query-gpu="$fields" --format=csv,nounits -lms "$ms" -i "$ids" >"$out" 2>"$out.err" &
  BG_LAST=$!
  BG_PIDS+=("$BG_LAST")
}

# fields_ok BUS FIELDS prints the comma separated fields this nvidia-smi
# accepts for BUS, in order. A clocks_event_reasons field the driver
# rejects (R525 and older) is asked again by its clocks_throttle_reasons
# name, as the agent's discovery does.
fields_ok() {
  local bus=$1 f alt ok=() fs
  IFS=, read -r -a fs <<<"$2"
  for f in "${fs[@]}"; do
    if nv_q "$f" "$bus" >/dev/null 2>&1; then
      ok+=("$f")
    elif [[ $f == clocks_event_reasons.* ]]; then
      alt=clocks_throttle_reasons.${f#clocks_event_reasons.}
      nv_q "$alt" "$bus" >/dev/null 2>&1 && ok+=("$alt")
    fi
  done
  (
    IFS=,
    echo "${ok[*]}"
  )
}

pc_build() {
  if [[ -n ${HW_PEERCOPY:-} ]]; then
    PEERCOPY=$HW_PEERCOPY
  elif command -v nvcc >/dev/null 2>&1; then
    PEERCOPY=/tmp/peercopy
    log "building peercopy with nvcc"
    rec build-peercopy nvcc -O2 -std=c++17 -x cu -o "$PEERCOPY" "$HW_PROBE_DIR/peercopy.cu" \
      -gencode arch=compute_80,code=sm_80 -gencode arch=compute_86,code=sm_86 \
      -gencode arch=compute_89,code=sm_89 -gencode arch=compute_90,code=sm_90 -lpthread ||
      refuse "nvcc failed to build peercopy (build-peercopy.err)"
  elif command -v hipcc >/dev/null 2>&1; then
    PEERCOPY=/tmp/peercopy
    local archs=() a
    IFS=, read -r -a archs <<<"$(opt OFFLOAD_ARCH gfx90a,gfx942,gfx1100)"
    local flags=()
    for a in "${archs[@]}"; do flags+=("--offload-arch=$a"); done
    log "building peercopy with hipcc for ${archs[*]}"
    rec build-peercopy hipcc -x hip -O2 -std=c++17 "${flags[@]}" -o "$PEERCOPY" "$HW_PROBE_DIR/peercopy.cu" -lpthread ||
      refuse "hipcc failed to build peercopy (build-peercopy.err)"
  else
    refuse "this measurement needs peercopy: run it with the cuda or rocm image (nvcc or hipcc)"
  fi
  rec peercopy-list "$PEERCOPY" list || refuse "peercopy list failed (peercopy-list.err)"
  printf 'peercopy: %s\n' "$PEERCOPY" >>"$HW_OUT/tools.txt"
}

need() {
  local t
  for t in "$@"; do
    command -v "$t" >/dev/null 2>&1 || refuse "the image has no $t"
  done
}

# ----------------------------------------------------------------- tracing

# trace_open CPU sets up a tracefs instance that records power:cpu_frequency
# of CPU, journaled so the trap removes it.
trace_open() {
  local cpu=$1 root
  TRACE_DIR=
  case $HW_TRACEFS in
    none)
      log "tracefs off (HW_TRACEFS=none): frequency from scaling_cur_freq samples only"
      return 0
      ;;
    auto)
      if [[ -d $HW_SYS/kernel/tracing/instances ]]; then
        root=$HW_SYS/kernel/tracing
      else
        root=/run/hw-tracefs
        mkdir -p "$root"
        if mount -t tracefs nodev "$root" 2>/dev/null; then
          jrec mount "$root" -
        else
          log "tracefs not available: frequency from scaling_cur_freq samples only"
          return 0
        fi
      fi
      ;;
    *) root=$HW_TRACEFS ;;
  esac
  [[ -e $root/events/power/cpu_frequency ]] || {
    log "no power:cpu_frequency event under $root: frequency from samples only"
    return 0
  }
  TRACE_DIR=$root/instances/joulie-hw-$(date +%s)-$$
  jrec tracefs "$TRACE_DIR" -
  mkdir "$TRACE_DIR" || die "cannot create the tracefs instance $TRACE_DIR"
  echo 2048 >"$TRACE_DIR/buffer_size_kb"
  echo "cpu_id == $cpu" >"$TRACE_DIR/events/power/cpu_frequency/filter" || die "cannot set the trace filter"
  echo 1 >"$TRACE_DIR/events/power/cpu_frequency/enable" || die "cannot enable power:cpu_frequency"
  log "tracing power:cpu_frequency of cpu$cpu in $TRACE_DIR"
}

trace_take() { # trace_take OUT copies and clears the instance's buffer
  [[ -n $TRACE_DIR ]] || return 0
  cat "$TRACE_DIR/trace" >"$1" 2>/dev/null
  : >"$TRACE_DIR/trace" 2>/dev/null
}

# ---------------------------------------------------------------- samplers

# sample_files OUT PERIOD_S FILE... writes "t,<value of each file>" lines.
sample_files() {
  local out=$1 period=$2 f line
  shift 2
  {
    line=t
    for f in "$@"; do line+=",${f#"$HW_SYS"/}"; done
    printf '%s\n' "$line"
    while :; do
      line=$(now)
      for f in "$@"; do line+=",$(rd "$f" 2>/dev/null)"; done
      printf '%s\n' "$line"
      sleep "$period"
    done
  } >"$out"
}

# sample_amd OUT PERIOD_S BDF... writes power (W) and the starred clocks of
# each device.
sample_amd() {
  local out=$1 period=$2 b p line
  shift 2
  {
    line=t
    for b in "$@"; do line+=",$b.power_w,$b.level,$b.sclk,$b.mclk,$b.fclk,$b.socclk"; done
    printf '%s\n' "$line"
    while :; do
      line=$(now)
      for b in "$@"; do
        p=$(rd "$(amd_power_file "$b")" 2>/dev/null)
        line+=",$(awk -v u="$p" 'BEGIN { if (u != "") printf "%.3f", u / 1e6 }'),$(rd "$(amd_level "$b")" 2>/dev/null)"
        line+=",$(amd_star "$(pcidir "$b")/pp_dpm_sclk"),$(amd_star "$(pcidir "$b")/pp_dpm_mclk")"
        line+=",$(amd_star "$(pcidir "$b")/pp_dpm_fclk"),$(amd_star "$(pcidir "$b")/pp_dpm_socclk")"
      done
      printf '%s\n' "$line"
      sleep "$period"
    done
  } >"$out"
}

# energy_read prints "name uj max_uj" for each RAPL zone.
energy_read() {
  local z n
  while read -r z n; do
    printf '%s %s %s\n' "$n" "$(rd "$z/energy_uj")" "$(rd "$z/max_energy_range_uj" 2>/dev/null || echo 0)"
  done < <(rapl_zones)
}

# energy_delta BEFORE AFTER SECONDS prints "name joules watts" per zone,
# allowing one wrap of the counter.
energy_delta() {
  awk -v s="$3" 'NR == FNR { a[$1] = $2; next }
    { d = $2 - a[$1]; if (d < 0) d += $3; printf "%s %.3f %.3f\n", $1, d / 1e6, d / 1e6 / s }' "$1" "$2"
}

# ------------------------------------------------------------ measurements

# hwp-epb: read only. Whether the CPU has HWP (and EPP), and its EPB.
m_hwp_epb() {
  local dev=$1 cf cpus=() c p
  cf=$(cpuinfo_file)
  if [[ $dev == all-cpus ]]; then
    for c in "$(cpu_dir)"/cpu[0-9]*; do cpus+=("${c##*/cpu}"); done
  else
    [[ $dev =~ ^cpu([0-9]+)$ ]] || refuse "device $dev is not cpuN or all-cpus"
    cpus=("${BASH_REMATCH[1]}")
  fi
  stage measuring
  cp "$cf" "$HW_OUT/cpuinfo.txt"
  awk -F': ' '/^processor/ { p = $2 } /^flags/ {
      n = split($2, f, " "); h = ""
      for (i = 1; i <= n; i++) if (f[i] ~ /^(hwp|hwp_epp|hwp_notify|hwp_act_window|hwp_pkg_req|epb)$/) h = h " " f[i]
      print p "," substr(h, 2) }' "$cf" >"$HW_OUT/cpu-flags.csv"
  echo "cpu,energy_perf_bias,scaling_driver,scaling_governor,energy_performance_preference" >"$HW_OUT/epb.csv"
  for c in "${cpus[@]}"; do
    p=$(cpu_policy_of "$c" 2>/dev/null) || p=
    printf 'cpu%s,%s,%s,%s,%s\n' "$c" \
      "$(rd "$(cpu_dir)/cpu$c/power/energy_perf_bias" 2>/dev/null || echo absent)" \
      "$(rd "$p/scaling_driver" 2>/dev/null || echo absent)" \
      "$(rd "$p/scaling_governor" 2>/dev/null || echo absent)" \
      "$(rd "$p/energy_performance_preference" 2>/dev/null || echo absent)" >>"$HW_OUT/epb.csv"
  done
  {
    for f in intel_pstate/status intel_pstate/no_turbo intel_pstate/hwp_dynamic_boost amd_pstate/status cpufreq/boost; do
      printf '%s=%s\n' "$f" "$(rd "$(cpu_dir)/$f" 2>/dev/null || echo absent)"
    done
    p=$(cpu_policy_of "${cpus[0]}" 2>/dev/null) || p=
    printf 'energy_performance_available_preferences=%s\n' "$(rd "$p/energy_performance_available_preferences" 2>/dev/null || echo absent)"
  } >"$HW_OUT/pstate.txt"
  local ncpu nflags hwp epp epb_vals eppf driver
  ncpu=$(grep -c '^processor' "$cf")
  nflags=$(wc -l <"$HW_OUT/cpu-flags.csv")
  hwp=$(grep -c -w hwp "$HW_OUT/cpu-flags.csv")
  epp=$(grep -c -w hwp_epp "$HW_OUT/cpu-flags.csv")
  epb_vals=$(tail -n +2 "$HW_OUT/epb.csv" | cut -d, -f2 | sort | uniq -c | awk '{ printf "%s%s x%s", (NR > 1 ? ", " : ""), $2, $1 }')
  eppf=$(tail -n +2 "$HW_OUT/epb.csv" | cut -d, -f5 | sort -u | paste -sd' ' -)
  driver=$(tail -n +2 "$HW_OUT/epb.csv" | cut -d, -f3 | sort -u | paste -sd' ' -)
  summary "CPU model: $(cpu_model)"
  summary ""
  summary "| Fact | Value |"
  summary "|---|---|"
  summary "| CPUs in cpuinfo, with a flags line | $ncpu, $nflags |"
  summary "| CPUs with the hwp flag | $hwp of $nflags |"
  summary "| CPUs with the hwp_epp flag | $epp of $nflags |"
  summary "| energy_perf_bias (value x CPUs read) | $epb_vals |"
  summary "| scaling_driver | $driver |"
  summary "| energy_performance_preference | $eppf |"
  summary "| intel_pstate/status | $(sed -n 's/^intel_pstate\/status=//p' "$HW_OUT/pstate.txt") |"
  summary ""
  if ((nflags == 0)); then
    summary "cpuinfo has no flags line, so whether the CPU has HWP is unknown: no profile change follows from this run."
    return 0
  fi
  local verdict=no
  if ((hwp > 0)) && [[ $eppf != absent ]]; then verdict=yes; fi
  summary "EPP can be a knob here: $verdict (it needs the hwp flag and an energy_performance_preference file)."
  summary ""
  summary "Profile change: \`cpufreq.hwp: {v: $( ((hwp > 0)) && echo true || echo false), src: \"measured:<id>#hwp flag in /proc/cpuinfo\"}\`."
}

# amdgpu-cap-range: read only. The power cap range of each amdgpu device.
m_amdgpu_cap_range() {
  local devs b h f vals
  local list
  list=$(amd_devices "$1") || exit $?
  mapfile -t devs <<<"$list"
  stage measuring
  echo "bdf,device,product,power1_cap_uw,power1_cap_min_uw,power1_cap_max_uw,power1_cap_default_uw,power1_average_uw,power1_input_uw,runtime_status" >"$HW_OUT/cap-range.csv"
  for b in "${devs[@]}"; do
    h=$(amd_hwmon "$b") || h=/nonexistent
    vals=
    for f in power1_cap power1_cap_min power1_cap_max power1_cap_default power1_average power1_input; do
      if [[ ! -e $h/$f ]]; then
        vals+=",absent"
      else
        vals+=",$(rd "$h/$f" 2>/dev/null || echo unreadable)"
      fi
    done
    printf '%s,%s,%s%s,%s\n' "$b" "$(rd "$(pcidir "$b")/device" 2>/dev/null)" "$(amd_product "$b" | tr ',' ' ')" "$vals" \
      "$(rd "$(pcidir "$b")/power/runtime_status" 2>/dev/null || echo absent)" >>"$HW_OUT/cap-range.csv"
  done
  summary "| Device | PCI device | power1_cap_min (W) | power1_cap_max (W) | power1_cap_default (W) | power1_cap (W) |"
  summary "|---|---|---|---|---|---|"
  tail -n +2 "$HW_OUT/cap-range.csv" | awk -F, '
    function w(v) { return (v ~ /^[0-9]+$/) ? sprintf("%.1f", v / 1e6) : v }
    { printf "| gpu%d | %s | %s | %s | %s | %s |\n", NR - 1, $2, w($5), w($6), w($7), w($4) }' >>"$SUMMARY"
  local mins
  mins=$(tail -n +2 "$HW_OUT/cap-range.csv" | cut -d, -f5 | sort -u)
  summary ""
  if [[ $(wc -l <<<"$mins") == 1 && $mins =~ ^[0-9]+$ ]]; then
    summary "Profile change: \`gpus.limits.minW: {v: $(awk -v u="$mins" 'BEGIN { printf "%g", u / 1e6 }'), src: \"measured:<id>#power1_cap_min, every board\"}\`."
  else
    summary "power1_cap_min differs between devices or is not a number: keep the profile's value and record the table."
  fi
}

# smi-formats: the text the real tools print, for the fake tools of hwemu.
m_smi_formats() {
  local dev=$1 bus='' n=0 writes=1 gc lo hi
  [[ $(opt READ_ONLY 0) == 1 ]] && writes=0
  nv_init || true
  rocm_init || true
  ((${#NVSMI[@]} + ${#ROCMSMI[@]})) || refuse "neither nvidia-smi nor rocm-smi runs here"
  if ((${#NVSMI[@]})) && [[ $dev != all ]]; then
    bus=$(nv_device "$dev") || exit $?
    SNAP_NV_BUSES=("$bus")
  fi
  if ((writes)) && ((${#NVSMI[@]})) && [[ -z $bus ]]; then
    log "device all: the lock commands need one GPU, so only the read-only commands run"
    writes=0
  fi
  if ((writes)) && ((${#NVSMI[@]})); then
    gc=$(nv_gfx_clocks "$bus")
    lo=$(head -n1 <<<"$gc")
    hi=$(tail -n1 <<<"$gc")
    [[ $lo =~ ^[0-9]+$ && $hi =~ ^[0-9]+$ ]] || refuse "no supported graphics clocks for $bus"
    (($(nv_compute_apps "$bus") == 0)) || refuse "$bus runs compute processes"
  fi
  begin_changes
  stage measuring
  local idsel=()
  [[ -n $bus ]] && idsel=(-i "$bus")
  if ((${#NVSMI[@]})); then
    rec "nv-$((n++))-help" "${NVSMI[@]}" -h
    rec "nv-$((n++))-help-query-gpu" "${NVSMI[@]}" --help-query-gpu
    rec "nv-$((n++))-supported-clocks" "${NVSMI[@]}" -q -d SUPPORTED_CLOCKS "${idsel[@]}"
    rec "nv-$((n++))-clock-performance-power" "${NVSMI[@]}" -q -d CLOCK,PERFORMANCE,POWER "${idsel[@]}"
    rec "nv-$((n++))-clock-query-throttle" "${NVSMI[@]}" --query-gpu=pci.bus_id,clocks.sm,clocks.mem,clocks.max.sm,clocks.max.mem,pstate,clocks_throttle_reasons.active --format=csv,noheader,nounits "${idsel[@]}"
    rec "nv-$((n++))-clock-query-event" "${NVSMI[@]}" --query-gpu=pci.bus_id,clocks.sm,clocks.mem,clocks.max.sm,clocks.max.mem,pstate,clocks_event_reasons.active --format=csv,noheader,nounits "${idsel[@]}"
    rec "nv-$((n++))-limits-query" "${NVSMI[@]}" --query-gpu=pci.bus_id,power.limit,power.default_limit,power.min_limit,power.max_limit,enforced.power.limit,mig.mode.current --format=csv "${idsel[@]}"
    if ((writes)); then
      journaled nvrgc "$bus" || jrec nvrgc "$bus" -
      rec "nv-$((n++))-lgc" "${NVSMI[@]}" -i "$bus" -lgc "$lo,$hi"
      rec "nv-$((n++))-rgc" "${NVSMI[@]}" -i "$bus" -rgc
      undo nvrgc "$bus"
      if [[ $(opt WITH_LMC 0) == 1 ]]; then
        local mc mlo mhi
        mc=$(nv_mem_clocks "$bus")
        mlo=$(head -n1 <<<"$mc")
        mhi=$(tail -n1 <<<"$mc")
        journaled nvrmc "$bus" || jrec nvrmc "$bus" -
        rec "nv-$((n++))-lmc" "${NVSMI[@]}" -i "$bus" -lmc "$mlo,$mhi"
        rec "nv-$((n++))-rmc" "${NVSMI[@]}" -i "$bus" -rmc
        undo nvrmc "$bus"
      fi
    fi
  fi
  if ((${#ROCMSMI[@]})); then
    rec "rocm-$((n++))-showclocks-json" "${ROCMSMI[@]}" --showclocks --json
    rec "rocm-$((n++))-showclocks" "${ROCMSMI[@]}" --showclocks
    rec "rocm-$((n++))-showperflevel-json" "${ROCMSMI[@]}" --showperflevel --json
    rec "rocm-$((n++))-showperflevel" "${ROCMSMI[@]}" --showperflevel
    rec "rocm-$((n++))-showpower" "${ROCMSMI[@]}" --showpower
  fi
  summary "| Command | Exit status | Output file |"
  summary "|---|---|---|"
  local c
  for c in "$HW_OUT"/*.cmd; do
    [[ $(basename "$c") == lock-* ]] && continue
    summary "| \`$(head -c 160 "$c" | tr -d '\n' | sed 's/|/\\|/g')\` | $(cat "${c%.cmd}.rc") | $(basename "${c%.cmd}").out |"
  done
  summary ""
  summary "Copy each .out text into the matching fake tool of simulator/pkg/hwemu/smi, replacing the texts its comments mark as assumed; the bus ids, serial numbers and UUIDs in them stay out of the repository."
}

# nvidia-pl-mig: -pl on a GPU with MIG enabled, its exit status and effect.
m_nvidia_pl_mig() {
  local bus mig base min max target rc after
  nv_init || refuse "nvidia-smi does not run here"
  bus=$(nv_device "$1") || exit $?
  SNAP_NV_BUSES=("$bus")
  rec limits-before "${NVSMI[@]}" --query-gpu=pci.bus_id,mig.mode.current,power.limit,power.default_limit,power.min_limit,power.max_limit,enforced.power.limit --format=csv -i "$bus"
  mig=$(nv_q mig.mode.current "$bus" | tr -d ' ')
  [[ $mig == Enabled ]] || refuse "MIG mode of $bus is '$mig', not Enabled: this measurement answers the question only on a MIG-enabled GPU"
  base=$(nv_q power.limit "$bus" | tr -d ' ')
  min=$(nv_q power.min_limit "$bus" | tr -d ' ')
  max=$(nv_q power.max_limit "$bus" | tr -d ' ')
  [[ $base =~ ^[0-9.]+$ ]] || refuse "power.limit of $bus prints '$base', not a number: nothing to restore to"
  target=$(opt WATTS "")
  if [[ -z $target ]]; then
    [[ $min =~ ^[0-9.]+$ ]] || refuse "power.min_limit prints '$min'; give --watts"
    target=$(awk -v b="$base" -v m="$min" 'BEGIN { t = int(b * 0.9); if (t < m) t = int(m + 0.999); print t }')
  fi
  log "baseline power.limit $base W (range $min to $max); -pl $target"
  begin_changes
  stage measuring
  jrec nvpl "$bus" "$base"
  rec pl-set "${NVSMI[@]}" -i "$bus" -pl "$target"
  rc=$?
  nap 2
  rec limits-after "${NVSMI[@]}" --query-gpu=pci.bus_id,mig.mode.current,power.limit,power.default_limit,power.min_limit,power.max_limit,enforced.power.limit --format=csv -i "$bus"
  after=$(nv_q power.limit "$bus" | tr -d ' ')
  undo nvpl "$bus"
  summary "| Step | Value |"
  summary "|---|---|"
  summary "| MIG mode | $mig |"
  summary "| power.limit before | $base W |"
  summary "| command | \`nvidia-smi -i <bus> -pl $target\` |"
  summary "| exit status | $rc |"
  summary "| stdout | $(tr '\n' ' ' <"$HW_OUT/pl-set.out" | sed 's/|/\\|/g') |"
  summary "| stderr | $(tr '\n' ' ' <"$HW_OUT/pl-set.err" | sed 's/|/\\|/g') |"
  summary "| power.limit after | $after W |"
  summary ""
  if [[ $after == "$base" ]]; then
    summary "Effect: none, the limit stayed at $base W."
  else
    summary "Effect: the limit moved from $base W to $after W (target $target W)."
  fi
  summary "This settles how the agent treats -pl on a MIG-enabled GPU (the exit status the fake nvidia-smi returns, cmd/agent's GPU cap path)."
}

# nvidia-pl-na: a refused -pl, with and without --error-on-warning, on a GPU
# whose limits print [N/A].
m_nvidia_pl_na() {
  local bus base def min max target rc1 rc2 restore_to
  nv_init || refuse "nvidia-smi does not run here"
  bus=$(nv_device "$1") || exit $?
  SNAP_NV_BUSES=("$bus")
  rec limits-before "${NVSMI[@]}" --query-gpu=pci.bus_id,power.limit,power.default_limit,power.min_limit,power.max_limit,enforced.power.limit --format=csv -i "$bus"
  base=$(nv_q power.limit "$bus" | tr -d ' ')
  def=$(nv_q power.default_limit "$bus" | tr -d ' ')
  min=$(nv_q power.min_limit "$bus" | tr -d ' ')
  max=$(nv_q power.max_limit "$bus" | tr -d ' ')
  if [[ $min =~ ^[0-9.]+$ && $max =~ ^[0-9.]+$ ]]; then
    refuse "power.min_limit and power.max_limit of $bus are numbers ($min, $max): this measurement is for a GPU whose limits print a token such as [N/A]"
  fi
  if [[ $base =~ ^[0-9.]+$ ]]; then
    restore_to=$base
  elif [[ $def =~ ^[0-9.]+$ ]]; then
    restore_to=$def
  else
    refuse "neither power.limit ('$base') nor power.default_limit ('$def') is a number: a set that is accepted could not be undone"
  fi
  target=$(opt WATTS "$restore_to")
  log "limits: power.limit '$base', default '$def', min '$min', max '$max'; -pl $target, restore to $restore_to"
  begin_changes
  stage measuring
  jrec nvpl "$bus" "$restore_to"
  rec pl-plain "${NVSMI[@]}" -i "$bus" -pl "$target"
  rc1=$?
  rec pl-eow "${NVSMI[@]}" -i "$bus" -pl "$target" --error-on-warning
  rc2=$?
  rec limits-after "${NVSMI[@]}" --query-gpu=pci.bus_id,power.limit,power.default_limit,power.min_limit,power.max_limit,enforced.power.limit --format=csv -i "$bus"
  undo nvpl "$bus"
  summary "| Command | Exit status | stdout | stderr |"
  summary "|---|---|---|---|"
  summary "| \`nvidia-smi -i <bus> -pl $target\` | $rc1 | $(tr '\n' ' ' <"$HW_OUT/pl-plain.out" | sed 's/|/\\|/g') | $(tr '\n' ' ' <"$HW_OUT/pl-plain.err" | sed 's/|/\\|/g') |"
  summary "| \`nvidia-smi -i <bus> -pl $target --error-on-warning\` | $rc2 | $(tr '\n' ' ' <"$HW_OUT/pl-eow.out" | sed 's/|/\\|/g') | $(tr '\n' ' ' <"$HW_OUT/pl-eow.err" | sed 's/|/\\|/g') |"
  summary ""
  summary "Limits before: power.limit '$base', power.default_limit '$def', power.min_limit '$min', power.max_limit '$max'; after: see limits-after.out."
  summary "This replaces the assumed exit statuses of -pl on such a GPU, with and without --error-on-warning, in the fake nvidia-smi (nvSetPowerLimit in simulator/pkg/hwemu/smi/nvidia.go)."
}

# nvidia-lock-floor-idle: idle power and clocks under a clock lock floor:
# a graphics lock (-lgc) by default, with --clock memory a memory lock
# (-lmc), the lock sustained writes on GDDR parts.
m_nvidia_lock_floor_idle() {
  local bus clocks lock max floor s settle fields kind col which
  nv_init || refuse "nvidia-smi does not run here"
  bus=$(nv_device "$1") || exit $?
  SNAP_NV_BUSES=("$bus")
  s=$(opt SAMPLE_S 60)
  settle=$(opt SETTLE_S 5)
  which=$(opt CLOCK graphics)
  case $which in
    graphics) kind=gc col=clocks.sm clocks=$(nv_gfx_clocks "$bus") ;;
    memory) kind=mc col=clocks.mem clocks=$(nv_mem_clocks "$bus") ;;
    *) refuse "--clock $which is not graphics or memory" ;;
  esac
  max=$(tail -n1 <<<"$clocks")
  [[ $max =~ ^[0-9]+$ ]] || refuse "no supported $which clocks for $bus"
  floor=$max
  [[ $kind == gc ]] && floor=$(nearest 0.75 <<<"$clocks")
  lock=$(opt LOCK "$floor,$max")
  [[ $lock =~ ^[0-9]+,[0-9]+$ ]] || refuse "--lock $lock is not MIN,MAX"
  (($(nv_compute_apps "$bus") == 0)) || refuse "$bus runs compute processes: the GPU must be idle"
  fields=$(fields_ok "$bus" timestamp,pstate,clocks.sm,clocks.mem,power.draw,clocks_event_reasons.active)
  log "idle $s s, then -l$kind $lock and idle $s s, then -r$kind and idle $s s; fields $fields"
  begin_changes
  stage "measuring before"
  nv_sample_to "$HW_OUT/idle-before.csv" 1000 "$fields" "$bus"
  nap "$s"
  bg_stop "$BG_LAST"
  stage "measuring locked"
  nv_lock "$kind" "$bus" "$lock"
  nap "$settle"
  nv_sample_to "$HW_OUT/idle-locked.csv" 1000 "$fields" "$bus"
  nap "$s"
  bg_stop "$BG_LAST"
  nv_unlock "$kind" "$bus"
  stage "measuring after"
  nap "$settle"
  nv_sample_to "$HW_OUT/idle-after.csv" 1000 "$fields" "$bus"
  nap "$s"
  bg_stop "$BG_LAST"
  local pcol ccol arm st
  pcol=$(tr ',' '\n' <<<"$fields" | grep -n -x power.draw | cut -d: -f1)
  ccol=$(tr ',' '\n' <<<"$fields" | grep -n -x "$col" | cut -d: -f1)
  summary "| Arm | power.draw median (W) | power.draw mean (W) | $col median (MHz) | samples |"
  summary "|---|---|---|---|---|"
  for arm in before locked after; do
    st=$(stats "$HW_OUT/idle-$arm.csv" "$pcol")
    summary "| $arm | $(field median "$st") | $(field mean "$st") | $(field median "$(stats "$HW_OUT/idle-$arm.csv" "$ccol")") | $(field n "$st") |"
  done
  local lockedc after_c delta
  lockedc=$(field median "$(stats "$HW_OUT/idle-locked.csv" "$ccol")")
  after_c=$(field median "$(stats "$HW_OUT/idle-after.csv" "$ccol")")
  delta=$(awk -v a="$(field median "$(stats "$HW_OUT/idle-before.csv" "$pcol")")" -v b="$(field median "$(stats "$HW_OUT/idle-locked.csv" "$pcol")")" 'BEGIN { if (a != "" && b != "") printf "%.2f", b - a }')
  summary ""
  summary "Lock -l$kind $lock: the idle cost of the lock is $delta W (locked median minus before median)."
  if [[ $kind == gc ]]; then
    summary "An idle GPU at the lower bound of its lock means \`gpus.clocks.lockFloorAtIdle: {v: true, src: \"measured:<id>#idle clocks.sm under -lgc\"}\` (idle clocks.sm under the lock: $lockedc MHz)."
  else
    summary "A memory lock at the maximum is what \`sustained\` writes on a GDDR part, so its idle cost is \`physics.gpu.dataPathHoldW: {v: $delta, src: \"measured:<id>#idle power under -lmc max minus no lock\"}\` (idle clocks.mem under the lock: $lockedc MHz)."
  fi
  if awk -v a="$after_c" -v f="${lock%,*}" 'BEGIN { exit !(a != "" && a + 0 < f + 0) }'; then
    summary "After -r$kind the idle clock ($after_c MHz) is below the lock's floor: the reset took effect."
  else
    summary "After -r$kind the idle clock ($after_c MHz) is not below the lock's floor ${lock%,*} MHz: check the reset by hand."
    echo "after -r$kind the idle $col $after_c MHz is not below the lock's floor ${lock%,*} MHz" >"$HW_OUT/restore-doubt.txt"
  fi
}

# nv_load_sampled OUT FIELDS BUS SECONDS runs the memory-bound load of
# peercopy on BUS for SECONDS while nvidia-smi samples it every 100 ms.
nv_load_sampled() {
  local samp
  nv_sample_to "$1" 100 "$2" "$3"
  samp=$BG_LAST
  bg "$PEERCOPY" stream --device "$3" --seconds "$4"
  wait "$BG_LAST" || die "peercopy stream failed on $3"
  bg_stop "$samp"
}

# nvidia-lock-lifetime: the event reason under -lgc, and whether the lock
# survives every client leaving the GPU.
m_nvidia_lock_lifetime() {
  local bus gc lock f load deinit fields pm apps
  nv_init || refuse "nvidia-smi does not run here"
  bus=$(nv_device "$1") || exit $?
  SNAP_NV_BUSES=("$bus")
  pc_build
  load=$(opt LOAD_S 20)
  deinit=$(opt DEINIT_S 15)
  gc=$(nv_gfx_clocks "$bus")
  f=$(nearest 0.6 <<<"$gc") || refuse "no supported graphics clocks for $bus"
  lock=$(opt LOCK "$f,$f")
  [[ $lock =~ ^[0-9]+,[0-9]+$ ]] || refuse "--lock $lock is not MIN,MAX"
  (($(nv_compute_apps "$bus") == 0)) || refuse "$bus runs compute processes"
  pm=$(nv_q persistence_mode "$bus" | tr -d ' ')
  fields=$(fields_ok "$bus" timestamp,pstate,clocks.sm,clocks.mem,power.draw,clocks_event_reasons.active,clocks_event_reasons.applications_clocks_setting,clocks_event_reasons.sw_power_cap,clocks_event_reasons.hw_slowdown,clocks_event_reasons.sw_thermal_slowdown,clocks_event_reasons.gpu_idle)
  log "persistence mode $pm; -lgc $lock, load $load s, all clients gone for $deinit s, load again"
  begin_changes
  stage "measuring locked load"
  nv_lock gc "$bus" "$lock"
  nv_load_sampled "$HW_OUT/locked-load.csv" "$fields" "$bus" "$load"
  stage "measuring deinit"
  apps=$(nv_compute_apps "$bus")
  log "compute processes on the GPU after the load: $apps; waiting $deinit s without touching the GPU"
  nap "$deinit"
  stage "measuring load after deinit"
  nv_load_sampled "$HW_OUT/after-deinit-load.csv" "$fields" "$bus" "$load"
  nv_unlock gc "$bus"
  stage "measuring after reset"
  nv_load_sampled "$HW_OUT/after-reset-load.csv" "$fields" "$bus" 5
  local ccol rcol c1 c2 c3 reasons survived=no
  ccol=$(tr ',' '\n' <<<"$fields" | grep -n -x clocks.sm | cut -d: -f1)
  rcol=$(tr ',' '\n' <<<"$fields" | grep -n -E -x 'clocks_(event|throttle)_reasons.active' | cut -d: -f1)
  c1=$(field median "$(stats "$HW_OUT/locked-load.csv" "$ccol")")
  c2=$(field median "$(stats "$HW_OUT/after-deinit-load.csv" "$ccol")")
  c3=$(field median "$(stats "$HW_OUT/after-reset-load.csv" "$ccol")")
  reasons=$(tail -n +2 "$HW_OUT/locked-load.csv" | cut -d, -f"${rcol:-1}" | tr -d ' ' | sort | uniq -c | sort -rn | head -n3 | awk '{ printf "%s%s (%s samples)", (NR > 1 ? ", " : ""), $2, $1 }')
  awk -v c="$c2" -v l="${lock#*,}" 'BEGIN { exit !(c != "" && c + 0 <= l + 15) }' && survived=yes
  summary "| Phase | clocks.sm median under load (MHz) |"
  summary "|---|---|"
  summary "| -lgc $lock, first load | $c1 |"
  summary "| same lock, load after $deinit s with no client | $c2 |"
  summary "| after -rgc | $c3 |"
  summary ""
  summary "Persistence mode: $pm. Compute processes left after the first load: $apps."
  summary "clocks_event_reasons.active under the lock (most frequent): ${reasons:-not reported}."
  summary "Lock survived every client leaving: $survived. Profile change: \`gpus.clocks.locksResetOnDeinit: {v: $([[ $survived == yes ]] && echo false || echo true), src: \"measured:<id>#lock lifetime, persistence $pm\"}\`; the reason value settles how a held lock reads back (the NVIDIA clock emulation, simulator/pkg/hwemu/node_nvclock.go)."
  if ! awk -v c="$c3" -v l="${lock#*,}" 'BEGIN { exit !(c != "" && c + 0 > l + 15) }'; then
    echo "after -rgc the clock under load ($c3 MHz) is not above the lock ${lock#*,} MHz" >"$HW_OUT/restore-doubt.txt"
  fi
}

# nvidia-receiver-gate: peer copies into an idle, a busy and a locked
# receiver (hopper-receiver-gate, l40s-receiver-gate).
m_nvidia_receiver_gate() {
  local recv src arms bytes iters issuer cold gc mc arm fields
  nv_init || refuse "nvidia-smi does not run here"
  recv=$(nv_device "$1") || exit $?
  [[ -n ${2:-} ]] || refuse "a receiver test needs --peer, the GPU the copies come from"
  src=$(nv_device "$2") || exit $?
  [[ $src != "$recv" ]] || refuse "--peer is the receiver itself"
  SNAP_NV_BUSES=("$recv" "$src")
  pc_build
  arms=$(opt ARMS "$3")
  bytes=$(opt BYTES 67108864)
  iters=$(opt ITERS 12)
  issuer=$(opt ISSUER src)
  cold=$(opt COLD_S 10)
  gc=$(nv_gfx_clocks "$recv")
  mc=$(nv_mem_clocks "$recv")
  local a
  for a in ${arms//,/ }; do
    case $a in
      default | warm) ;;
      lgc-max | lgc-min) [[ -n $gc ]] || refuse "arm $a needs the supported graphics clocks" ;;
      lmc-max) [[ -n $mc ]] || refuse "arm $a needs the supported memory clocks" ;;
      *) refuse "unknown arm $a (default, warm, lgc-max, lgc-min, lmc-max)" ;;
    esac
  done
  (($(nv_compute_apps "$recv") + $(nv_compute_apps "$src") == 0)) || refuse "the GPUs run compute processes"
  fields=$(fields_ok "$recv" timestamp,pci.bus_id,pstate,clocks.sm,clocks.mem,power.draw,clocks_event_reasons.active)
  log "receiver $recv, source $src, arms $arms, $iters copies of $bytes bytes issued from the $issuer"
  begin_changes
  echo "arm,iter,bytes,ms,gbps" >"$HW_OUT/copies.csv"
  for arm in ${arms//,/ }; do
    stage "measuring $arm"
    local load=none
    case $arm in
      warm) load=stream ;;
      lgc-max) nv_lock gc "$recv" "$(tail -n1 <<<"$gc"),$(tail -n1 <<<"$gc")" ;;
      lgc-min) nv_lock gc "$recv" "$(head -n1 <<<"$gc"),$(head -n1 <<<"$gc")" ;;
      lmc-max) nv_lock mc "$recv" "$(tail -n1 <<<"$mc"),$(tail -n1 <<<"$mc")" ;;
    esac
    nv_sample_to "$HW_OUT/arm-$arm-smi.csv" 10 "$fields" "$recv,$src"
    local samp=$BG_LAST
    bg "$PEERCOPY" copy --src "$src" --dst "$recv" --bytes "$bytes" --iters "$iters" --issuer "$issuer" \
      --cold-s "$cold" --receiver-load "$load" >"$HW_OUT/arm-$arm.txt" 2>"$HW_OUT/arm-$arm.err"
    wait "$BG_LAST" || die "peercopy failed in arm $arm (arm-$arm.err)"
    bg_stop "$samp"
    awk -v a="$arm" '/^copy / { for (i = 2; i <= NF; i++) { split($i, kv, "="); v[kv[1]] = kv[2] }
      print a "," v["iter"] "," v["bytes"] "," v["ms"] "," v["gbps"] }' "$HW_OUT/arm-$arm.txt" >>"$HW_OUT/copies.csv"
    case $arm in
      lgc-*) nv_unlock gc "$recv" ;;
      lmc-*) nv_unlock mc "$recv" ;;
    esac
  done
  receiver_summary
}

# receiver_summary writes the copy table of copies.csv and the gate facts.
receiver_summary() {
  local arm st med best=0 first
  summary "| Receiver arm | copies | median GB/s | min GB/s | max GB/s | first copy GB/s |"
  summary "|---|---|---|---|---|---|"
  for arm in $(tail -n +2 "$HW_OUT/copies.csv" | cut -d, -f1 | awk '!s[$0]++'); do
    grep "^$arm," "$HW_OUT/copies.csv" >"$HW_OUT/.arm"
    st=$(stats "$HW_OUT/.arm" 5)
    med=$(field median "$st")
    first=$(head -n1 "$HW_OUT/.arm" | cut -d, -f5)
    summary "| $arm | $(field n "$st") | $med | $(field min "$st") | $(field max "$st") | $first |"
    best=$(awk -v a="$best" -v b="$med" 'BEGIN { print (b + 0 > a + 0) ? b : a }')
    printf '%s %s\n' "$arm" "$med" >>"$HW_OUT/.medians"
  done
  rm -f "$HW_OUT/.arm"
  summary ""
  local cold
  cold=$(awk '$1 == "default" || $1 == "idle" { print $2; exit }' "$HW_OUT/.medians")
  if [[ -n $cold ]]; then
    local frac
    frac=$(awk -v c="$cold" -v b="$best" 'BEGIN { if (b > 0) printf "%.2f", c / b }')
    summary "Idle receiver over the best arm: $frac. Below about 0.9 the family gates inbound copies on an idle receiver: \`gpus.transfer.receiverGate: {enabled: {v: true, ...}, slowFraction: {v: $frac, src: \"measured:<id>#median of the idle arm over the best arm\"}}\`; at about 1.0, \`enabled: {v: false, src: \"measured:<id>#no gate\"}\`."
    summary "A gate measured on lab hardware goes into a private profile under SIM_PROFILES_DIR until the user approves publishing it (the receiver gate stays off in every repository profile)."
  fi
  rm -f "$HW_OUT/.medians"
}

# instinct-receiver-gate: peer copies into an idle and a busy AMD receiver.
m_instinct_receiver_gate() {
  local recv src arms bytes iters issuer cold arm lvl
  recv=$(amd_devices "$1") || exit $?
  [[ -n ${2:-} ]] || refuse "a receiver test needs --peer, the GPU the copies come from"
  src=$(amd_devices "$2") || exit $?
  [[ $src != "$recv" ]] || refuse "--peer is the receiver itself"
  pc_build
  arms=$(opt ARMS idle,busy)
  bytes=$(opt BYTES 67108864)
  iters=$(opt ITERS 12)
  issuer=$(opt ISSUER src)
  cold=$(opt COLD_S 10)
  lvl=$(rd "$(amd_level "$recv")") || refuse "cannot read the level of $recv"
  [[ $lvl == auto ]] || refuse "the receiver's level is '$lvl': the gate is measured from auto"
  for arm in ${arms//,/ }; do
    case $arm in idle | busy | high) ;; *) refuse "unknown arm $arm (idle, busy, high)" ;; esac
  done
  snap_add "$(amd_level "$recv")" "$(amd_level "$src")"
  begin_changes
  echo "arm,iter,bytes,ms,gbps" >"$HW_OUT/copies.csv"
  for arm in ${arms//,/ }; do
    stage "measuring $arm"
    local load=none
    case $arm in
      busy) load=stream ;;
      high) set_sysfs "$(amd_level "$recv")" high ;;
    esac
    bgf sample_amd "$HW_OUT/arm-$arm-amd.csv" 0.05 "$recv" "$src"
    local samp=$BG_LAST
    bg "$PEERCOPY" copy --src "$src" --dst "$recv" --bytes "$bytes" --iters "$iters" --issuer "$issuer" \
      --cold-s "$cold" --receiver-load "$load" >"$HW_OUT/arm-$arm.txt" 2>"$HW_OUT/arm-$arm.err"
    wait "$BG_LAST" || die "peercopy failed in arm $arm (arm-$arm.err)"
    bg_stop "$samp"
    awk -v a="$arm" '/^copy / { for (i = 2; i <= NF; i++) { split($i, kv, "="); v[kv[1]] = kv[2] }
      print a "," v["iter"] "," v["bytes"] "," v["ms"] "," v["gbps"] }' "$HW_OUT/arm-$arm.txt" >>"$HW_OUT/copies.csv"
    [[ $arm == high ]] && undo sysfs "$(amd_level "$recv")"
  done
  receiver_summary
}

# amd_arms_power BDFS SAMPLE_S ARM writes arm power samples of devices.
amd_power_arm() {
  local arm=$1 s=$2
  shift 2
  stage "measuring $arm"
  bgf sample_amd "$HW_OUT/power-$arm.csv" 1 "$@"
  nap "$s"
  bg_stop "$BG_LAST"
}

# delta_median FILE LEVEL prints the median of the "LEVEL delta" lines.
delta_median() {
  awk -v l="$2" '$1 == l && $2 ~ /^-?[0-9.]+$/ { print $2 }' "$1" | sort -g |
    awk '{ v[NR] = $1 } END { if (NR) printf "%.2f", (NR % 2) ? v[(NR + 1) / 2] : (v[NR / 2] + v[NR / 2 + 1]) / 2 }'
}

# amd_set_levels LEVEL BDF... writes LEVEL to each device's level file.
amd_set_levels() {
  local l=$1 b
  shift
  for b in "$@"; do set_sysfs "$(amd_level "$b")" "$l"; done
}

amd_undo_levels() {
  local b
  for b in "$@"; do undo sysfs "$(amd_level "$b")"; done
}

# w7900-level-power: GPU power at each forced level (high by default)
# against auto, idle and under a memory-bound kernel.
m_w7900_level_power() {
  local devs b s settle load lvl levels l list
  list=$(amd_devices "$1") || exit $?
  mapfile -t devs <<<"$list"
  s=$(opt SAMPLE_S 120)
  settle=$(opt SETTLE_S 10)
  load=$(opt LOAD vram)
  levels=$(opt LEVELS high)
  for b in "${devs[@]}"; do
    lvl=$(rd "$(amd_level "$b")") || refuse "cannot read the level of $b"
    [[ $lvl == auto ]] || refuse "the level of $b is '$lvl': the comparison starts from auto"
    amd_power_file "$b" >/dev/null || refuse "$b has no power1_average or power1_input"
    snap_add "$(amd_level "$b")"
  done
  case $load in
    vram) pc_build ;;
    none) ;;
    *) refuse "--load $load is not vram or none" ;;
  esac
  log "${#devs[@]} devices, levels $levels against auto, $s s per arm, load $load"
  begin_changes
  local phase
  for phase in idle load; do
    [[ $phase == load && $load != vram ]] && continue
    local pids=()
    if [[ $phase == load ]]; then
      local n
      n=$(tr ',' '\n' <<<"$levels" | grep -c .)
      for b in "${devs[@]}"; do
        bg "$PEERCOPY" stream --device "$b" --seconds "$(((n + 1) * (s + 2 * settle) + 60))"
        pids+=("$BG_LAST")
      done
      nap "$settle"
    fi
    amd_power_arm "$phase-auto" "$s" "${devs[@]}"
    for l in ${levels//,/ }; do
      amd_set_levels "$l" "${devs[@]}"
      nap "$settle"
      amd_power_arm "$phase-$l" "$s" "${devs[@]}"
      amd_undo_levels "${devs[@]}"
      nap "$settle"
    done
    for b in "${pids[@]}"; do bg_stop "$b"; done
  done
  local i=0 col
  summary "Device: $(amd_product "${devs[0]}"), ${#devs[@]} measured. Power is power1_average (or power1_input), the median of samples every second for $s s per arm."
  summary ""
  summary "| GPU | level | idle auto (W) | idle at level (W) | idle delta (W) | loaded auto (W) | loaded at level (W) | loaded delta (W) |"
  summary "|---|---|---|---|---|---|---|---|"
  : >"$HW_OUT/.deltas"
  for b in "${devs[@]}"; do
    col=$((2 + 6 * i))
    for l in ${levels//,/ }; do
      local m=() arm
      for arm in idle-auto "idle-$l" load-auto "load-$l"; do
        if [[ -f $HW_OUT/power-$arm.csv ]]; then m+=("$(field median "$(stats "$HW_OUT/power-$arm.csv" "$col")")"); else m+=(""); fi
      done
      local d1 d2
      d1=$(awk -v a="${m[0]}" -v b="${m[1]}" 'BEGIN { if (a != "" && b != "") printf "%.2f", b - a }')
      d2=$(awk -v a="${m[2]}" -v b="${m[3]}" 'BEGIN { if (a != "" && b != "") printf "%.2f", b - a }')
      printf '%s %s\n' "$l" "$d1" >>"$HW_OUT/.deltas"
      summary "| gpu$i | $l | ${m[0]} | ${m[1]} | $d1 | ${m[2]} | ${m[3]} | $d2 |"
    done
    i=$((i + 1))
  done
  local hold peak
  hold=$(delta_median "$HW_OUT/.deltas" high)
  peak=$(delta_median "$HW_OUT/.deltas" profile_peak)
  rm -f "$HW_OUT/.deltas"
  summary ""
  if [[ -n $hold ]]; then
    summary "The idle delta at high is what holding the data path costs an idle GPU: \`physics.gpu.dataPathHoldW: {v: $hold, src: \"measured:<id>#median over GPUs of idle power at high minus auto\"}\`."
  fi
  if [[ -n $peak && -n $hold ]]; then
    summary "profile_peak also turns idle gating off: \`physics.gpu.gatingOffIdleW: {v: $(awk -v p="$peak" -v h="$hold" 'BEGIN { printf "%.2f", p - h }'), src: \"measured:<id>#idle power at profile_peak minus high\"}\`."
  fi
}

# fabric-floor: the fabric clock floor alone (manual, top fclk) against
# high and auto, idle power and peer copies into the receiver.
m_fabric_floor() {
  local recv src s bytes iters top lvl arm f
  recv=$(amd_devices "$1") || exit $?
  [[ -n ${2:-} ]] || refuse "fabric-floor needs --peer, the GPU the copies come from"
  src=$(amd_devices "$2") || exit $?
  [[ $src != "$recv" ]] || refuse "--peer is the receiver itself"
  pc_build
  s=$(opt SAMPLE_S 60)
  bytes=$(opt BYTES 67108864)
  iters=$(opt ITERS 12)
  lvl=$(rd "$(amd_level "$recv")") || refuse "cannot read the level of $recv"
  [[ $lvl == auto ]] || refuse "the receiver's level is '$lvl': the arms start from auto"
  f=$(pcidir "$recv")/pp_dpm_fclk
  [[ -r $f ]] || refuse "$recv has no pp_dpm_fclk"
  top=$(awk -F: '{ gsub(/[^0-9]/, "", $2); if ($2 + 0 >= m) { m = $2 + 0; i = $1 } } END { print i }' "$f")
  [[ $top =~ ^[0-9]+$ ]] || refuse "cannot find the top level of $f"
  amd_power_file "$recv" >/dev/null || refuse "$recv has no power1_average or power1_input"
  cp "$f" "$HW_OUT/pp_dpm_fclk.before"
  snap_add "$(amd_level "$recv")" "$(amd_level "$src")"
  log "receiver $recv, source $src, top fclk level $top"
  begin_changes
  echo "arm,iter,bytes,ms,gbps" >"$HW_OUT/copies.csv"
  for arm in auto high fclk-max; do
    case $arm in
      high) set_sysfs "$(amd_level "$recv")" high ;;
      fclk-max)
        set_sysfs "$(amd_level "$recv")" manual
        write_only "$f" "$top"
        cp "$f" "$HW_OUT/pp_dpm_fclk.forced"
        ;;
    esac
    nap 5
    amd_power_arm "idle-$arm" "$s" "$recv"
    stage "measuring copies $arm"
    bgf sample_amd "$HW_OUT/copy-$arm-amd.csv" 0.05 "$recv" "$src"
    local samp=$BG_LAST
    bg "$PEERCOPY" copy --src "$src" --dst "$recv" --bytes "$bytes" --iters "$iters" --issuer src \
      --cold-s 2 --receiver-load none >"$HW_OUT/arm-$arm.txt" 2>"$HW_OUT/arm-$arm.err"
    wait "$BG_LAST" || die "peercopy failed in arm $arm (arm-$arm.err)"
    bg_stop "$samp"
    awk -v a="$arm" '/^copy / { for (i = 2; i <= NF; i++) { split($i, kv, "="); v[kv[1]] = kv[2] }
      print a "," v["iter"] "," v["bytes"] "," v["ms"] "," v["gbps"] }' "$HW_OUT/arm-$arm.txt" >>"$HW_OUT/copies.csv"
    [[ $arm != auto ]] && undo sysfs "$(amd_level "$recv")"
  done
  cp "$f" "$HW_OUT/pp_dpm_fclk.after"
  summary "| Receiver arm | idle power median (W) |"
  summary "|---|---|"
  for arm in auto high fclk-max; do
    summary "| $arm | $(field median "$(stats "$HW_OUT/power-idle-$arm.csv" 2)") |"
  done
  summary ""
  receiver_summary
  summary "If fclk-max copies as fast as high at a lower idle power, a cheaper \`sustained\` mapping for this family is manual with only the fabric clock forced (pkg/governor, PlanAMD)."
}

# cpu_set_governor GOV writes GOV to every policy.
cpu_set_governor() {
  local p
  for p in $(cpu_policies); do set_sysfs "$p/scaling_governor" "$1"; done
}

# cpu-governor-energy: package energy per governor at steady loads.
m_cpu_governor_energy() {
  local govs loads settle m avail g l pols=() p ok=()
  [[ $1 == all-cpus ]] || refuse "device $1: cpu-governor-energy sets every policy, the device is all-cpus"
  need stress-ng
  mapfile -t pols < <(cpu_policies)
  ((${#pols[@]})) || refuse "no cpufreq policy under $(cpu_dir)/cpufreq"
  [[ -n $(rapl_zones) ]] || refuse "no RAPL package zone under $HW_SYS/class/powercap"
  rd "$(rapl_zones | head -n1 | cut -d' ' -f1)/energy_uj" >/dev/null || refuse "energy_uj is not readable"
  govs=$(opt GOVERNORS performance,schedutil,conservative)
  loads=$(opt LOADS 0,30,70,100)
  settle=$(opt SETTLE_S 10)
  m=$(opt MEASURE_S 60)
  avail=$(rd "${pols[0]}/scaling_available_governors")
  for g in ${govs//,/ }; do
    if [[ " $avail " == *" $g "* ]]; then ok+=("$g"); else log "governor $g is not available ($avail): skipped"; fi
  done
  ((${#ok[@]})) || refuse "none of $govs is available ($avail)"
  for p in "${pols[@]}"; do
    snap_add "$p/scaling_governor" "$p/scaling_min_freq" "$p/scaling_max_freq" "$p/energy_performance_preference"
  done
  snap_add "$(cpu_dir)/cpufreq/boost" "$(cpu_dir)/intel_pstate/no_turbo"
  log "${#pols[@]} policies, driver $(rd "${pols[0]}/scaling_driver"), governors ${ok[*]}, loads $loads, $settle s settle and $m s per cell"
  begin_changes
  echo "governor,load_pct,zone,joules,watts,seconds,bogo_ops" >"$HW_OUT/energy.csv"
  for g in "${ok[@]}"; do
    cpu_set_governor "$g"
    for l in ${loads//,/ }; do
      stage "measuring $g at $l %"
      local cell=$g-$l pid=
      if ((l > 0)); then
        bg stress-ng --cpu 0 --cpu-load "$l" -t "$((settle + m + 1))" --metrics-brief --yaml "$HW_OUT/stress-$cell.yaml"
        pid=$BG_LAST
      fi
      nap "$settle"
      energy_read >"$HW_OUT/.e0"
      local t0 t1
      t0=$(now)
      bgf sample_files "$HW_OUT/freq-$cell.csv" 1 "${pols[@]/%//scaling_cur_freq}"
      nap "$m"
      energy_read >"$HW_OUT/.e1"
      t1=$(now)
      bg_stop "$BG_LAST"
      if [[ -n $pid ]]; then
        wait "$pid" || die "stress-ng failed at $g, $l % (see the pod log)"
      fi
      local secs ops
      secs=$(awk -v a="$t0" -v b="$t1" 'BEGIN { printf "%.3f", b - a }')
      ops=$(awk '/bogo-ops:/ { s += $2 } END { if (NR) print s }' "$HW_OUT/stress-$cell.yaml" 2>/dev/null)
      energy_delta "$HW_OUT/.e0" "$HW_OUT/.e1" "$secs" | while read -r z j w; do
        printf '%s,%s,%s,%s,%s,%s,%s\n' "$g" "$l" "$z" "$j" "$w" "$secs" "$ops" >>"$HW_OUT/energy.csv"
      done
    done
  done
  for p in "${pols[@]}"; do undo sysfs "$p/scaling_governor"; done
  rm -f "$HW_OUT/.e0" "$HW_OUT/.e1"
  summary "CPU: $(cpu_model); driver $(rd "${pols[0]}/scaling_driver"); ${#pols[@]} policies. Package power is the sum of the package zones over $m s after $settle s at each load."
  summary ""
  summary "| Governor | load % | package W | psys W | stress-ng bogo ops |"
  summary "|---|---|---|---|---|"
  awk -F, 'NR > 1 { k = $1 "," $2; if ($3 ~ /^package/) pk[k] += $5; else if ($3 == "psys") ps[k] = $5; ops[k] = $7; if (!(k in seen)) { seen[k] = 1; ord[++n] = k } }
    END { for (i = 1; i <= n; i++) { split(ord[i], a, ","); printf "| %s | %s | %.2f | %s | %s |\n", a[1], a[2], pk[ord[i]], (ord[i] in ps) ? sprintf("%.2f", ps[ord[i]]) : "", ops[ord[i]] } }' "$HW_OUT/energy.csv" >>"$SUMMARY"
  local idle_perf idle_low
  idle_perf=$(awk -F, '$1 == "performance" && $2 == 0 && $3 ~ /^package/ { s += $5 } END { if (NR) printf "%.2f", s }' "$HW_OUT/energy.csv")
  idle_low=$(awk -F, '$1 != "performance" && $2 == 0 && $3 ~ /^package/ { s[$1] += $5 } END { m = ""; for (g in s) if (m == "" || s[g] < m) m = s[g]; if (m != "") printf "%.2f", m }' "$HW_OUT/energy.csv")
  summary ""
  if [[ -n $idle_perf && -n $idle_low ]]; then
    summary "Idle: performance $idle_perf W against $idle_low W for the lowest demand-driven governor. \`physics.cpu.idleGovernorDeltaW: {v: $(awk -v a="$idle_perf" -v b="$idle_low" 'BEGIN { d = a - b; printf "%.2f", (d > 0 ? d : 0) }'), src: \"measured:<id>#idle package power under performance minus the demand-driven governor\"}\`."
  fi
  summary "Energy per bogo op per governor and load answers Q1 on this driver; the loaded rows feed the sweep's CPU arms."
}

# freq_steps POLICY GOV LOADS CPU writes the frequency trace of each load.
freq_steps() {
  local cpu=$1 gov=$2 loads=$3 m pol l avail fmax cell
  need stress-ng
  pol=$(cpu_policy_of "$cpu")
  m=$(opt MEASURE_S 30)
  avail=$(rd "$pol/scaling_available_governors")
  [[ " $avail " == *" $gov "* ]] || refuse "governor $gov is not available on cpu$cpu ($avail)"
  fmax=$(rd "$pol/cpuinfo_max_freq")
  rd "$pol/scaling_available_frequencies" >"$HW_OUT/available-frequencies.txt" 2>/dev/null ||
    echo none >"$HW_OUT/available-frequencies.txt"
  snap_add "$pol/scaling_governor" "$pol/scaling_min_freq" "$pol/scaling_max_freq"
  log "cpu$cpu (policy ${pol##*/}, driver $(rd "$pol/scaling_driver"), governor now $(rd "$pol/scaling_governor")), $gov, loads $loads, $m s each"
  begin_changes
  [[ $(rd "$pol/scaling_governor") == "$gov" ]] || set_sysfs "$pol/scaling_governor" "$gov"
  trace_open "$cpu"
  for l in ${loads//,/ }; do
    stage "measuring $gov at $l %"
    cell=$gov-$l
    trace_take /dev/null
    bgf sample_files "$HW_OUT/freq-$cell.csv" 0.1 "$pol/scaling_cur_freq"
    local samp=$BG_LAST
    bg stress-ng --cpu 1 --cpu-load "$l" --taskset "$cpu" -t "$m" --metrics-brief
    wait "$BG_LAST" || die "stress-ng failed at $l % (see the pod log)"
    bg_stop "$samp"
    trace_take "$HW_OUT/trace-$cell.txt"
  done
  [[ -n $TRACE_DIR ]] && undo tracefs "$TRACE_DIR"
  undo sysfs "$pol/scaling_governor"
  summary "cpu$cpu, driver $(rd "$pol/scaling_driver"), cpuinfo_max_freq $fmax kHz, available frequencies: $(cat "$HW_OUT/available-frequencies.txt")."
  summary ""
  summary "| Governor, load | frequency kHz: share of samples | trace events by target kHz |"
  summary "|---|---|---|"
  for l in ${loads//,/ }; do
    cell=$gov-$l
    local res ev
    res=$(tail -n +2 "$HW_OUT/freq-$cell.csv" | cut -d, -f2 | grep -E '^[0-9]+$' | sort | uniq -c | sort -k2 -n |
      awk '{ c[NR] = $1; f[NR] = $2; t += $1 } END { for (i = 1; i <= NR; i++) printf "%s%s: %.0f %%", (i > 1 ? ", " : ""), f[i], 100 * c[i] / t }')
    ev=$(grep -o 'state=[0-9]*' "$HW_OUT/trace-$cell.txt" 2>/dev/null | cut -d= -f2 | sort | uniq -c | sort -k2 -n |
      awk '{ printf "%s%s x%s", (NR > 1 ? ", " : ""), $2, $1 }')
    summary "| $gov, $l % | ${res:-no samples} | ${ev:-no trace} |"
  done
  summary ""
}

# schedutil-steps: which table entry schedutil picks at a steady load.
m_schedutil_steps() {
  local cpu
  cpu=$(cpu_one "$1") || exit $?
  freq_steps "$cpu" schedutil "$(opt LOADS 35)"
  local l fmax
  fmax=$(rd "$(cpu_policy_of "$cpu")/cpuinfo_max_freq")
  for l in $(opt LOADS 35 | tr ',' ' '); do
    summary "At $l % the schedutil rule asks 1.25 x $fmax x $l / 100 = $(awk -v f="$fmax" -v u="$l" 'BEGIN { printf "%d", 1.25 * f * u / 100 }') kHz and resolves to the lowest entry at or above it (phys.GovernorStep); compare with the most frequent entry above."
  done
}

# conservative-steps: whether 5 % steps move between the entries of a
# short table or stall.
m_conservative_steps() {
  local cpu
  cpu=$(cpu_one "$1") || exit $?
  freq_steps "$cpu" conservative "$(opt LOADS 30,70)"
  summary "conservative moves by freq_step % of scaling_max_freq per sampling period; on a 3-entry table the residency above shows whether the steps reach the middle and top entries or stay at the lowest (the hwemu GovernorStep rule)."
}

# ------------------------------------------------------------ entry points

# on_term: TERM, INT or HUP. A stop from hwmeasure.sh (probe.sh stop leaves
# $HW_OUT/STOP) restores and then still waits for the results to be
# collected; any other signal (the kubelet deleting the pod) and any second
# one exit as soon as the restore is done.
on_term() {
  if ((IN_EXIT)); then
    STOP_REQ=1
    return
  fi
  [[ -e $HW_OUT/STOP ]] || STOP_REQ=1
  log "signal received: stopping and restoring"
  exit 143
}

on_exit() {
  local rc=$? r=nothing v=skipped
  ((IN_EXIT)) && return
  IN_EXIT=1
  stage restoring
  bg_stop_all
  if ((CHANGED)) || [[ -s $JOURNAL ]]; then
    if restore_all; then r=ok; else r=failed; fi
  fi
  if [[ -f $BASE_SNAP ]]; then
    if verify; then v=ok; else v=failed; fi
  fi
  [[ -f $HW_OUT/restore-doubt.txt ]] && v="$v,doubt"
  printf 'rc=%s restore=%s verify=%s\n' "$rc" "$r" "$v" >"$HW_OUT/STATUS.tmp"
  mv "$HW_OUT/STATUS.tmp" "$HW_OUT/STATUS"
  stage finished
  log "finished: rc=$rc restore=$r verify=$v"
  local deadline=$((SECONDS + HW_LINGER_S))
  while ((!STOP_REQ)) && [[ ! -e $HW_OUT/COLLECTED ]] && ((SECONDS < deadline)); do
    nap 1
  done
  exit "$rc"
}

run() {
  local m=${1:-} dev=${2:-} peer=${3:-}
  [[ -n $m && -n $dev ]] || {
    echo "usage: probe.sh run MEASUREMENT DEVICE [PEER]" >&2
    exit 2
  }
  mkdir -p "$HW_OUT"
  rm -f "$HW_OUT/STATUS" "$HW_OUT/COLLECTED" "$HW_OUT/STOP"
  echo $$ >"$HW_OUT/probe.pid"
  : >"$HW_OUT/tools.txt"
  trap on_exit EXIT
  trap on_term TERM INT HUP
  stage preflight
  log "measurement $m, device $dev${peer:+, peer $peer}"
  {
    echo "# $m"
    echo
    echo "Measured $(date -u +%Y-%m-%dT%H:%M:%SZ) by experiments/05-governor-exploration/hardware. Replace <id> with a source id of the profile's sources block, {kind: measured, ref: \"$m on a node with <count>x <model>, <YYYY-MM>\"}; never the node name."
    echo
  } >"$SUMMARY"
  case $m in
    hwp-epb) m_hwp_epb "$dev" ;;
    amdgpu-cap-range) m_amdgpu_cap_range "$dev" ;;
    smi-formats) m_smi_formats "$dev" ;;
    nvidia-pl-mig) m_nvidia_pl_mig "$dev" ;;
    nvidia-pl-na) m_nvidia_pl_na "$dev" ;;
    nvidia-lock-floor-idle) m_nvidia_lock_floor_idle "$dev" ;;
    nvidia-lock-lifetime) m_nvidia_lock_lifetime "$dev" ;;
    hopper-receiver-gate) m_nvidia_receiver_gate "$dev" "$peer" default,warm,lgc-max,lgc-min ;;
    l40s-receiver-gate) m_nvidia_receiver_gate "$dev" "$peer" default,warm,lmc-max ;;
    instinct-receiver-gate) m_instinct_receiver_gate "$dev" "$peer" ;;
    w7900-level-power) m_w7900_level_power "$dev" ;;
    fabric-floor) m_fabric_floor "$dev" "$peer" ;;
    cpu-governor-energy) m_cpu_governor_energy "$dev" ;;
    schedutil-steps) m_schedutil_steps "$dev" ;;
    conservative-steps) m_conservative_steps "$dev" ;;
    *) refuse "unknown measurement $m" ;;
  esac
  log "measurement done"
  exit 0
}

case ${1:-} in
  run)
    shift
    run "$@"
    ;;
  status)
    if [[ -f $HW_OUT/STATUS ]]; then
      echo "done $(cat "$HW_OUT/STATUS")"
    elif [[ -f $HW_OUT/probe.pid ]] && kill -0 "$(cat "$HW_OUT/probe.pid")" 2>/dev/null; then
      echo "running $(cat "$HW_OUT/stage" 2>/dev/null)"
    else
      echo absent
    fi
    ;;
  stop)
    if [[ ! -f $HW_OUT/STATUS ]]; then
      touch "$HW_OUT/STOP"
      kill -TERM "$(cat "$HW_OUT/probe.pid" 2>/dev/null)" 2>/dev/null
    fi
    for ((i = 0; i < HW_STOP_S * 2; i++)); do
      [[ -f $HW_OUT/STATUS ]] && break
      sleep 0.5
    done
    if [[ -f $HW_OUT/STATUS ]]; then echo "done $(cat "$HW_OUT/STATUS")"; else
      echo "no status after $HW_STOP_S s"
      exit 1
    fi
    ;;
  verify)
    nv_init >/dev/null 2>&1 || true
    if verify; then
      cat "$HW_OUT/verify.txt"
    else
      cat "$HW_OUT/verify.txt"
      exit 1
    fi
    ;;
  pack) tar -C "$HW_OUT" -cf - . ;;
  release) touch "$HW_OUT/COLLECTED" ;;
  -h | --help) awk 'NR > 2 && /^#/ { sub(/^# ?/, ""); print; next } NR > 2 { exit }' "$0" ;;
  *)
    echo "usage: probe.sh run|status|stop|verify|pack|release (see --help)" >&2
    exit 2
    ;;
esac
