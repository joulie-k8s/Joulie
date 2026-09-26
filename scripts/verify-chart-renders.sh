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
# Requires helm and awk. Exit status 0 when every case passes, 1 otherwise.

set -u -o pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHART="${REPO_ROOT}/charts/joulie"
SIM_CHART="${REPO_ROOT}/charts/joulie-simulator"
RELEASE="joulie"
VERBOSE=0

usage() {
	cat <<'EOF'
verify-chart-renders.sh - render the Joulie charts under the value combinations
CI covers and assert the workload objects they produce.

Usage:
  scripts/verify-chart-renders.sh [options]

Options:
  -h, --help     Show this help and exit.
  -v, --verbose  Print the rendered Kind/name list for every case.

Cases:
  defaults                 chart defaults
  values/joulie.yaml       the values file shipped for real installs
  legacy operator: key     asserts it renders byte for byte what
                           controllerManager: renders
  leaderElection.enabled   asserts the Lease Role and RoleBinding appear
  agent.mode=pool          asserts the StatefulSet replaces the DaemonSet
  schedulerExtender        asserts a second Deployment, one per component
  joulie-simulator         the simulator chart
  packaged appVersion      packages both charts the way the release does and
                           asserts every image is tagged with the appVersion,
                           also under values/joulie.yaml, unless a tag is set

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
# The simulator chart, rendered the way the integration job deploys it.
# --------------------------------------------------------------------------
case_start "joulie-simulator defaults"
if render simulator "${SIM_CHART}"; then
	assert_workloads simulator "Deployment/joulie-telemetry-sim"
	assert_no_duplicates simulator
	assert_image_tags simulator latest
fi

# --------------------------------------------------------------------------
# Packaged the way the release packages them (--app-version), both charts must
# pull the images of that version and not ":latest", also with the values file
# the quickstart installs with. An explicit tag still wins.
# --------------------------------------------------------------------------
case_start "packaged charts pull their appVersion"
APP_VERSION="9.9.9-rc1"
if helm package "${CHART}" "${SIM_CHART}" --version "${APP_VERSION}" --app-version "${APP_VERSION}" \
	--destination "${WORKDIR}/pkg" >"${WORKDIR}/package.err" 2>&1; then
	PKG="${WORKDIR}/pkg/joulie-${APP_VERSION}.tgz"
	SIM_PKG="${WORKDIR}/pkg/joulie-sim-${APP_VERSION}.tgz"
	render packaged "${PKG}" --set 'schedulerExtender.enabled=true' &&
		assert_image_tags packaged "${APP_VERSION}"
	render packaged-pool "${PKG}" --set 'agent.mode=pool' &&
		assert_image_tags packaged-pool "${APP_VERSION}"
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

echo
if [ "${FAILURES}" -ne 0 ]; then
	echo "verify-chart-renders.sh: FAIL (${FAILURES} assertion(s) over ${CASES} cases)" >&2
	exit 1
fi
echo "verify-chart-renders.sh: PASS (${CASES} cases)"
