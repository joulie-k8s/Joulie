#!/usr/bin/env bash
# Set up a KWOK cluster for experiment 04.
# Creates fake nodes matching cluster-nodes.yaml and installs Joulie CRDs.
set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-joulie-control-loop-exp04}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
CONFIGS_DIR="$SCRIPT_DIR/../configs"

echo "=== Experiment 04: Setting up KWOK cluster ==="
echo "Cluster: $CLUSTER_NAME"

# Create cluster
if kwokctl get cluster "$CLUSTER_NAME" &>/dev/null; then
  echo "Cluster $CLUSTER_NAME already exists, reusing."
else
  echo "Creating cluster..."
  kwokctl create cluster --name "$CLUSTER_NAME"
fi

export KUBECONFIG="$(kwokctl get kubeconfig-path --name "$CLUSTER_NAME")"
echo "KUBECONFIG=$KUBECONFIG"

# Apply Joulie CRDs
echo "Installing Joulie CRDs..."
kubectl apply -f "$REPO_ROOT/config/crd/bases/"

# Generate and apply KWOK nodes from cluster-nodes.yaml
echo "Applying KWOK nodes..."
python3 "$SCRIPT_DIR/generate_kwok_nodes.py" \
  "$CONFIGS_DIR/cluster-nodes.yaml" \
  | kubectl apply -f -

echo ""
echo "Cluster ready. Nodes:"
kubectl get nodes -o wide
echo ""
echo "To use this cluster:"
echo "  export KUBECONFIG=$KUBECONFIG"
