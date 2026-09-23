#!/bin/sh
# collect-hardware-fixture.sh: capture one machine into the Joulie hardware
# fixture corpus (testdata/hardware/<machine>/).
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

Captures the inputs the Joulie agent reads on one machine and writes them to
<dir>/<machine>/, then tars that directory. Everything identifying is redacted
before anything is written: GPU UUIDs and serial numbers, MAC addresses, the
hostname in every spelling, and the machine's IP addresses.

What it writes:
  powercap/          copy of /sys/class/powercap (or /host-sys/class/powercap),
                     one directory per zone, with name, constraint_0_* and
                     energy_uj as plain files. Absent when the node has no RAPL.
  cpuinfo            copy of /proc/cpuinfo
  nvidia-smi.txt     stdout of the exact query the agent runs, if nvidia-smi exists
  rocm-smi.txt       stdout of the exact query the agent runs, if rocm-smi exists
  node-labels.json   the node's labels as a JSON object, or {} when unknown
  machine.yaml       metadata skeleton for a human to complete

It deliberately does NOT write expected.json. That golden is produced by a human
who reviewed the capture, with:

  go test ./cmd/agent/ -run Corpus -update

Options:
  --name <machine>   corpus directory name, e.g. xeon-4socket-no-labels
  --out <dir>        parent output directory (default: ./joulie-fixtures)
  --node <nodeName>  Kubernetes node name, used to fetch labels with kubectl
                     (default: $NODE_NAME, which the agent DaemonSet sets)
  --no-tar           leave the directory, do not create the tar
  -h, --help         this text

Way 1: on the node itself

  sudo sh hack/collect-hardware-fixture.sh --name my-machine --out /tmp/fixtures
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
        < hack/collect-hardware-fixture.sh
  kubectl -n "$NS" cp "$NS/$POD:/tmp/fx/$NODE.tar" "./$NODE.tar"

The stock agent image is gcr.io/distroless/static, which has no shell, so that
exec only works against an image that carries one. On a stock install use a
debug container on the same node instead, which sees the same host files:

  kubectl debug node/$NODE -it --image=busybox -- sh
  # inside: /host is the node root, so run with --sysfs /host/sys --procfs /host/proc

Then, back in the repository:

  tar -xf <node>.tar -C testdata/hardware/
  # review every file, complete machine.yaml, then
  go test ./cmd/agent/ -run Corpus -update
  git add testdata/hardware/<machine>
EOF
}

SYSFS_CLASS=""
PROCFS=""

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

if [ -z "$MACHINE_NAME" ]; then
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

DEST="$OUT_DIR/$MACHINE_NAME"
rm -rf "$DEST"
mkdir -p "$DEST"

# ---------------------------------------------------------------------------
# Redaction
#
# Built once, applied to every text file the script writes. Anything that ties
# the dump to a machine, a person or a network must not reach the repository.
# ---------------------------------------------------------------------------

REDACT_SED="$DEST/.redact.sed"
: >"$REDACT_SED"

# GPU and MIG UUIDs, and bare UUIDs anywhere.
cat >>"$REDACT_SED" <<'EOF'
s/(GPU|MIG)-[0-9a-fA-F]{8}(-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}/\1-REDACTED/g
s/(GPU|MIG)-[0-9a-fA-F-]{16,}/\1-REDACTED/g
s/[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}/UUID-REDACTED/g
EOF

# Serial numbers, board serials and asset tags, whatever the key spelling.
cat >>"$REDACT_SED" <<'EOF'
s/([Ss]erial([ _-]?[Nn]umber)?[^A-Za-z0-9]{0,3})[A-Za-z0-9-]{4,}/\1SERIAL-REDACTED/g
s/([Aa]sset[ _-]?[Tt]ag[^A-Za-z0-9]{0,3})[A-Za-z0-9-]{4,}/\1ASSET-REDACTED/g
EOF

# MAC addresses, both separators.
cat >>"$REDACT_SED" <<'EOF'
s/\b[0-9a-fA-F]{2}([:-][0-9a-fA-F]{2}){5}\b/MAC-REDACTED/g
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

HOST_CANDIDATES="$DEST/.hosts"
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

CPUFREQ_DRIVER="$(dirname "$SYSFS_CLASS")/devices/system/cpu/cpufreq/policy0/scaling_driver"
if [ -f "$CPUFREQ_DRIVER" ]; then
	cat "$CPUFREQ_DRIVER" >"$DEST/cpufreq-driver"
	echo "$SCRIPT_NAME: copied $CPUFREQ_DRIVER"
else
	echo "$SCRIPT_NAME: no cpufreq scaling driver at $CPUFREQ_DRIVER"
fi

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

rm -f "$REDACT_SED" "$HOST_CANDIDATES" "$HOST_CANDIDATES.sorted"

echo "$SCRIPT_NAME: wrote $DEST"
echo "$SCRIPT_NAME: review every file, complete machine.yaml, then run the test with -update"

if [ "$MAKE_TAR" -eq 1 ]; then
	tar -cf "$OUT_DIR/$MACHINE_NAME.tar" -C "$OUT_DIR" "$MACHINE_NAME"
	echo "$SCRIPT_NAME: wrote $OUT_DIR/$MACHINE_NAME.tar"
fi
