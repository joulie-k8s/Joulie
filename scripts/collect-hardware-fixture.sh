#!/bin/sh
# collect-hardware-fixture.sh: capture one machine into the Joulie hardware
# fixture corpus (cmd/agent/testdata/hardware/<machine>/).
#
# The Joulie agent reads hardware only through files and command output, so a
# faithful copy of those inputs replays the machine in a unit test. This script
# copies exactly what cmd/agent reads, redacts everything that identifies the
# machine, and leaves the golden file to be generated later by the test itself.
#
# POSIX sh on purpose: it has to run in a busybox debug pod as well as on a node.

set -eu

SCRIPT_NAME="collect-hardware-fixture.sh"
OUT_DIR=""
MACHINE_NAME=""
NODE_NAME_ARG="${NODE_NAME:-}"
MAKE_TAR=1

usage() {
	cat <<'EOF'
Usage: collect-hardware-fixture.sh --name <machine> [--out <dir>] [--node <nodeName>] [--no-tar]
       collect-hardware-fixture.sh --check <dir> [--node <nodeName>]

Captures the inputs the Joulie agent reads on one machine and writes them to
<dir>/<machine>/, then tars that directory. Everything identifying is redacted
before anything is written: GPU UUIDs and serial numbers, MAC addresses, PCI
bus ids, the hostname in every spelling, and the machine's IP addresses.

Before the tar is written, an identifier check reads every file under
governor/ and stops the capture, with exit status 1 and no tar, when a PCI bus
id, a serial number or a host name is still in one of them, or when one of
them cannot be read.

What it writes:
  powercap/          copy of /sys/class/powercap (or /host-sys/class/powercap),
                     one directory per zone, with name, constraint_0_* and
                     energy_uj as plain files. Absent when the node has no RAPL.
  cpuinfo            copy of /proc/cpuinfo
  nvidia-smi.txt     stdout of the exact query the agent runs, if nvidia-smi exists
  rocm-smi.txt       stdout of the exact query the agent runs, if rocm-smi exists
  node-labels.json   the node's labels as a JSON object, or {} when unknown
  machine.yaml       metadata skeleton for a human to complete
  governor/          the frequency governor state, absent when the node has
                     none. Copied verbatim from sysfs:
                       cpufreq/policyN/   scaling_governor,
                                          scaling_available_governors,
                                          energy_performance_preference,
                                          energy_performance_available_preferences,
                                          boost, cpuinfo_min_freq, cpuinfo_max_freq
                       cpufreq/boost
                       intel_pstate/      status, no_turbo
                       amd_pstate/        status
                       cpuidle/stateN/    name, the idle states of CPU 0
                       amdgpu/gpuN/       device, power_dpm_force_performance_level
                                          and pp_dpm_{sclk,mclk,fclk,socclk,pcie}
                                          of the Nth amdgpu device in bus id
                                          order; the bus id itself is not kept
                     and, redacted:
                       nvidia-smi-clocks.txt  stdout of
                                          nvidia-smi -q -d CLOCK,SUPPORTED_CLOCKS,PERFORMANCE

It deliberately does NOT write expected.json. That golden is produced by a human
who reviewed the capture, with:

  go test ./cmd/agent/ -run Corpus -update

Options:
  --name <machine>   corpus directory name, e.g. xeon-4socket-no-labels
  --out <dir>        parent output directory (default: ./joulie-fixtures)
  --node <nodeName>  Kubernetes node name, used to fetch labels with kubectl
                     (default: $NODE_NAME, which the agent DaemonSet sets)
  --no-tar           leave the directory, do not create the tar
  --sysfs <dir>      the host /sys to read (default: /host-sys when it exists,
                     else /sys)
  --procfs <dir>     the host /proc to read (default: /proc)
  --check <dir>      capture nothing: run the identifier check over the
                     governor/ files of the capture in <dir>, and exit 1 when
                     it finds a PCI bus id, a serial number or a host name.
                     Host names are the --node value and this machine's names
  -h, --help         this text

Way 1: on the node itself

  sudo sh scripts/collect-hardware-fixture.sh --name my-machine --out /tmp/fixtures
  # then copy /tmp/fixtures/my-machine.tar off the node

Way 2: through the cluster, against the agent DaemonSet

The agent DaemonSet already mounts the host /sys at /host-sys, which is where
this script looks first. Pick the agent pod on the node you want, pipe the
script in, and pull the tar back out:

  NS=joulie-system
  NODE=<node>
  POD=$(kubectl -n "$NS" get pod -l app.kubernetes.io/name=joulie-agent \
        --field-selector "spec.nodeName=$NODE" -o jsonpath='{.items[0].metadata.name}')
  kubectl -n "$NS" exec -i "$POD" -- sh -s -- --name "$NODE" --out /tmp/fx \
        < scripts/collect-hardware-fixture.sh
  kubectl -n "$NS" cp "$NS/$POD:/tmp/fx/$NODE.tar" "./$NODE.tar"

The stock agent image is gcr.io/distroless/static, which has no shell, so that
exec only works against an image that carries one. On a stock install use a
debug container on the same node instead, which sees the same host files:

  kubectl debug node/$NODE -it --image=busybox -- sh
  # inside: /host is the node root, so run with --sysfs /host/sys --procfs /host/proc

Then, back in the repository:

  tar -xf <node>.tar -C cmd/agent/testdata/hardware/
  # review every file, complete machine.yaml, then
  go test ./cmd/agent/ -run Corpus -update
  git add cmd/agent/testdata/hardware/<machine>
EOF
}

