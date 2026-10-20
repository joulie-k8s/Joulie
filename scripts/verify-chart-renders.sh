#!/usr/bin/env bash
#
# verify-chart-renders.sh renders the Joulie Helm charts under every value
# combination CI cares about and asserts the objects that come out.
#
# Why it exists: "helm lint" accepts a chart whose templates render the same
# Deployment twice, and a chart broken that way once reached main. Every case
# below compares the full list of Deployments, DaemonSets and StatefulSets with
# the exact list expected for that case, so a duplicated template (one object
# too many) and an orphaned one (one missing) both fail.
#
# The agent's workloads are also compared byte for byte with goldens under
# scripts/testdata/chart-renders/. Goldens are written only by
# --update-goldens, never by hand, so a change to the agent pod spec shows up
# as a golden diff in review.
#
# Requires helm and awk. Exit status 0 when every case passes, 1 otherwise.

set -u -o pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHART="${REPO_ROOT}/charts/joulie"
SIM_CHART="${REPO_ROOT}/charts/joulie-simulator"
GOLDEN_DIR="${REPO_ROOT}/scripts/testdata/chart-renders"
RELEASE="joulie"
VERBOSE=0
UPDATE_GOLDENS=0

usage() {
	cat <<'EOF'
verify-chart-renders.sh - render the Joulie charts under the value combinations
CI covers and assert the workload objects they produce.

Usage:
  scripts/verify-chart-renders.sh [options]

Options:
  -h, --help        Show this help and exit.
  -v, --verbose     Print the rendered Kind/name list for every case.
  --update-goldens  Write the renders the golden cases compare with to
                    scripts/testdata/chart-renders/ instead of comparing.
                    Review the diff: an intended change only.

Cases:
  defaults                 chart defaults
  values/joulie.yaml       the values file shipped for real installs
  legacy operator: key     asserts it renders byte for byte what
                           controllerManager: renders
  leaderElection.enabled   asserts the Lease Role and RoleBinding appear
  agent.mode=pool          asserts the StatefulSet replaces the DaemonSet
  schedulerExtender        asserts a second Deployment, one per component
  C01                      the agent DaemonSet equals its goldens: defaults,
                           values/joulie.yaml, hardwareCatalog.enabled, and
                           agent.hostNetwork=false,agent.privileged=false
  C02                      the agent pool StatefulSet equals its golden
  C03                      the agent tool hooks render and only add lines
  C04                      agent.mode=emulated renders one Deployment per
                           node and no other agent; equals its golden
  C05                      each emulated agent: no host network or
                           privileges, no CAP_DAC_OVERRIDE, literal
                           NODE_NAME, PATH, its hwemu
                           tree mounts, /dev/nvidiactl on NVIDIA only,
                           Recreate, labels and annotations
  C06                      agent.mode=none renders no agent
  C07                      a host network scheduler extender uses Recreate
  C09                      values/simcluster.yaml pulls only joulie.local
                           images, IfNotPresent
  C10                      the agent tolerates the GPU vendor taints by
                           default
  C11                      agent.emulated.fuse=true: sys and state from the
                           simulator's views under root/fuse, mounted with
                           HostToContainer, and nothing else changes; the
                           DaemonSet ignores it
  joulie-simulator         the simulator chart; asserts its ConfigMap volumes
                           are optional, so a bare install starts
  joulie-simulator emulate emulate.enabled with the smoke cluster spec; asserts
                           Recreate, root, the hostPath tree root, the emulate
                           and OTEL_* env, the spec ConfigMap, the exporter
                           endpoints and the workload run rules, and that
                           replicas=2 and a missing spec fail
  joulie-simulator fuse    emulate.fuse: SIM_HWEMU_FUSE, privileged with
                           SYS_ADMIN, /dev/fuse and Bidirectional on the root,
                           none of which emulate mode renders without it
  rbac.workloadRuns        the workload run rules follow emulate.enabled
                           unless forced; extraEnv sets a trace path in
                           emulate mode
  C08 packaged appVersion  packages both charts the way the release does and
                           asserts every image is tagged with the appVersion,
                           also under values/joulie.yaml and for emulated
                           agents, unless a tag is set
  GC12 governor control    the controller manager writes no governor by
                           default: GOVERNOR_CONTROL=off, with the documented
                           preset defaults, also under values/joulie.yaml
  GA27 agent governor      the agent writes no governor by default:
                           GOVERNOR_WRITES=false, GOVERNOR_RESTORE_ON_EXIT=true,
                           also under values/joulie.yaml; the two variables
                           are the only lines they add to the DaemonSet
  OTel inputs              the controller manager reads OTel inputs by
                           default: NODE_POWER_SOURCE=prometheus, scope
                           components, a 5m window and FACILITY_PRESET=otel;
                           no NODE_POWER_PRESET, so a custom query stays
                           custom; KEPLER_AVAILABLE and
                           FACILITY_PROMETHEUS_ADDRESS are gone

Every case also asserts that no two documents share a Kind and name.

Run it from anywhere; it resolves the repository root from its own location:

  scripts/verify-chart-renders.sh
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
	-h | --help)
		usage
		exit 0
		;;
	-v | --verbose)
		VERBOSE=1
		shift
		;;
	--update-goldens)
		UPDATE_GOLDENS=1
		shift
		;;
	*)
		echo "verify-chart-renders.sh: unknown argument '$1'" >&2
		echo >&2
		usage >&2
		exit 2
		;;
	esac
done

if ! command -v helm >/dev/null 2>&1; then
	echo "verify-chart-renders.sh: helm is required but is not on PATH" >&2
	exit 1
fi

WORKDIR="$(mktemp -d)"
trap 'rm -rf "${WORKDIR}"' EXIT

FAILURES=0
CASES=0

fail() {
	echo "  FAIL: $*" >&2
	FAILURES=$((FAILURES + 1))
}

