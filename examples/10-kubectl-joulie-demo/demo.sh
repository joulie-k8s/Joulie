#!/usr/bin/env bash
# Joulie kubectl plugin demo — full setup + guided presentation
# Usage: ./examples/10-kubectl-joulie-demo/demo.sh <kind-cluster-name>
#
# Assumes:
#   - kubectl-joulie plugin is already installed (run: make kubectl-plugin && install bin/kubectl-joulie ~/.local/bin/)
#   - A trace file exists at examples/10-kubectl-joulie-demo/trace.jsonl (generate with: make -C examples/10-kubectl-joulie-demo trace)
set -euo pipefail

CLUSTER="${1:?Usage: $0 <kind-cluster-name>}"
DEMO_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_DIR="$(cd "$DEMO_DIR/../.." && pwd)"
cd "$REPO_DIR"

DEMO_NS="demo"

pause() {
  echo ""
  echo ">>> Press ENTER to continue..."
  read -r
}

# ─────────────────────────────────────────────────────────
# INFRASTRUCTURE SETUP
# ─────────────────────────────────────────────────────────

echo "=== 1/6 Create kind cluster ==="
kind delete cluster --name "$CLUSTER"
kind create cluster --config "$DEMO_DIR/kind-cluster.yaml" --name "$CLUSTER"
kind get kubeconfig --name "$CLUSTER" > "$DEMO_DIR/kubeconfig.yaml"
export KUBECONFIG="$DEMO_DIR/kubeconfig.yaml"
kubectl apply -f https://github.com/kubernetes-sigs/kwok/releases/download/v0.7.0/kwok.yaml

echo "=== 2/6 KWOK stages + nodes ==="
kubectl apply -f "$DEMO_DIR/00-kwok-stages.yaml"
kubectl apply -f "$DEMO_DIR/01-kwok-nodes.yaml"

echo "=== 3/6 Build images ==="
make build TAG=demo
make simulator-build TAG=demo
kind load docker-image "registry.cern.ch/mbunino/joulie/joulie-agent:demo" --name "$CLUSTER"
kind load docker-image "registry.cern.ch/mbunino/joulie/joulie-operator:demo" --name "$CLUSTER"
kind load docker-image "registry.cern.ch/mbunino/joulie/joulie-scheduler:demo" --name "$CLUSTER"
kind load docker-image "registry.cern.ch/mbunino/joulie/joulie-simulator:demo" --name "$CLUSTER"

echo "=== 4/6 kube-prometheus-stack ==="
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts --force-update 2>/dev/null
helm upgrade --install telemetry prometheus-community/kube-prometheus-stack \
  -n monitoring --create-namespace \
  -f "$DEMO_DIR/prometheus-values.yaml" \
  --wait --timeout 3m

echo "=== 5/6 Simulator (no workload yet) ==="
kubectl create namespace joulie-system 2>/dev/null || true
kubectl create namespace "$DEMO_NS" 2>/dev/null || true

kubectl create configmap joulie-simulator-node-classes \
  -n joulie-system \
  --from-file=node-classes.yaml="$DEMO_DIR/node-classes-data.yaml" \
  --dry-run=client -o yaml | kubectl apply -f -

# Empty trace — simulator starts idle
kubectl create configmap joulie-simulator-workload-trace \
  -n joulie-system \
  --from-literal=trace.jsonl="" \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl create configmap joulie-simulator-hardware-catalog \
  -n joulie-system \
  --from-literal=hardware.generated.yaml="" \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl create configmap joulie-hardware-catalog \
  -n joulie-system \
  --from-file=hardware.yaml=simulator/catalog/hardware.yaml \
  --dry-run=client -o yaml | kubectl apply -f -

helm upgrade --install joulie-sim charts/joulie-simulator \
  -n joulie-system \
  -f "$DEMO_DIR/sim-values.yaml" \
  --wait --timeout 2m

kubectl apply -f "$DEMO_DIR/03-simulator-servicemonitor.yaml"
kubectl apply -f "$DEMO_DIR/04-joulie-servicemonitors.yaml"

echo "=== 6/6 Install Joulie ==="
helm upgrade --install joulie charts/joulie \
  -n joulie-system \
  -f "$DEMO_DIR/joulie-values.yaml" \
  --set operator.image.tag=demo \
  --set operator.image.pullPolicy=IfNotPresent \
  --set agent.image.tag=demo \
  --set agent.image.pullPolicy=IfNotPresent \
  --set schedulerExtender.image.tag=demo \
  --set schedulerExtender.image.pullPolicy=IfNotPresent \
  --set grafanaDashboard.enabled=true \
  --set hardwareCatalog.enabled=true \
  --wait --timeout 2m

echo "=== Open Grafana ==="
kubectl port-forward -n monitoring svc/telemetry-grafana 3300:80 1>/dev/null &
GRAFANA_PID=$!
echo "Grafana running at http://localhost:3300 (admin / joulie)"

echo ""
echo "============================================"
echo "  SETUP COMPLETE — STARTING DEMO"
echo "============================================"

# ─────────────────────────────────────────────────────────
# DEMO PRESENTATION
# ─────────────────────────────────────────────────────────

pause

echo ""
echo ">>> Step 1: Show the cluster (idle, no workload)"
echo ""
kubectl get nodes -L nvidia.com/gpu.product,amd.com/gpu.product,joulie.io/power-profile
echo ""
kubectl joulie status

pause

echo ""
echo ">>> Step 2: Launch workload — loading trace into simulator..."
echo ""

# Load real trace and restart simulator to pick it up
kubectl create configmap joulie-simulator-workload-trace \
  -n joulie-system \
  --from-file=trace.jsonl="$DEMO_DIR/trace.jsonl" \
  --dry-run=client -o yaml | kubectl replace -f -

kubectl rollout restart deployment/joulie-telemetry-sim -n joulie-system
kubectl rollout status deployment/joulie-telemetry-sim -n joulie-system --timeout=60s

echo ""
echo "Workload launched! Pods will appear shortly."
echo "Switching to live watch mode..."
sleep 3

echo ""
echo ">>> Step 3: Watch energy state update live (Ctrl-C to stop)"
echo ""
kubectl joulie status -W

pause

echo ""
echo ">>> Step 4: Reset — kill all workloads"
echo ""

# Load empty trace first so simulator stops creating new pods
kubectl create configmap joulie-simulator-workload-trace \
  -n joulie-system \
  --from-literal=trace.jsonl="" \
  --dry-run=client -o yaml | kubectl replace -f -

kubectl rollout restart deployment/joulie-telemetry-sim -n joulie-system
kubectl rollout status deployment/joulie-telemetry-sim -n joulie-system --timeout=60s

# Delete all workload pods and wait for them to be gone
kubectl delete pods -n "$DEMO_NS" --all --grace-period=0 --force 2>/dev/null || true
echo -n "Waiting for pods to terminate..."
while [ "$(kubectl get pods -n "$DEMO_NS" --no-headers 2>/dev/null | wc -l)" -gt 0 ]; do
  echo -n "."
  sleep 1
done
echo " done."

echo "Workloads cleared. Cluster returning to idle."
sleep 3
kubectl joulie status

# Cleanup
kill $GRAFANA_PID 2>/dev/null || true