SYSFS_CLASS=""
PROCFS=""
CHECK_DIR=""

while [ $# -gt 0 ]; do
	case "$1" in
	--name)
		MACHINE_NAME="${2:-}"
		shift 2
		;;
	--out)
		OUT_DIR="${2:-}"
		shift 2
		;;
	--node)
		NODE_NAME_ARG="${2:-}"
		shift 2
		;;
	--sysfs)
		SYSFS_CLASS="${2:-}/class"
		shift 2
		;;
	--procfs)
		PROCFS="${2:-}"
		shift 2
		;;
	--no-tar)
		MAKE_TAR=0
		shift
		;;
	--check)
		CHECK_DIR="${2:-}"
		shift 2
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		echo "$SCRIPT_NAME: unknown argument: $1" >&2
		usage >&2
		exit 2
		;;
	esac
done

if [ -z "$MACHINE_NAME" ] && [ -z "$CHECK_DIR" ]; then
	echo "$SCRIPT_NAME: --name is required" >&2
	exit 2
fi
[ -n "$OUT_DIR" ] || OUT_DIR="./joulie-fixtures"

# The agent sees the host sysfs at /host-sys; on a node it is /sys.
if [ -z "$SYSFS_CLASS" ]; then
	if [ -d /host-sys/class ]; then
		SYSFS_CLASS=/host-sys/class
	else
		SYSFS_CLASS=/sys/class
	fi
fi
[ -n "$PROCFS" ] || PROCFS=/proc
SYSFS_ROOT=$(dirname "$SYSFS_CLASS")

# The redaction program and the host name list live outside the capture, so
# nothing of them can reach the tar.
WORK=$(mktemp -d "${TMPDIR:-/tmp}/collect-hardware-fixture.XXXXXX")
trap 'rm -rf "$WORK"' EXIT

# ---------------------------------------------------------------------------
# Redaction
#
# Built once, applied to every text file the script writes. Anything that ties
# the dump to a machine, a person or a network must not reach the repository.
# ---------------------------------------------------------------------------

REDACT_SED="$WORK/redact.sed"
: >"$REDACT_SED"

# GPU and MIG UUIDs, and bare UUIDs anywhere.
cat >>"$REDACT_SED" <<'EOF'
s/(GPU|MIG)-[0-9a-fA-F]{8}(-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}/\1-REDACTED/g
s/(GPU|MIG)-[0-9a-fA-F-]{16,}/\1-REDACTED/g
s/[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}/UUID-REDACTED/g
EOF