# objects prints one "Kind/name" line per rendered document, in render order.
# Helm puts every document's top level keys at column 0, so a small awk state
# machine is enough and the script needs no yq or python.
objects() {
	awk '
		function flush() { if (kind != "") print kind "/" name; kind=""; name=""; inmeta=0 }
		/^---[[:space:]]*$/ { flush(); next }
		/^kind:[[:space:]]/ { kind=$2; inmeta=0; next }
		/^metadata:[[:space:]]*$/ { inmeta=1; next }
		/^[^[:space:]#]/ { inmeta=0 }
		inmeta == 1 && /^  name:[[:space:]]/ { if (name == "") name=$2; next }
		END { flush() }
	' "$1"
}

# render writes one case's manifest to ${WORKDIR}/<slug>.yaml.
render() {
	local slug="$1" chart="$2"
	shift 2
	if ! helm template "${RELEASE}" "${chart}" "$@" \
		>"${WORKDIR}/${slug}.yaml" 2>"${WORKDIR}/${slug}.err"; then
		fail "helm template failed"
		sed 's/^/    /' "${WORKDIR}/${slug}.err" >&2
		return 1
	fi
	if [ "${VERBOSE}" -eq 1 ]; then
		objects "${WORKDIR}/${slug}.yaml" | sed 's/^/    /'
	fi
	return 0
}

# assert_workloads compares the rendered Deployments, DaemonSets and
# StatefulSets with the expected set, order independent.
assert_workloads() {
	local slug="$1"
	shift
	local got want
	got="$(objects "${WORKDIR}/${slug}.yaml" | grep -E '^(Deployment|DaemonSet|StatefulSet)/' | sort)"
	want="$(printf '%s\n' "$@" | sort)"
	if [ "${got}" != "${want}" ]; then
		fail "workload objects differ from the expected set (- want, + got)"
		diff -u <(printf '%s\n' "${want}") <(printf '%s\n' "${got}") | sed 's/^/    /' >&2
		return 1
	fi
	return 0
}

# assert_no_duplicates catches a template rendered twice for any kind, not only
# workloads: two documents with the same Kind and name never survive an apply.
assert_no_duplicates() {
	local slug="$1" dupes
	dupes="$(objects "${WORKDIR}/${slug}.yaml" | sort | uniq -d)"
	if [ -n "${dupes}" ]; then
		fail "duplicate objects in the render:"
		printf '%s\n' "${dupes}" | sed 's/^/    /' >&2
		return 1
	fi
	return 0
}

# assert_count checks how many "Kind/name" lines match an extended regex.
assert_count() {
	local slug="$1" pattern="$2" want="$3" got
	got="$(objects "${WORKDIR}/${slug}.yaml" | grep -cE "${pattern}")" || true
	if [ "${got}" != "${want}" ]; then
		fail "expected ${want} object(s) matching /${pattern}/, got ${got}"
		return 1
	fi
	return 0
}

# doc prints the one rendered document of a Kind and name, empty when there is
# none, so an assertion about a Deployment cannot match a ConfigMap's text.
# Blank lines at the end of a document are dropped: Helm 4.3 separates
# documents with them and 4.1 does not, and they are not part of the spec.
doc() {
	awk -v want="$2/$3" '
		function flush() { sub(/\n+$/, "\n", buf); if (kind "/" name == want) printf "%s", buf; buf=""; kind=""; name=""; inmeta=0 }
		/^---[[:space:]]*$/ { flush(); next }
		{ buf = buf $0 "\n" }
		/^kind:[[:space:]]/ { kind=$2; inmeta=0; next }
		/^metadata:[[:space:]]*$/ { inmeta=1; next }
		/^[^[:space:]#]/ { inmeta=0 }
		inmeta == 1 && /^  name:[[:space:]]/ { if (name == "") name=$2; next }
		END { flush() }
	' "${WORKDIR}/$1.yaml"
}

# assert_golden compares a file of rendered documents with
# scripts/testdata/chart-renders/<golden>.golden.yaml, or writes the golden
# under --update-goldens.
assert_golden() {
	local got="$1" golden="${GOLDEN_DIR}/$2.golden.yaml"
	if [ ! -s "${got}" ]; then
		fail "golden $2: nothing rendered to compare"
		return 1
	fi
	if [ "${UPDATE_GOLDENS}" -eq 1 ]; then
		mkdir -p "${GOLDEN_DIR}"
		cp "${got}" "${golden}"
		echo "    wrote ${golden#"${REPO_ROOT}/"}"
		return 0
	fi
	if [ ! -f "${golden}" ]; then
		fail "golden ${golden#"${REPO_ROOT}/"} does not exist; write it with --update-goldens"
		return 1
	fi
	if ! diff -u "${golden}" "${got}" >"${got}.diff"; then
		fail "render differs from ${golden#"${REPO_ROOT}/"} (- golden, + got)"
		sed 's/^/    /' "${got}.diff" >&2
		return 1
	fi
	return 0
}

# assert_golden_doc compares one rendered document of a Kind and name with
# its golden.
assert_golden_doc() {
	local slug="$1" kind="$2" name="$3" golden="$4"
	doc "${slug}" "${kind}" "${name}" >"${WORKDIR}/${slug}.${kind}.${name}.yaml"
	assert_golden "${WORKDIR}/${slug}.${kind}.${name}.yaml" "${golden}"
}

# assert_doc checks that one document holds a line matching an extended regex
# ("has") or none ("lacks").
assert_doc() {
	local slug="$1" want="$2" kind="$3" name="$4" pattern="$5" what="$6"
	if doc "${slug}" "${kind}" "${name}" | grep -qE -- "${pattern}"; then
		[ "${want}" = has ] && return 0
		fail "${kind}/${name}: ${what}: found /${pattern}/"
	else
		[ "${want}" = lacks ] && return 0
		fail "${kind}/${name}: ${what}: no line matches /${pattern}/"
	fi
	return 1
}

# env_of prints the value of one env var of a Deployment's containers, the
# line after its name with "value:" and quotes stripped.
env_of() {
	doc "$1" Deployment "$2" |
		awk -v var="$3" '
			$0 ~ "^[[:space:]]+- name: " var "$" { found=1; next }
			found == 1 { sub(/^[[:space:]]+value:[[:space:]]*/, ""); gsub(/"/, ""); print; exit }
		'
}

# assert_env checks one env var of a Deployment's containers: its value, or
# "-" for an env var that must be absent.
assert_env() {
	local slug="$1" deploy="$2" var="$3" want="$4" got
	got="$(env_of "${slug}" "${deploy}" "${var}")"
	if [ "${want}" = "-" ]; then
		if doc "${slug}" Deployment "${deploy}" | grep -qE "^[[:space:]]+- name: ${var}\$"; then
			fail "Deployment/${deploy}: env ${var} must not be rendered"
			return 1
		fi
		return 0
	fi
	if [ "${got}" != "${want}" ]; then
		fail "Deployment/${deploy}: env ${var}=\"${got}\", want \"${want}\""
		return 1
	fi
	return 0
}

# assert_optional_volumes checks that each named configMap volume of a
# Deployment is optional. A pod whose configMap volume is missing and not
# optional waits in ContainerCreating forever, which is what a bare install of
# the simulator chart did, since nothing creates those ConfigMaps by default.
assert_optional_volumes() {
	local slug="$1" deploy="$2" vol block
	shift 2
	for vol in "$@"; do
		block="$(doc "${slug}" Deployment "${deploy}" |
			awk -v vol="${vol}" '
				/^      volumes:/ { invol=1; next }
				invol == 1 && $0 ~ "^        - name: " vol "$" { pick=1; next }
				pick == 1 && /^        - / { exit }
				pick == 1 { print }
			')"
		if [ -z "${block}" ]; then
			fail "Deployment/${deploy}: no volume ${vol}"
		elif ! printf '%s\n' "${block}" | grep -qE '^[[:space:]]+optional: true$'; then
			fail "Deployment/${deploy}: volume ${vol} is not optional"
		fi
	done
}

# entry prints one list entry of a Deployment's pod spec: the volume
# ("volumes") or volume mount ("volumeMounts") of a name, from its "- name:"
# line to the next entry.
entry() {
	local slug="$1" deploy="$2" list="$3" name="$4"
	doc "${slug}" Deployment "${deploy}" |
		awk -v list="${list}" -v name="${name}" '
			$0 ~ "^ +" list ":$" { inlist=1; match($0, /^ +/); depth=RLENGTH; next }
			inlist == 1 && match($0, /^ +/) && RLENGTH <= depth && $0 !~ /^ +- / { inlist=0 }
			inlist == 1 && $0 ~ "^ +- name: " name "$" { pick=1; print; next }
			pick == 1 && /^ +- / { exit }
			pick == 1 && inlist == 0 { exit }
			pick == 1 { print }
		'
}

# assert_entry checks that the volume or volume mount of a name holds a line
# matching an extended regex ("has") or none ("lacks").
assert_entry() {
	local slug="$1" want="$2" deploy="$3" list="$4" name="$5" pattern="$6" block
	block="$(entry "${slug}" "${deploy}" "${list}" "${name}")"
	if [ -z "${block}" ]; then
		fail "Deployment/${deploy}: no ${list} entry ${name}"
		return 1
	fi
	if printf '%s\n' "${block}" | grep -qE -- "${pattern}"; then
		[ "${want}" = has ] && return 0
		fail "Deployment/${deploy}: ${list} ${name}: found /${pattern}/"
	else
		[ "${want}" = lacks ] && return 0
		fail "Deployment/${deploy}: ${list} ${name}: no line matches /${pattern}/"
	fi
	return 1
}

# render_fails asserts that helm template exits non-zero with a message
# matching an extended regex: a values combination the chart must refuse.
render_fails() {
	local slug="$1" chart="$2" message="$3"
	shift 3
	if helm template "${RELEASE}" "${chart}" "$@" >"${WORKDIR}/${slug}.yaml" 2>"${WORKDIR}/${slug}.err"; then
		fail "${slug}: helm template succeeded, want a failure matching /${message}/"
		return 1
	fi
	if ! grep -qE -- "${message}" "${WORKDIR}/${slug}.err"; then
		fail "${slug}: helm template failed without /${message}/:"
		sed 's/^/    /' "${WORKDIR}/${slug}.err" >&2
		return 1
	fi
	return 0
}

# assert_image_tags checks that every container image in the render carries
# the given tag. A chart that pulls ":latest" runs whatever was published last,
# a release candidate included, instead of the version it was packaged with.
assert_image_tags() {
	local slug="$1" want="$2" images bad
	images="$(grep -E '^[[:space:]]+image:' "${WORKDIR}/${slug}.yaml" | sed -E 's/^[[:space:]]+image:[[:space:]]*//; s/"//g')"
	if [ -z "${images}" ]; then
		fail "no container images in the render"
		return 1
	fi
	bad="$(printf '%s\n' "${images}" | grep -vE ":${want}\$")" || true
	if [ -n "${bad}" ]; then
		fail "images not tagged ${want}:"
		printf '%s\n' "${bad}" | sed 's/^/    /' >&2
		return 1
	fi
	return 0
}

case_start() {
	CASES=$((CASES + 1))
	echo "==> $1"
}

echo "Verifying chart renders from ${REPO_ROOT}"

# --------------------------------------------------------------------------
# Chart defaults: agent DaemonSet and controller manager, scheduler extender
# off, leader election off.
# --------------------------------------------------------------------------
case_start "defaults"
if render defaults "${CHART}"; then
	assert_workloads defaults \
		"DaemonSet/joulie-agent" \
		"Deployment/joulie-controller-manager"
	assert_no_duplicates defaults
	assert_count defaults '^Role/.*-controller-manager-leader-election$' 0
	assert_count defaults '^RoleBinding/.*-controller-manager-leader-election$' 0
	# From a checkout the chart follows the newest release; only a packaged
	# release pins its own version (the "packaged" case below).
	assert_image_tags defaults latest
fi

# --------------------------------------------------------------------------
# The values file shipped for real installs.
# --------------------------------------------------------------------------
case_start "values/joulie.yaml"
if render release-values "${CHART}" -f "${REPO_ROOT}/values/joulie.yaml"; then
	assert_workloads release-values \
		"DaemonSet/joulie-agent" \
		"Deployment/joulie-controller-manager"
	assert_no_duplicates release-values
fi

# --------------------------------------------------------------------------
# The deprecated "operator:" values key must still render byte for byte what
# "controllerManager:" renders. The alias lives in templates/_helpers.tpl and
# goes away a release from now; a key that stops being merged has to fail here
# and not on an adopter's cluster.
# --------------------------------------------------------------------------
case_start "legacy operator: key renders identically to controllerManager:"
NEW_KEY_ARGS=(
	--set 'controllerManager.leaderElection.enabled=true'
	--set 'controllerManager.image.tag=v0.0.0-test'
	--set 'controllerManager.image.pullPolicy=Always'
	--set 'controllerManager.env.RECONCILE_INTERVAL=2m'
	--set 'controllerManager.env.ECO_CAP_WATTS=99'
	--set 'controllerManager.nodeSelector.disktype=ssd'
	--set 'controllerManager.service.port=9191'
)
LEGACY_KEY_ARGS=("${NEW_KEY_ARGS[@]//controllerManager./operator.}")
if render controller-manager-key "${CHART}" "${NEW_KEY_ARGS[@]}" &&
	render operator-key "${CHART}" "${LEGACY_KEY_ARGS[@]}"; then
	if ! diff -u "${WORKDIR}/controller-manager-key.yaml" "${WORKDIR}/operator-key.yaml" \
		>"${WORKDIR}/alias.diff"; then
		fail "the deprecated operator: key no longer renders what controllerManager: renders"
		sed 's/^/    /' "${WORKDIR}/alias.diff" >&2
	fi
	assert_workloads operator-key \
		"DaemonSet/joulie-agent" \
		"Deployment/joulie-controller-manager"
	assert_no_duplicates operator-key
fi

# --------------------------------------------------------------------------
# Leader election on: the namespaced Lease Role and RoleBinding appear and the
# workload set is unchanged.
# --------------------------------------------------------------------------
case_start "controllerManager.leaderElection.enabled=true"
if render leader-election "${CHART}" --set 'controllerManager.leaderElection.enabled=true'; then
	assert_workloads leader-election \
		"DaemonSet/joulie-agent" \
		"Deployment/joulie-controller-manager"
	assert_no_duplicates leader-election
	assert_count leader-election '^Role/.*-controller-manager-leader-election$' 1
	assert_count leader-election '^RoleBinding/.*-controller-manager-leader-election$' 1
fi

# --------------------------------------------------------------------------
# Pool mode: the agent becomes a StatefulSet and the DaemonSet must be gone,
# otherwise two agents would reconcile the same nodes.
# --------------------------------------------------------------------------
case_start "agent.mode=pool"
if render agent-pool "${CHART}" --set 'agent.mode=pool'; then
	assert_workloads agent-pool \
		"StatefulSet/joulie-agent-pool" \
		"Deployment/joulie-controller-manager"
	assert_no_duplicates agent-pool
fi

# --------------------------------------------------------------------------
# Scheduler extender on: a second Deployment, exactly one per component.
# --------------------------------------------------------------------------
case_start "schedulerExtender.enabled=true"
if render scheduler-extender "${CHART}" --set 'schedulerExtender.enabled=true'; then
	assert_workloads scheduler-extender \
		"DaemonSet/joulie-agent" \
		"Deployment/joulie-controller-manager" \
		"Deployment/joulie-scheduler-extender"
	assert_no_duplicates scheduler-extender
fi

# --------------------------------------------------------------------------
# C01: the agent DaemonSet's pod spec comes from the joulie.agentPodSpec
# helper, which the emulated agents share. The DaemonSet stays byte for byte
# its golden under the value combinations that change it.
# --------------------------------------------------------------------------
case_start "C01 agent DaemonSet matches its goldens"
c01() {
	local golden="$1"
	shift
	render "${golden}" "${CHART}" "$@" &&
		assert_golden_doc "${golden}" DaemonSet joulie-agent "${golden}"
}
c01 daemonset-defaults
c01 daemonset-joulie-values -f "${REPO_ROOT}/values/joulie.yaml"
c01 daemonset-catalog --set 'hardwareCatalog.enabled=true'
c01 daemonset-nohost --set 'agent.hostNetwork=false' --set 'agent.privileged=false'
# The emulated agents' FUSE setting never reaches the DaemonSet.
render daemonset-fuse "${CHART}" --set 'agent.emulated.fuse=true' &&
	assert_golden_doc daemonset-fuse DaemonSet joulie-agent daemonset-defaults

# --------------------------------------------------------------------------
# C02: the pool StatefulSet does not use the helper and stays its golden.
# --------------------------------------------------------------------------
case_start "C02 agent pool StatefulSet matches its golden"
render statefulset-pool "${CHART}" --set 'agent.mode=pool' &&
	assert_golden_doc statefulset-pool StatefulSet joulie-agent-pool statefulset-pool

# --------------------------------------------------------------------------
# C03: the tool hooks (runtimeClassName, extraEnv, extraVolumes,
# extraVolumeMounts) render where they belong and only add lines: with every
# hook set, nothing the defaults render changes.
# --------------------------------------------------------------------------
case_start "C03 agent tool hooks only add to the DaemonSet"
HOOKS_VALUES="${GOLDEN_DIR}/hooks-values.yaml"
if render daemonset-hooks "${CHART}" -f "${HOOKS_VALUES}"; then
	assert_golden_doc daemonset-hooks DaemonSet joulie-agent daemonset-hooks
	removed="$(diff "${WORKDIR}/daemonset-defaults.DaemonSet.joulie-agent.yaml" \
		"${WORKDIR}/daemonset-hooks.DaemonSet.joulie-agent.yaml" | grep -E '^<')" || true
	if [ -n "${removed}" ]; then
		fail "DaemonSet/joulie-agent: the hooks changed lines the defaults render:"
		printf '%s\n' "${removed}" | sed 's/^/    /' >&2
	fi
	assert_doc daemonset-hooks has DaemonSet joulie-agent '^      runtimeClassName: nvidia$' "runtimeClassName hook"
	assert_doc daemonset-hooks has DaemonSet joulie-agent '^            - name: NVIDIA_DRIVER_CAPABILITIES$' "extraEnv hook"
	assert_doc daemonset-hooks has DaemonSet joulie-agent '^            - mountPath: /opt/joulie-tools$' "extraVolumeMounts hook"
	assert_doc daemonset-hooks has DaemonSet joulie-agent '^            path: /opt/joulie-tools$' "extraVolumes hook"
fi

# --------------------------------------------------------------------------
# C04: agent.mode=emulated renders one Deployment per agent.emulated.nodes
# entry and no other agent, each the golden pod spec of its node.
# --------------------------------------------------------------------------
case_start "C04 agent.mode=emulated renders one Deployment per node"
EMULATED_VALUES="${GOLDEN_DIR}/emulated-values.yaml"
EMU_NODES=(epyc-0 mi300x-0 nvl-0)
if render emulated "${CHART}" --set 'agent.mode=emulated' -f "${EMULATED_VALUES}"; then
	assert_workloads emulated \
		"Deployment/joulie-agent-emu-0" \
		"Deployment/joulie-agent-emu-1" \
		"Deployment/joulie-agent-emu-2" \
		"Deployment/joulie-controller-manager"
	assert_no_duplicates emulated
	for i in "${!EMU_NODES[@]}"; do
		doc emulated Deployment "joulie-agent-emu-${i}"
	done >"${WORKDIR}/emulated.agents.yaml"
	assert_golden "${WORKDIR}/emulated.agents.yaml" agent-emulated
fi
# A name becomes hostPaths under agent.emulated.root, so only a Node name is
# accepted; a profile is what the simcluster scripts report.
render_fails emulated-badname "${CHART}" 'is not a Node name' \
	--set 'agent.mode=emulated' --set-json 'agent.emulated.nodes=[{"name":"../x","profile":"p"}]'
render_fails emulated-noprofile "${CHART}" 'needs a profile' \
	--set 'agent.mode=emulated' --set-json 'agent.emulated.nodes=[{"name":"x"}]'
# Two entries of one name would be two agents writing one hwemu tree.
render_fails emulated-dupname "${CHART}" '"x" is listed twice' \
	--set 'agent.mode=emulated' --set-json 'agent.emulated.nodes=[{"name":"x","profile":"p"},{"name":"x","profile":"p"}]'

# --------------------------------------------------------------------------
# C05: the contract of each emulated agent pod, on the C04 render: no host
# network and no privileges, root without CAP_DAC_OVERRIDE (so a 0444 file
# of the tree refuses its writes, as kernfs does), a literal NODE_NAME, the
# emulated tools first
# on PATH, its node's hwemu tree in place of the host's, /dev/nvidiactl on
# the NVIDIA node only, Recreate, and the labels and annotations the
# simcluster scripts select on.
# --------------------------------------------------------------------------
case_start "C05 emulated agent pod contract"
for i in "${!EMU_NODES[@]}"; do
	d="joulie-agent-emu-${i}"
	n="${EMU_NODES[$i]}"
	tree="/var/lib/joulie-hwemu/${n}"
	if [ -z "$(doc emulated Deployment "${d}")" ]; then
		fail "no Deployment/${d} for ${n} to check"
		continue
	fi
	assert_doc emulated has Deployment "${d}" '^      hostNetwork: false$' "no host network"
	assert_doc emulated has Deployment "${d}" '^      dnsPolicy: ClusterFirst$' "cluster DNS"
	assert_doc emulated has Deployment "${d}" '^            privileged: false$' "not privileged"
	assert_doc emulated has Deployment "${d}" '^            runAsUser: 0$' "runs as root"
	assert_doc emulated has Deployment "${d}" '^            runAsGroup: 0$' "runs as root"
	assert_doc emulated has Deployment "${d}" '^              drop: \["DAC_OVERRIDE"\]$' "drops CAP_DAC_OVERRIDE"
	assert_doc emulated lacks Deployment "${d}" 'fieldPath: spec\.nodeName' "NODE_NAME from the pod's own node"
	assert_env emulated "${d}" NODE_NAME "${n}"
	case "$(env_of emulated "${d}" PATH)" in
	/emu/bin:*) ;;
	*) fail "Deployment/${d}: PATH=\"$(env_of emulated "${d}" PATH)\" does not start with /emu/bin" ;;
	esac
	assert_doc emulated has Deployment "${d}" '^    type: Recreate$' "strategy Recreate"
	assert_doc emulated has Deployment "${d}" '^        joulie\.io/hwemu-host: "true"$' "runs on the hwemu host"
	assert_doc emulated has Deployment "${d}" '^    app\.kubernetes\.io/component: emulated$' "component label"
	assert_doc emulated has Deployment "${d}" "^    sim\\.joulie\\.io/agent-index: \"${i}\"\$" "agent index label"
	assert_doc emulated has Deployment "${d}" "^    sim\\.joulie\\.io/node: \"${n}\"\$" "node annotation"
	assert_doc emulated has Deployment "${d}" '^    sim\.joulie\.io/profile: "[a-z0-9-]+"$' "profile annotation"
	assert_entry emulated has "${d}" volumes host-sys "^ +path: ${tree}/sys\$"
	assert_entry emulated has "${d}" volumes host-sys '^ +type: Directory$'
	assert_entry emulated has "${d}" volumes hwemu-cpuinfo "^ +path: ${tree}/cpuinfo\$"
	assert_entry emulated has "${d}" volumes hwemu-cpuinfo '^ +type: File$'
	assert_entry emulated has "${d}" volumes hwemu-state "^ +path: ${tree}/state\$"
	assert_entry emulated has "${d}" volumes hwemu-state '^ +type: Directory$'
	assert_entry emulated has "${d}" volumes hwemu-bin "^ +path: ${tree}/bin\$"
	assert_entry emulated has "${d}" volumes hwemu-bin '^ +type: Directory$'
	assert_entry emulated has "${d}" volumes hwemu-lib '^ +path: /var/lib/joulie-hwemu/lib$'
	assert_entry emulated has "${d}" volumes hwemu-lib '^ +type: Directory$'
	assert_entry emulated has "${d}" volumeMounts host-sys '^ +mountPath: /host-sys$'
	assert_entry emulated lacks "${d}" volumeMounts host-sys 'readOnly'
	assert_entry emulated lacks "${d}" volumeMounts host-sys 'mountPropagation'
	assert_entry emulated has "${d}" volumeMounts hwemu-cpuinfo '^ +mountPath: /proc/cpuinfo$'
	assert_entry emulated has "${d}" volumeMounts hwemu-cpuinfo '^ +readOnly: true$'
	assert_entry emulated has "${d}" volumeMounts hwemu-state '^ +mountPath: /emu/state$'
	assert_entry emulated lacks "${d}" volumeMounts hwemu-state 'readOnly'
	assert_entry emulated has "${d}" volumeMounts hwemu-bin '^ +mountPath: /emu/bin$'
	assert_entry emulated has "${d}" volumeMounts hwemu-bin '^ +readOnly: true$'
	assert_entry emulated has "${d}" volumeMounts hwemu-lib '^ +mountPath: /emu/lib$'
	assert_entry emulated has "${d}" volumeMounts hwemu-lib '^ +readOnly: true$'
	if [ "${n}" = nvl-0 ]; then
		assert_entry emulated has "${d}" volumes hwemu-nvidiactl "^ +path: ${tree}/dev/nvidiactl\$"
		assert_entry emulated has "${d}" volumes hwemu-nvidiactl '^ +type: File$'
		assert_entry emulated has "${d}" volumeMounts hwemu-nvidiactl '^ +mountPath: /dev/nvidiactl$'
		assert_doc emulated has Deployment "${d}" '^              name: driver-stub$' "the node's own extra mount"
	else
		assert_doc emulated lacks Deployment "${d}" 'nvidiactl' "/dev/nvidiactl off the NVIDIA node"
		assert_doc emulated lacks Deployment "${d}" 'driver-stub' "another node's extra mount"
	fi
done

# --------------------------------------------------------------------------
# C11: with agent.emulated.fuse each emulated agent mounts sys and state from
# the simulator's FUSE views (root/fuse/<node>) with mount propagation
# HostToContainer, so a restarted simulator's views reach the running pod;
# cpuinfo, the tools and /dev/nvidiactl stay on the tree, and no other line
# of the C04 render changes.
# --------------------------------------------------------------------------
case_start "C11 agent.emulated.fuse mounts the simulator's views"
if render emulated-fuse "${CHART}" --set 'agent.mode=emulated' -f "${EMULATED_VALUES}" --set 'agent.emulated.fuse=true'; then
	for i in "${!EMU_NODES[@]}"; do
		d="joulie-agent-emu-${i}"
		n="${EMU_NODES[$i]}"
		view="/var/lib/joulie-hwemu/fuse/${n}"
		assert_entry emulated-fuse has "${d}" volumes host-sys "^ +path: ${view}/sys\$"
		assert_entry emulated-fuse has "${d}" volumes hwemu-state "^ +path: ${view}/state\$"
		assert_entry emulated-fuse has "${d}" volumeMounts host-sys '^ +mountPropagation: HostToContainer$'
		assert_entry emulated-fuse has "${d}" volumeMounts hwemu-state '^ +mountPropagation: HostToContainer$'
		assert_entry emulated-fuse has "${d}" volumes hwemu-cpuinfo "^ +path: /var/lib/joulie-hwemu/${n}/cpuinfo\$"
		assert_entry emulated-fuse lacks "${d}" volumeMounts hwemu-cpuinfo 'mountPropagation'
	done
	changed="$(diff "${WORKDIR}/emulated.yaml" "${WORKDIR}/emulated-fuse.yaml" | grep -E '^[<>]' |
		grep -vE '^< +path: /var/lib/joulie-hwemu/[a-z0-9-]+/(sys|state)$' |
		grep -vE '^> +path: /var/lib/joulie-hwemu/fuse/[a-z0-9-]+/(sys|state)$' |
		grep -vE '^> +mountPropagation: HostToContainer$')" || true
	if [ -n "${changed}" ]; then
		fail "agent.emulated.fuse changes more than the sys and state mounts:"
		printf '%s\n' "${changed}" | sed 's/^/    /' >&2
	fi
fi
# The simulated cluster's FUSE values file turns it on.
if render simcluster-fuse "${CHART}" -f "${REPO_ROOT}/values/simcluster.yaml" -f "${EMULATED_VALUES}" -f "${REPO_ROOT}/values/simcluster-fuse.yaml"; then
	assert_entry simcluster-fuse has joulie-agent-emu-0 volumes host-sys "^ +path: /var/lib/joulie-hwemu/fuse/${EMU_NODES[0]}/sys\$"
fi

# --------------------------------------------------------------------------
# C06: agent.mode=none renders no agent workload.
# --------------------------------------------------------------------------
case_start "C06 agent.mode=none renders no agent"
if render agent-none "${CHART}" --set 'agent.mode=none'; then
	assert_workloads agent-none "Deployment/joulie-controller-manager"
	assert_no_duplicates agent-none
fi

# --------------------------------------------------------------------------
# C07: a scheduler extender on the host network rolls out with Recreate, as
# the old pod holds the host port the new one needs on a single node (D17).
# Off the host network the rolling update stays.
# --------------------------------------------------------------------------
case_start "C07 scheduler extender on the host network uses Recreate"
if render extender-hostnet "${CHART}" --set 'schedulerExtender.enabled=true' --set 'schedulerExtender.hostNetwork=true'; then
	assert_doc extender-hostnet has Deployment joulie-scheduler-extender '^    type: Recreate$' "strategy Recreate (D17)"
	assert_doc extender-hostnet has Deployment joulie-scheduler-extender '^      hostNetwork: true$' "host network"
fi
assert_doc scheduler-extender lacks Deployment joulie-scheduler-extender 'type: Recreate' "Recreate off the host network"

# --------------------------------------------------------------------------
# C09: the simulated cluster's values with emulated agents pull only images
# built from the working tree and imported, never from a registry.
# --------------------------------------------------------------------------
case_start "C09 values/simcluster.yaml pulls only joulie.local images"
if render simcluster "${CHART}" -f "${REPO_ROOT}/values/simcluster.yaml" -f "${EMULATED_VALUES}"; then
	assert_workloads simcluster \
		"Deployment/joulie-agent-emu-0" \
		"Deployment/joulie-agent-emu-1" \
		"Deployment/joulie-agent-emu-2" \
		"Deployment/joulie-controller-manager" \
		"Deployment/joulie-scheduler-extender"
	images="$(grep -cE '^[[:space:]]+image:' "${WORKDIR}/simcluster.yaml")" || true
	local_images="$(grep -cE '^[[:space:]]+image: "?joulie\.local/' "${WORKDIR}/simcluster.yaml")" || true
	policies="$(grep -cE '^[[:space:]]+imagePullPolicy: IfNotPresent$' "${WORKDIR}/simcluster.yaml")" || true
	if [ "${images}" != 5 ] || [ "${local_images}" != 5 ] || [ "${policies}" != 5 ]; then
		fail "want 5 containers, each a joulie.local/ image with IfNotPresent; got ${images} images, ${local_images} from joulie.local/, ${policies} IfNotPresent"
	fi
fi

# --------------------------------------------------------------------------
# C10: the agent tolerates the GPU vendor taints by default (D15), so GPU
# nodes tainted nvidia.com/gpu or amd.com/gpu:NoSchedule get an agent, and
# an empty list renders no tolerations.
# --------------------------------------------------------------------------
case_start "C10 agent DaemonSet tolerates the GPU taints by default"
C10_WANT='      tolerations:
        - effect: NoSchedule
          key: nvidia.com/gpu
          operator: Exists
        - effect: NoSchedule
          key: amd.com/gpu
          operator: Exists'
for slug in daemonset-defaults daemonset-joulie-values; do
	got="$(doc "${slug}" DaemonSet joulie-agent | awk '/^      tolerations:$/ { on=1; print; next } on == 1 && /^        / { print; next } { on=0 }')"
	if [ "${got}" != "${C10_WANT}" ]; then
		fail "DaemonSet/joulie-agent (${slug}): tolerations differ (- want, + got)"
		diff -u <(printf '%s\n' "${C10_WANT}") <(printf '%s\n' "${got}") | sed 's/^/    /' >&2
	fi
done
if render daemonset-notolerations "${CHART}" --set-json 'agent.tolerations=[]'; then
	assert_doc daemonset-notolerations lacks DaemonSet joulie-agent 'tolerations:' "no tolerations when the list is empty"
fi

# --------------------------------------------------------------------------
# The simulator chart, rendered the way the integration job deploys it.
# --------------------------------------------------------------------------
SIM_DEPLOY="joulie-telemetry-sim"
SIM_LEGACY_VOLUMES=(node-classes workload-trace hardware-catalog)
case_start "joulie-simulator defaults"
if render simulator "${SIM_CHART}"; then
	assert_workloads simulator "Deployment/${SIM_DEPLOY}"
	assert_no_duplicates simulator
	assert_image_tags simulator latest
	assert_optional_volumes simulator "${SIM_DEPLOY}" "${SIM_LEGACY_VOLUMES[@]}"
	assert_doc simulator has Deployment "${SIM_DEPLOY}" 'fieldPath: metadata\.name$' "POD_NAME from the downward API"
	assert_doc simulator lacks Deployment "${SIM_DEPLOY}" 'type: Recreate$' "Recreate outside emulate mode"
	assert_doc simulator lacks Deployment "${SIM_DEPLOY}" 'runAsUser: 0$' "root outside emulate mode"
	assert_env simulator "${SIM_DEPLOY}" SIM_EMULATE -
	assert_doc simulator lacks Deployment "${SIM_DEPLOY}" '^ +- name: OTEL_' "OTEL_* outside emulate mode"
	assert_env simulator "${SIM_DEPLOY}" SIM_WORKLOAD_TRACE_PATH /etc/joulie-sim-trace/trace.jsonl
	assert_doc simulator lacks ClusterRole joulie-simulator 'resources: \["jobs"\]' "no workload run rules outside emulate mode"
	assert_doc simulator lacks ClusterRole joulie-simulator 'verbs: \["patch", "deletecollection"\]' "no pod patch outside emulate mode"
fi

# --------------------------------------------------------------------------
# Emulate mode with the smoke cluster spec, the way simcluster deploys it: one
# root-owned writer of the hostPath trees, never two pods at once.
# --------------------------------------------------------------------------
case_start "joulie-simulator emulate"
SMOKE_SPEC="${REPO_ROOT}/tests/simcluster/specs/smoke.yaml"
EMULATE_ARGS=(
	--set 'emulate.enabled=true'
	--set-file "emulate.clusterSpec=${SMOKE_SPEC}"
	--set 'serviceMonitor.enabled=true'
	--set 'emulate.exporters={kepler-0.7,dcgm-4.8.4,amd-dme-1.5.2}'
	--set 'otel.metricsEndpoint=http://p:9090/api/v1/otlp/v1/metrics'
	--set 'otel.logsEndpoint=http://c:4318/v1/logs'
	--set 'otel.resourceAttributes.a=b'
	--set 'otel.resourceAttributes.c=d'
)
if render simulator-emulate "${SIM_CHART}" "${EMULATE_ARGS[@]}"; then
	assert_workloads simulator-emulate "Deployment/${SIM_DEPLOY}"
	assert_no_duplicates simulator-emulate
	assert_doc simulator-emulate has Deployment "${SIM_DEPLOY}" '^    type: Recreate$' "strategy Recreate"
	assert_doc simulator-emulate has Deployment "${SIM_DEPLOY}" '^ +runAsUser: 0$' "runs as root"
	assert_doc simulator-emulate has Deployment "${SIM_DEPLOY}" '^ +runAsNonRoot: false$' "runs as root"
	assert_doc simulator-emulate has Deployment "${SIM_DEPLOY}" '^ +path: /var/lib/joulie-hwemu$' "hostPath at emulate.root"
	assert_doc simulator-emulate has Deployment "${SIM_DEPLOY}" '^ +type: DirectoryOrCreate$' "hostPath created by the simulator"
	assert_doc simulator-emulate has Deployment "${SIM_DEPLOY}" '^ +mountPath: /var/lib/joulie-hwemu$' "tree root mounted at the same path"
	assert_doc simulator-emulate has Deployment "${SIM_DEPLOY}" '^ +path: /readyz$' "readiness on /readyz"
	assert_doc simulator-emulate has Deployment "${SIM_DEPLOY}" '^ +joulie\.io/hwemu-host: "true"$' "runs on the hwemu host"
	assert_env simulator-emulate "${SIM_DEPLOY}" SIM_EMULATE true
	assert_env simulator-emulate "${SIM_DEPLOY}" SIM_HWEMU_ROOT /var/lib/joulie-hwemu
	assert_env simulator-emulate "${SIM_DEPLOY}" SIM_CLUSTER_SPEC /etc/joulie-sim-spec/cluster-spec.yaml
	assert_env simulator-emulate "${SIM_DEPLOY}" SIM_EMULATE_TICK 1s
	assert_env simulator-emulate "${SIM_DEPLOY}" SIM_EXPORTERS kepler-0.7,dcgm-4.8.4,amd-dme-1.5.2
	assert_env simulator-emulate "${SIM_DEPLOY}" OTEL_EXPORTER_OTLP_METRICS_ENDPOINT http://p:9090/api/v1/otlp/v1/metrics
	assert_env simulator-emulate "${SIM_DEPLOY}" OTEL_EXPORTER_OTLP_LOGS_ENDPOINT http://c:4318/v1/logs
	assert_env simulator-emulate "${SIM_DEPLOY}" OTEL_METRIC_EXPORT_INTERVAL 10000
	assert_env simulator-emulate "${SIM_DEPLOY}" OTEL_RESOURCE_ATTRIBUTES a=b,c=d
	assert_env simulator-emulate "${SIM_DEPLOY}" SIM_WORKLOAD_TRACE_PATH -
	assert_optional_volumes simulator-emulate "${SIM_DEPLOY}" "${SIM_LEGACY_VOLUMES[@]}"
	assert_count simulator-emulate '^ConfigMap/joulie-simulator-cluster-spec$' 1
	assert_doc simulator-emulate has ConfigMap joulie-simulator-cluster-spec '^    kind: ClusterSpec$' "holds the cluster spec"
	assert_doc simulator-emulate has Deployment "${SIM_DEPLOY}" '^ +name: joulie-simulator-cluster-spec$' "spec volume from the chart's ConfigMap"
	assert_doc simulator-emulate has Deployment "${SIM_DEPLOY}" '^ +checksum/cluster-spec: [0-9a-f]{64}$' "restarts on a changed spec"
	assert_doc simulator-emulate has ServiceMonitor "${SIM_DEPLOY}" '^ +path: /exporters/kepler-0\.7/metrics$' "scrapes kepler-0.7"
	assert_doc simulator-emulate has ServiceMonitor "${SIM_DEPLOY}" '^ +path: /exporters/dcgm-4\.8\.4/metrics$' "scrapes dcgm-4.8.4"
	assert_doc simulator-emulate has ServiceMonitor "${SIM_DEPLOY}" '^ +path: /exporters/amd-dme-1\.5\.2/metrics$' "scrapes amd-dme-1.5.2"
	assert_doc simulator-emulate has ClusterRole joulie-simulator 'resources: \["jobs"\]' "workload run rules in emulate mode"
	assert_doc simulator-emulate has ClusterRole joulie-simulator 'verbs: \["patch", "deletecollection"\]' "pod patch in emulate mode"
	assert_doc simulator-emulate has ClusterRole joulie-simulator 'resources: \["nodetwins"\]' "NodeTwin reads in emulate mode"
	if [ "$(doc simulator-emulate ServiceMonitor "${SIM_DEPLOY}" | grep -cE '^ +honorLabels: false$')" != 3 ]; then
		fail "ServiceMonitor/${SIM_DEPLOY}: want honorLabels: false on all three exporter endpoints"
	fi
	assert_env simulator-emulate "${SIM_DEPLOY}" SIM_HWEMU_FUSE -
	assert_doc simulator-emulate lacks Deployment "${SIM_DEPLOY}" 'privileged' "unprivileged without emulate.fuse"
	assert_doc simulator-emulate lacks Deployment "${SIM_DEPLOY}" 'mountPropagation' "no mount propagation without emulate.fuse"
	assert_doc simulator-emulate lacks Deployment "${SIM_DEPLOY}" '/dev/fuse' "no /dev/fuse without emulate.fuse"
fi

# --------------------------------------------------------------------------
# emulate.fuse: the simulator serves the views through FUSE, which takes
# /dev/fuse and CAP_SYS_ADMIN, and binds them into the root, which reaches
# the host only from a privileged container with Bidirectional propagation.
# --------------------------------------------------------------------------
case_start "joulie-simulator emulate.fuse"
if render simulator-fuse "${SIM_CHART}" "${EMULATE_ARGS[@]}" -f "${REPO_ROOT}/values/simcluster-simulator-fuse.yaml"; then
	assert_workloads simulator-fuse "Deployment/${SIM_DEPLOY}"
	assert_env simulator-fuse "${SIM_DEPLOY}" SIM_HWEMU_FUSE true
	assert_doc simulator-fuse has Deployment "${SIM_DEPLOY}" '^ +privileged: true$' "privileged"
	assert_doc simulator-fuse has Deployment "${SIM_DEPLOY}" '^ +add: \["SYS_ADMIN"\]$' "CAP_SYS_ADMIN"
	assert_entry simulator-fuse has "${SIM_DEPLOY}" volumeMounts hwemu-root '^ +mountPropagation: Bidirectional$'
	assert_entry simulator-fuse has "${SIM_DEPLOY}" volumeMounts dev-fuse '^ +mountPath: /dev/fuse$'
	assert_entry simulator-fuse has "${SIM_DEPLOY}" volumes dev-fuse '^ +path: /dev/fuse$'
	assert_entry simulator-fuse has "${SIM_DEPLOY}" volumes dev-fuse '^ +type: CharDevice$'
	assert_doc simulator-fuse has Deployment "${SIM_DEPLOY}" '^    type: Recreate$' "strategy Recreate"
fi
render_fails simulator-emulate-replicas "${SIM_CHART}" 'requires replicas: 1' \
	--set 'emulate.enabled=true' --set 'emulate.clusterSpecConfigMap=existing-spec' --set 'replicas=2'
render_fails simulator-emulate-nospec "${SIM_CHART}" 'needs a cluster spec' \
	--set 'emulate.enabled=true'
render_fails simulator-emulate-twospecs "${SIM_CHART}" 'not both' \
	--set 'emulate.enabled=true' --set 'emulate.clusterSpecConfigMap=existing-spec' \
	--set 'emulate.clusterSpec=kind: ClusterSpec'

# --------------------------------------------------------------------------
# rbac.workloadRuns: auto grants the v1 workload run rules with emulate mode,
# false withholds them there, true grants them outside it, and an unquoted
# boolean reads as its word. In emulate mode extraEnv still sets a trace path.
# --------------------------------------------------------------------------
case_start "joulie-simulator rbac.workloadRuns"
if render simulator-runs-off "${SIM_CHART}" --set 'emulate.enabled=true' --set 'emulate.clusterSpecConfigMap=existing-spec' \
	--set 'rbac.workloadRuns=false' --set 'extraEnv.SIM_WORKLOAD_TRACE_PATH=/etc/joulie-sim-trace/trace.jsonl'; then
	assert_doc simulator-runs-off lacks ClusterRole joulie-simulator 'resources: \["jobs"\]' "no workload run rules with workloadRuns false"
	assert_doc simulator-runs-off has ClusterRole joulie-simulator 'resources: \["pods", "nodes"\]' "the base rules"
	assert_env simulator-runs-off "${SIM_DEPLOY}" SIM_WORKLOAD_TRACE_PATH /etc/joulie-sim-trace/trace.jsonl
fi
if render simulator-runs-on "${SIM_CHART}" --set 'rbac.workloadRuns=true'; then
	assert_doc simulator-runs-on has ClusterRole joulie-simulator 'resources: \["jobs"\]' "workload run rules with workloadRuns true"
fi
render_fails simulator-runs-bad "${SIM_CHART}" 'rbac.workloadRuns is "maybe"' --set 'rbac.workloadRuns=maybe'

# --------------------------------------------------------------------------
# Emulate mode with a spec ConfigMap the user manages: the chart mounts it and
# renders no ConfigMap of its own, nor a checksum of a spec it does not see.
# --------------------------------------------------------------------------
case_start "joulie-simulator emulate, spec in an existing ConfigMap"
if render simulator-emulate-cm "${SIM_CHART}" \
	--set 'emulate.enabled=true' --set 'emulate.clusterSpecConfigMap=existing-spec'; then
	assert_workloads simulator-emulate-cm "Deployment/${SIM_DEPLOY}"
	assert_count simulator-emulate-cm '^ConfigMap/joulie-simulator-cluster-spec$' 0
	assert_doc simulator-emulate-cm has Deployment "${SIM_DEPLOY}" '^ +name: existing-spec$' "spec volume from emulate.clusterSpecConfigMap"
	assert_doc simulator-emulate-cm lacks Deployment "${SIM_DEPLOY}" 'checksum/cluster-spec' "checksum of a spec the chart does not render"
	assert_env simulator-emulate-cm "${SIM_DEPLOY}" SIM_CLUSTER_SPEC /etc/joulie-sim-spec/cluster-spec.yaml
fi

# --------------------------------------------------------------------------
# Packaged the way the release packages them (--app-version), both charts must
# pull the images of that version and not ":latest", also with the values file
# the quickstart installs with. An explicit tag still wins.
# --------------------------------------------------------------------------
case_start "C08 packaged charts pull their appVersion"
APP_VERSION="9.9.9-rc1"
if helm package "${CHART}" "${SIM_CHART}" --version "${APP_VERSION}" --app-version "${APP_VERSION}" \
	--destination "${WORKDIR}/pkg" >"${WORKDIR}/package.err" 2>&1; then
	PKG="${WORKDIR}/pkg/joulie-${APP_VERSION}.tgz"
	SIM_PKG="${WORKDIR}/pkg/joulie-sim-${APP_VERSION}.tgz"
	render packaged "${PKG}" --set 'schedulerExtender.enabled=true' &&
		assert_image_tags packaged "${APP_VERSION}"
	render packaged-pool "${PKG}" --set 'agent.mode=pool' &&
		assert_image_tags packaged-pool "${APP_VERSION}"
	# C08: the emulated agents too.
	render packaged-emulated "${PKG}" --set 'agent.mode=emulated' -f "${EMULATED_VALUES}" &&
		assert_count packaged-emulated '^Deployment/joulie-agent-emu-[0-9]+$' 3 &&
		assert_image_tags packaged-emulated "${APP_VERSION}"
	render packaged-values "${PKG}" -f "${REPO_ROOT}/values/joulie.yaml" &&
		assert_image_tags packaged-values "${APP_VERSION}"
	render packaged-sim "${SIM_PKG}" &&
		assert_image_tags packaged-sim "${APP_VERSION}"
	render packaged-pinned "${PKG}" --set 'agent.image.tag=pinned' --set 'controllerManager.image.tag=pinned' &&
		assert_image_tags packaged-pinned "pinned"
else
	fail "helm package failed"
	sed 's/^/    /' "${WORKDIR}/package.err" >&2
fi

# --------------------------------------------------------------------------
# The controller manager reads the OpenTelemetry inputs by default: measured
# node power from Prometheus (cpu and gpu components, 5m rate window) and the
# otel facility preset. The chart must not set NODE_POWER_PRESET,
# PROMETHEUS_RESOURCE_ATTRIBUTES or PROMETHEUS_NAME_TRANSLATION: it renders
# every env key, so a preset there would turn an adopter's
# NODE_POWER_PROMETHEUS_QUERY into a fatal "preset plus query" error instead of
# the custom preset. KEPLER_AVAILABLE (read by nothing) and
# FACILITY_PROMETHEUS_ADDRESS (PROMETHEUS_ADDRESS is its default) are removed.
# --------------------------------------------------------------------------
case_start "controller manager reads OTel inputs by default"
CM_DEPLOY="joulie-controller-manager"
for otel_case in otel-defaults otel-release-values otel-custom-query; do
	case "${otel_case}" in
	otel-defaults) otel_args=() ;;
	otel-release-values) otel_args=(-f "${REPO_ROOT}/values/joulie.yaml") ;;
	otel-custom-query) otel_args=(--set 'controllerManager.env.NODE_POWER_PROMETHEUS_QUERY=sum by (node) (power)') ;;
	esac
	render "${otel_case}" "${CHART}" ${otel_args[@]+"${otel_args[@]}"} || continue
	assert_env "${otel_case}" "${CM_DEPLOY}" PROMETHEUS_ADDRESS "http://prometheus-operated.monitoring:9090"
	assert_env "${otel_case}" "${CM_DEPLOY}" NODE_POWER_SOURCE prometheus
	assert_env "${otel_case}" "${CM_DEPLOY}" NODE_POWER_SCOPE components
	assert_env "${otel_case}" "${CM_DEPLOY}" PROMETHEUS_RATE_WINDOW 5m
	assert_env "${otel_case}" "${CM_DEPLOY}" FACILITY_PRESET otel
	for absent in NODE_POWER_PRESET PROMETHEUS_RESOURCE_ATTRIBUTES PROMETHEUS_NAME_TRANSLATION \
		KEPLER_AVAILABLE FACILITY_PROMETHEUS_ADDRESS; do
		assert_env "${otel_case}" "${CM_DEPLOY}" "${absent}" -
	done
