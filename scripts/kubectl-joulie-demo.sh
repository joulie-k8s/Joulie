#!/usr/bin/env bash
#
# kubectl-joulie-demo.sh records the kubectl joulie GIF of the README on the
# simulated cluster: the demo scenario (tests/simcluster/scenarios/demo.yaml,
# 10 Nodes in five hardware families under 40 minutes of mixed work) played
# by scripts/kubectl-joulie-demo.tape in VHS, in Docker.
#
# Usage: scripts/kubectl-joulie-demo.sh [output.gif]
#
# The output defaults to website/static/images/kubectl-joulie.gif. Needs what
# the simulated cluster needs (website/content/en/docs/simulator/simulated-cluster.md).
# Starts the scenario in the background on the cluster SIMCLUSTER_NAME
# (default joulie-sim-demo) unless one already runs there, waits until 10
# pods run, records, and keeps the cluster: make simcluster-down
# SIMCLUSTER_NAME=joulie-sim-demo deletes it.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
out=$(realpath -m "${1:-$root/website/static/images/kubectl-joulie.gif}")
export SIMCLUSTER_NAME=${SIMCLUSTER_NAME:-joulie-sim-demo}
state=${SIMCLUSTER_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/joulie/simcluster/$SIMCLUSTER_NAME}
vhs=ghcr.io/charmbracelet/vhs:v0.12.1@sha256:ea49a6a1c529be83153e88321892b5585964418f0b9055e8c1e0d732194234a3
go=${GO:-go}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/bin" "$work/out"
(cd "$root" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 "$go" build -o "$work/bin/kubectl-joulie" ./cmd/kubectl-joulie)
# The image has no kubectl: this stand-in runs kubectl joulie as kubectl does.
cat >"$work/bin/kubectl" <<'EOF'
#!/bin/sh
[ "$1" = joulie ] && { shift; exec kubectl-joulie "$@"; }
echo "only kubectl joulie is available here" >&2
exit 1
EOF
chmod +x "$work/bin/kubectl"

running() {
	[ -f "$state/kubeconfig" ] || return 1
	KUBECONFIG=$state/kubeconfig "$work/bin/kubectl-joulie" pods -o json 2>/dev/null |
		python3 -c 'import json,sys; sys.exit(0 if len(json.load(sys.stdin).get("pods") or []) >= 10 else 1)'
}
if ! running; then
	echo "starting the demo scenario on $SIMCLUSTER_NAME (log: $state/demo-scenario.log)" >&2
	mkdir -p "$state"
	nohup bash "$root/scripts/simcluster/scenario.sh" tests/simcluster/scenarios/demo.yaml >"$state/demo-scenario.log" 2>&1 &
	for _ in $(seq 180); do
		running && break
		sleep 10
	done
	running || { echo "no 10 pods running after 30 minutes; see $state/demo-scenario.log" >&2; exit 1; }
fi

cp "$state/kubeconfig" "$work/kubeconfig"
cp "$root/scripts/kubectl-joulie-demo.tape" "$work/demo.tape"
# The API server listens on the host's 127.0.0.1.
docker run --rm --network host -v "$work:/demo" -w /demo "$vhs" demo.tape >&2
mkdir -p "$(dirname "$out")"
cp "$work/out/kubectl-joulie.gif" "$out"
echo "$out"