# Serial numbers, board serials and asset tags, whatever the key spelling,
# quoting and padding before the separator: nvidia-smi -q pads the key to a
# column ("Serial Number                         : 1650..."), and JSON quotes
# it. The separator is required, as in the corpus validator's pattern.
cat >>"$REDACT_SED" <<'EOF'
s/([Ss]erial([ _-]?[Nn]umber)?["']?[[:space:]]*[:=][[:space:]]*["']?)[A-Za-z0-9][A-Za-z0-9-]{3,}/\1SERIAL-REDACTED/g
s/([Aa]sset[ _-]?[Tt]ag["']?[[:space:]]*[:=][[:space:]]*["']?)[A-Za-z0-9][A-Za-z0-9-]{3,}/\1ASSET-REDACTED/g
EOF

# MAC addresses, both separators.
cat >>"$REDACT_SED" <<'EOF'
s/\b[0-9a-fA-F]{2}([:-][0-9a-fA-F]{2}){5}\b/MAC-REDACTED/g
EOF

# PCI bus ids, with or without the domain, as nvidia-smi -q heads every GPU
# with one ("GPU 00000000:3B:00.0"). The slot of a device is where it sits in
# one chassis, which is not needed to describe hardware.
cat >>"$REDACT_SED" <<'EOF'
s/\b([0-9a-fA-F]{4,8}:)?[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-7]\b/BUSID-REDACTED/g
EOF

# IPv4 addresses, with or without a CIDR suffix. Four dotted octets only, so
# version strings and the floating point numbers in cpuinfo are left alone.
cat >>"$REDACT_SED" <<'EOF'
s/\b([0-9]{1,3}\.){3}[0-9]{1,3}(\/[0-9]{1,2})?\b/IPV4-REDACTED/g
EOF

# IPv6 addresses. At least five colon separated hex groups, so powercap zone
# names such as intel-rapl:0:0 are never touched.
cat >>"$REDACT_SED" <<'EOF'
s/\b([0-9a-fA-F]{1,4}:){5,7}[0-9a-fA-F]{1,4}\b/IPV6-REDACTED/g
s/\b[0-9a-fA-F]{1,4}(:[0-9a-fA-F]{1,4}){0,4}::([0-9a-fA-F]{1,4}(:[0-9a-fA-F]{1,4}){0,4})?\b/IPV6-REDACTED/g
EOF

# Every spelling of this machine's name, longest first so that a short name
# does not chop a fully qualified one in half.
escape_for_sed() {
	printf '%s' "$1" | sed -e 's/[.[\*^$()+?{|\\/]/\\&/g'
}

HOST_CANDIDATES="$WORK/hosts"
: >"$HOST_CANDIDATES"
for h in \
	"$NODE_NAME_ARG" \
	"$(hostname 2>/dev/null || true)" \
	"$(hostname -s 2>/dev/null || true)" \
	"$(hostname -f 2>/dev/null || true)" \
	"$(cat "$PROCFS/sys/kernel/hostname" 2>/dev/null || true)"; do
	[ -n "$h" ] || continue
	printf '%s\n' "$h" >>"$HOST_CANDIDATES"
	# The domain part on its own: a site name identifies the machine too.
	case "$h" in
	*.*) printf '%s\n' "${h#*.}" >>"$HOST_CANDIDATES" ;;
	esac
done
# Longest first.
awk '{ print length($0), $0 }' "$HOST_CANDIDATES" | sort -rn | cut -d' ' -f2- |
	awk '!seen[$0]++' >"$HOST_CANDIDATES.sorted"
while IFS= read -r h; do
	[ -n "$h" ] || continue
	esc=$(escape_for_sed "$h")
	printf 's/%s/REDACTED-HOST/g\n' "$esc" >>"$REDACT_SED"
done <"$HOST_CANDIDATES.sorted"

redact() {
	# redact <file>: rewrite the file in place through the sed program.
	[ -f "$1" ] || return 0
	sed -E -f "$REDACT_SED" "$1" >"$1.redacted" 2>/dev/null || cp "$1" "$1.redacted"
	mv "$1.redacted" "$1"
}

# ---------------------------------------------------------------------------
# Identifier check
#
# The governor files are copied verbatim, because sysfs prints kernel words and
# numbers there and a substring redaction could rewrite a governor word. This
# check is what stands between them and the repository instead: it rejects a
# PCI bus id, a serial number or one of the host names above in any file under
# governor/, nvidia-smi-clocks.txt included after its redaction. Grep patterns
# without \b, so a busybox grep reads them the same way.
# ---------------------------------------------------------------------------

IDENT_BUSID='([0-9a-fA-F]{4,8}:)?[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-7]([^0-9]|$)'
IDENT_SERIAL="([Ss]erial([ _-]?[Nn]umber)?|[Aa]sset[ _-]?[Tt]ag)[\"']?[[:space:]]*[:=][[:space:]]*[\"']?[A-Za-z0-9][A-Za-z0-9-]{3,}"

ident_report() {
	# ident_report <file> <what>: one line per grep -n hit read from stdin.
	while IFS=: read -r line _; do
		echo "$SCRIPT_NAME: $1:$line: contains $2" >&2
	done
}

check_identifiers() {
	# check_identifiers <capture dir>: return 1 when a governor file still
	# carries an identifier. The placeholders the redaction writes, such as
	# BUSID-REDACTED and REDACTED-HOST, are not identifiers. The file list is
	# read line by line, not split on white space, so a capture under a path
	# with a space is checked too, and a file that cannot be read fails the
	# check rather than passing it unread.
	ident_found=0
	[ -d "$1/governor" ] || return 0
	find "$1/governor" -type f | sort >"$WORK/files"
	while IFS= read -r f; do
		rel=${f#"$1"/}
		if ! sed -E 's/[A-Za-z0-9]*-?REDACTED(-[A-Za-z]+)?//g' "$f" >"$WORK/clean"; then
			echo "$SCRIPT_NAME: $rel: cannot be read, so it cannot be checked" >&2
			ident_found=1
			continue
		fi
		if grep -qE "$IDENT_BUSID" "$WORK/clean"; then
			grep -nE "$IDENT_BUSID" "$WORK/clean" | ident_report "$rel" "a PCI bus id"
			ident_found=1
		fi
		if grep -qE "$IDENT_SERIAL" "$WORK/clean"; then
			grep -nE "$IDENT_SERIAL" "$WORK/clean" | ident_report "$rel" "a serial number or an asset tag"
			ident_found=1
		fi
		while IFS= read -r h; do
			[ -n "$h" ] || continue
			if grep -qiwF -- "$h" "$WORK/clean"; then
				grep -niwF -- "$h" "$WORK/clean" | ident_report "$rel" "the host name $h"
				ident_found=1
			fi
		done <"$HOST_CANDIDATES.sorted"
	done <"$WORK/files"
	return "$ident_found"
}

if [ -n "$CHECK_DIR" ]; then
	if [ ! -d "$CHECK_DIR" ]; then
		echo "$SCRIPT_NAME: --check $CHECK_DIR is not a directory" >&2
		exit 2
	fi
	if check_identifiers "$CHECK_DIR"; then
		echo "$SCRIPT_NAME: no identifier in $CHECK_DIR/governor"
		exit 0
	fi
	echo "$SCRIPT_NAME: $CHECK_DIR/governor still carries an identifier; remove it before the capture is published" >&2
	exit 1
fi

DEST="$OUT_DIR/$MACHINE_NAME"
rm -rf "$DEST"
mkdir -p "$DEST"

# ---------------------------------------------------------------------------
# powercap
# ---------------------------------------------------------------------------

POWERCAP_SRC="$SYSFS_CLASS/powercap"
zone_count=0
if [ -d "$POWERCAP_SRC" ]; then
	for zone in "$POWERCAP_SRC"/*; do
		[ -d "$zone" ] || continue
		base=$(basename "$zone")
		mkdir -p "$DEST/powercap/$base"
		for f in name enabled energy_uj max_energy_range_uj \
			constraint_0_name constraint_0_power_limit_uw \
			constraint_0_max_power_uw constraint_0_min_power_uw \
			constraint_0_time_window_us \
			constraint_1_name constraint_1_power_limit_uw \
			constraint_1_max_power_uw constraint_1_min_power_uw \
			constraint_1_time_window_us; do
			[ -f "$zone/$f" ] || continue
			# sysfs attributes report size 4096 and can fail to read on a
			# disabled zone; skip whatever does not read.
			cat "$zone/$f" >"$DEST/powercap/$base/$f" 2>/dev/null ||
				rm -f "$DEST/powercap/$base/$f"
		done
		zone_count=$((zone_count + 1))
	done
	if [ "$zone_count" -eq 0 ]; then
		echo "$SCRIPT_NAME: $POWERCAP_SRC is empty, this node has no RAPL"
	else
		echo "$SCRIPT_NAME: copied $zone_count powercap zone(s) from $POWERCAP_SRC"
	fi
else
	echo "$SCRIPT_NAME: no powercap tree at $POWERCAP_SRC, this node has no RAPL"
fi

# ---------------------------------------------------------------------------
# /proc/cpuinfo
# ---------------------------------------------------------------------------

if [ -f "$PROCFS/cpuinfo" ]; then
	cat "$PROCFS/cpuinfo" >"$DEST/cpuinfo"
	redact "$DEST/cpuinfo"
	echo "$SCRIPT_NAME: copied $PROCFS/cpuinfo"
else
	echo "$SCRIPT_NAME: warning: no $PROCFS/cpuinfo" >&2
fi

# ---------------------------------------------------------------------------
# cpufreq scaling driver, which the agent reports as cpu.driverFamily
# ---------------------------------------------------------------------------

CPUFREQ_DRIVER="$SYSFS_ROOT/devices/system/cpu/cpufreq/policy0/scaling_driver"
if [ -f "$CPUFREQ_DRIVER" ]; then
	cat "$CPUFREQ_DRIVER" >"$DEST/cpufreq-driver"
	echo "$SCRIPT_NAME: copied $CPUFREQ_DRIVER"
else
	echo "$SCRIPT_NAME: no cpufreq scaling driver at $CPUFREQ_DRIVER"
fi

# ---------------------------------------------------------------------------
# Governor state, which the agent reports as cpu.governor and gpu.governor:
# the cpufreq governor, preference and boost of every policy, the driver mode,
# the idle state names, and the performance level and clock tables of every
# GPU. Sysfs files are copied verbatim; check_identifiers reads them all.
# ---------------------------------------------------------------------------

GOV_DEST="$DEST/governor"
CPU_SYS="$SYSFS_ROOT/devices/system/cpu"

copy_attr() {
	# copy_attr <src> <dst>: copy one sysfs attribute when it reads.
	[ -f "$1" ] || return 0
	mkdir -p "$(dirname "$2")"
	cat "$1" >"$2" 2>/dev/null || rm -f "$2"
}

policy_count=0
for policy in "$CPU_SYS"/cpufreq/policy*; do
	[ -d "$policy" ] || continue
	base=$(basename "$policy")
	for f in scaling_governor scaling_available_governors \
		energy_performance_preference energy_performance_available_preferences \
		boost cpuinfo_min_freq cpuinfo_max_freq; do
		copy_attr "$policy/$f" "$GOV_DEST/cpufreq/$base/$f"
	done
	policy_count=$((policy_count + 1))
done
copy_attr "$CPU_SYS/cpufreq/boost" "$GOV_DEST/cpufreq/boost"
copy_attr "$CPU_SYS/intel_pstate/status" "$GOV_DEST/intel_pstate/status"
copy_attr "$CPU_SYS/intel_pstate/no_turbo" "$GOV_DEST/intel_pstate/no_turbo"
copy_attr "$CPU_SYS/amd_pstate/status" "$GOV_DEST/amd_pstate/status"
for state in "$CPU_SYS"/cpu0/cpuidle/state*; do
	[ -d "$state" ] || continue
	copy_attr "$state/name" "$GOV_DEST/cpuidle/$(basename "$state")/name"
done
echo "$SCRIPT_NAME: copied the governor state of $policy_count cpufreq policies"

# amdgpu devices in bus id order, as the agent lists them. The directory is
# named after the position, so the bus id stays on the machine.
amdgpu_count=0
for dev in "$SYSFS_ROOT"/bus/pci/devices/*; do
	[ -f "$dev/power_dpm_force_performance_level" ] || continue
	[ "$(cat "$dev/vendor" 2>/dev/null || true)" = "0x1002" ] || continue
	for f in device power_dpm_force_performance_level \
		pp_dpm_sclk pp_dpm_mclk pp_dpm_fclk pp_dpm_socclk pp_dpm_pcie; do
		copy_attr "$dev/$f" "$GOV_DEST/amdgpu/gpu$amdgpu_count/$f"
	done
	amdgpu_count=$((amdgpu_count + 1))
done
if [ "$amdgpu_count" -gt 0 ]; then
	echo "$SCRIPT_NAME: copied the performance level of $amdgpu_count amdgpu device(s)"
fi

if command -v nvidia-smi >/dev/null 2>&1; then
	mkdir -p "$GOV_DEST"
	if nvidia-smi -q -d CLOCK,SUPPORTED_CLOCKS,PERFORMANCE >"$GOV_DEST/nvidia-smi-clocks.txt" 2>/dev/null; then
		redact "$GOV_DEST/nvidia-smi-clocks.txt"
		echo "$SCRIPT_NAME: captured nvidia-smi clock state"
	else
		rm -f "$GOV_DEST/nvidia-smi-clocks.txt"
		echo "$SCRIPT_NAME: warning: nvidia-smi present but the clock query failed" >&2
	fi
fi
rmdir "$GOV_DEST" 2>/dev/null || true

# ---------------------------------------------------------------------------
# GPU query commands, exactly as cmd/agent/main.go runs them
# ---------------------------------------------------------------------------

if command -v nvidia-smi >/dev/null 2>&1; then
	if nvidia-smi \
		--query-gpu=index,power.min_limit,power.max_limit,power.limit,power.draw,name \
		--format=csv,noheader,nounits >"$DEST/nvidia-smi.txt" 2>/dev/null; then
		redact "$DEST/nvidia-smi.txt"
		echo "$SCRIPT_NAME: captured nvidia-smi query output"
	else
		rm -f "$DEST/nvidia-smi.txt"
		echo "$SCRIPT_NAME: warning: nvidia-smi present but the query failed" >&2
	fi
fi

if command -v rocm-smi >/dev/null 2>&1; then
	if rocm-smi --showpowercap --showproductname --json >"$DEST/rocm-smi.txt" 2>/dev/null; then
		redact "$DEST/rocm-smi.txt"
		echo "$SCRIPT_NAME: captured rocm-smi query output"
	else
		rm -f "$DEST/rocm-smi.txt"
		echo "$SCRIPT_NAME: warning: rocm-smi present but the query failed" >&2
	fi
fi

# ---------------------------------------------------------------------------
# node labels
# ---------------------------------------------------------------------------

printf '{}\n' >"$DEST/node-labels.json"
if [ -n "$NODE_NAME_ARG" ] && command -v kubectl >/dev/null 2>&1; then
	if kubectl get node "$NODE_NAME_ARG" -o jsonpath='{.metadata.labels}' \
		>"$DEST/node-labels.json.raw" 2>/dev/null &&
		[ -s "$DEST/node-labels.json.raw" ]; then
		mv "$DEST/node-labels.json.raw" "$DEST/node-labels.json"
		printf '\n' >>"$DEST/node-labels.json"
		echo "$SCRIPT_NAME: fetched labels for node $NODE_NAME_ARG"
	else
		rm -f "$DEST/node-labels.json.raw"
		echo "$SCRIPT_NAME: warning: could not read node labels, wrote {}" >&2
	fi
fi
redact "$DEST/node-labels.json"

# ---------------------------------------------------------------------------
# machine.yaml skeleton
#
# Everything here needs a human: the counts below come from the capture host,
# which is not always the same thing as what kubelet reports.
# ---------------------------------------------------------------------------

cpu_count=$(grep -c '^processor' "$DEST/cpuinfo" 2>/dev/null || echo 0)
mem_kb=$(awk '/^MemTotal:/ { print $2 }' "$PROCFS/meminfo" 2>/dev/null || true)
[ -n "$mem_kb" ] || mem_kb=0
gpu_count=0
if [ -f "$DEST/nvidia-smi.txt" ]; then
	gpu_count=$(grep -c . "$DEST/nvidia-smi.txt" || echo 0)
fi
captured_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)

{
	printf 'description: "TODO: one line, what kind of machine this is"\n'
	printf 'source: "TODO: where this dump came from, and who reviewed it"\n'
	printf 'capturedAt: "%s"\n' "$captured_at"
	printf 'allocatable:\n'
	printf '  cpu: "%s"\n' "$cpu_count"
	printf '  memory: "%sKi"\n' "$mem_kb"
	if [ "$gpu_count" -gt 0 ]; then
		printf '  gpu:\n'
		printf '    nvidia.com/gpu: "%s"\n' "$gpu_count"
	else
		printf '  gpu: {}\n'
	fi
	printf '# Zone directories whose constraint_0_power_limit_uw rejects writes on the\n'
	printf '# real machine, for example a disabled DRAM zone. The test replays them by\n'
	printf '# turning the limit file into a directory.\n'
	printf 'writeRejectingZones: []\n'
} >"$DEST/machine.yaml"

if ! check_identifiers "$DEST"; then
	echo "$SCRIPT_NAME: $DEST/governor still carries an identifier, so no tar was written." >&2
	echo "$SCRIPT_NAME: remove it from the files named above, then rerun with --check $DEST" >&2
	exit 1
fi

echo "$SCRIPT_NAME: wrote $DEST"
echo "$SCRIPT_NAME: review every file, complete machine.yaml, then run the test with -update"

if [ "$MAKE_TAR" -eq 1 ]; then
	tar -cf "$OUT_DIR/$MACHINE_NAME.tar" -C "$OUT_DIR" "$MACHINE_NAME"
	echo "$SCRIPT_NAME: wrote $OUT_DIR/$MACHINE_NAME.tar"
fi