done
assert_env otel-custom-query "${CM_DEPLOY}" NODE_POWER_PROMETHEUS_QUERY "sum by (node) (power)"

# --------------------------------------------------------------------------
# GC12: governor control is off unless the operator turns it on. The agent
# writes nothing without its own GOVERNOR_WRITES either, but the controller
# manager must not ask: a governor changes how every node runs. The preset
# defaults render as documented, the GPU performance preset empty (write none).
# --------------------------------------------------------------------------
case_start "GC12 governor control is off by default"
for gov_case in governor-defaults governor-release-values; do
	case "${gov_case}" in
	governor-defaults) gov_args=() ;;
	governor-release-values) gov_args=(-f "${REPO_ROOT}/values/joulie.yaml") ;;
	esac
	render "${gov_case}" "${CHART}" ${gov_args[@]+"${gov_args[@]}"} || continue
	assert_env "${gov_case}" "${CM_DEPLOY}" GOVERNOR_CONTROL off
	assert_env "${gov_case}" "${CM_DEPLOY}" CPU_GOVERNOR_PERFORMANCE_PRESET performance
	assert_env "${gov_case}" "${CM_DEPLOY}" CPU_GOVERNOR_ECO_PRESET efficiency
	assert_env "${gov_case}" "${CM_DEPLOY}" CPU_GOVERNOR_ON_GPU_NODES false
	# Empty, but rendered: an empty value and an absent one read the same here.
	assert_doc "${gov_case}" has Deployment "${CM_DEPLOY}" '^ +- name: GPU_GOVERNOR_PERFORMANCE_PRESET$' "GPU_GOVERNOR_PERFORMANCE_PRESET rendered"
	assert_env "${gov_case}" "${CM_DEPLOY}" GPU_GOVERNOR_PERFORMANCE_PRESET ""
	assert_env "${gov_case}" "${CM_DEPLOY}" GPU_GOVERNOR_ECO_PRESET efficient
	assert_env "${gov_case}" "${CM_DEPLOY}" GPU_GOVERNOR_ECO_CEILING_PCT_OF_MAX 80
