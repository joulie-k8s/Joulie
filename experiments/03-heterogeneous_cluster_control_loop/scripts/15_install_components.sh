#!/usr/bin/env bash
# Install/uninstall Joulie components for a given scenario.
#
# Usage:
#   ./15_install_components.sh <scenario>
#
# Scenarios:
#   A — No Joulie components (uninstall if present)
#   B — Operator + Agent only (caps, no scheduler)
#   C — Operator + Agent + Scheduler extender
set -euo pipefail

SCENARIO="${1:?Usage: $0 <A|B|C>}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
CLUSTER_NAME="${CLUSTER_NAME:-joulie-control-loop-exp03}"

export KUBECONFIG="${KUBECONFIG:-$(kwokctl get kubeconfig-path --name "$CLUSTER_NAME")}"

echo "=== Installing components for scenario $SCENARIO ==="

# Apply CRDs (always needed for resource collection even without operator)
echo "  Applying Joulie CRDs..."
kubectl apply -f "$REPO_ROOT/config/crd/bases/" --server-side 2>/dev/null || \
  kubectl apply -f "$REPO_ROOT/config/crd/bases/"

if [[ "$SCENARIO" == "A" ]]; then
  echo "  Scenario A: no Joulie components."
  # Uninstall operator/agent/scheduler if present
  helm uninstall joulie -n joulie-system 2>/dev/null || true
  kubectl delete deployment joulie-scheduler-extender -n joulie-system 2>/dev/null || true
  echo "  Done (baseline only)."
  exit 0
fi

# Scenarios B and C need operator + agent
echo "  Installing operator + agent via Helm..."
HELM_ARGS=(
  upgrade --install joulie "$REPO_ROOT/charts/joulie"
  -n joulie-system --create-namespace
  --set "operator.image.pullPolicy=IfNotPresent"
  --set "agent.image.pullPolicy=IfNotPresent"
)

helm "${HELM_ARGS[@]}"

echo "  Waiting for operator rollout..."
kubectl -n joulie-system rollout status deploy/joulie-operator --timeout=120s 2>/dev/null || true

# Scenario C needs the scheduler extender
if [[ "$SCENARIO" == "C" ]]; then
  echo "  Deploying scheduler extender..."
  kubectl apply -f "$REPO_ROOT/charts/joulie/templates/scheduler-extender.yaml" 2>/dev/null || \
    echo "  (scheduler extender template applied or skipped)"
  echo "  Scheduler extender deployed."
else
  echo "  Scenario B: no scheduler extender."
  kubectl delete deployment joulie-scheduler-extender -n joulie-system 2>/dev/null || true
fi

echo "  Components installed for scenario $SCENARIO."