done

# --------------------------------------------------------------------------
# GA27: the agent writes no governor unless the operator turns it on, and
# restores the baseline when it stops. Governor writes use the mounts and
# privileges the DVFS fallback already has: without the two variables the
# DaemonSet renders exactly as with them, less their four env lines.
# --------------------------------------------------------------------------
case_start "GA27 agent governor writes are off by default"
# daemonset_env prints the value of one env var of the agent DaemonSet.
daemonset_env() {
	doc "$1" DaemonSet joulie-agent |
		awk -v var="$2" '
			$0 ~ "^[[:space:]]+- name: " var "$" { found=1; next }
			found == 1 { sub(/^[[:space:]]+value:[[:space:]]*/, ""); gsub(/"/, ""); print; exit }
		'
}
for ga_case in agent-governor-defaults agent-governor-release-values; do
	case "${ga_case}" in
	agent-governor-defaults) ga_args=() ;;
	agent-governor-release-values) ga_args=(-f "${REPO_ROOT}/values/joulie.yaml") ;;
	esac
	render "${ga_case}" "${CHART}" ${ga_args[@]+"${ga_args[@]}"} || continue
	for want in GOVERNOR_WRITES=false GOVERNOR_RESTORE_ON_EXIT=true; do
		got="$(daemonset_env "${ga_case}" "${want%%=*}")"
		if [ "${got}" != "${want#*=}" ]; then
			fail "DaemonSet/joulie-agent: env ${want%%=*}=\"${got}\", want \"${want#*=}\""
		fi
	done
done
if render agent-governor-unset "${CHART}" --set 'agent.env.GOVERNOR_WRITES=null' --set 'agent.env.GOVERNOR_RESTORE_ON_EXIT=null'; then
	doc agent-governor-defaults DaemonSet joulie-agent >"${WORKDIR}/ga27.with.yaml"
	doc agent-governor-unset DaemonSet joulie-agent >"${WORKDIR}/ga27.without.yaml"
	added="$(diff "${WORKDIR}/ga27.without.yaml" "${WORKDIR}/ga27.with.yaml" | grep -E '^[<>]')" || true
	want_added="$(printf '%s\n' \
		'>             - name: GOVERNOR_RESTORE_ON_EXIT' \
		'>               value: "true"' \
		'>             - name: GOVERNOR_WRITES' \
		'>               value: "false"')"
	if [ "${added}" != "${want_added}" ]; then
		fail "DaemonSet/joulie-agent: the governor variables change more than their env lines:"
		printf '%s\n' "${added}" | sed 's/^/    /' >&2
	fi
fi

echo
if [ "${FAILURES}" -ne 0 ]; then
	echo "verify-chart-renders.sh: FAIL (${FAILURES} assertion(s) over ${CASES} cases)" >&2
	exit 1
fi
echo "verify-chart-renders.sh: PASS (${CASES} cases)"
