#!/usr/bin/env bash
# Run all 3 scenarios (A-C) against a live KWOK cluster.
#
# For each scenario:
#   1. Reset state (delete NodePowerProfiles, reset labels)
#   2. Install/uninstall Joulie components via 15_install_components.sh
#   3. Apply scenario-specific NodePowerProfiles and eco labels
#   4. Deploy test workloads with joulie.io/workload-class annotations
#   5. Wait for convergence
#   6. Collect snapshots: NodeTwinState, NodeHardware, NodePowerProfile, nodes, pods
#   7. Write per-scenario metrics JSON to results/
#
# Fast simulation alternative (no cluster needed):
#   go run ./experiments/04-heterogeneous_cluster_control_loop/
set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-joulie-control-loop-exp04}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
CONFIGS_DIR="$SCRIPT_DIR/../configs"
RESULTS_DIR="${RESULTS_DIR:-$SCRIPT_DIR/../results}"
BENCHMARK_CFG="${BENCHMARK_CFG:-$CONFIGS_DIR/benchmark.yaml}"
SETTLE_SECONDS="${SETTLE_SECONDS:-30}"

export KUBECONFIG="${KUBECONFIG:-$(kwokctl get kubeconfig-path --name "$CLUSTER_NAME")}"

mkdir -p "$RESULTS_DIR"

# Read benchmark config values if available
if command -v python3 &>/dev/null && [ -f "$BENCHMARK_CFG" ]; then
  ECO_CAP_CPU_PCT=$(python3 -c "
import yaml, sys
with open('$BENCHMARK_CFG') as f: c=yaml.safe_load(f)
print(c.get('policy',{}).get('eco_cap_cpu_pct', 65))
")
  ECO_CAP_GPU_PCT=$(python3 -c "
import yaml, sys
with open('$BENCHMARK_CFG') as f: c=yaml.safe_load(f)
print(c.get('policy',{}).get('eco_cap_gpu_pct', 65))
")
  ECO_NODE_FRACTION=$(python3 -c "
import yaml, sys
with open('$BENCHMARK_CFG') as f: c=yaml.safe_load(f)
print(c.get('policy',{}).get('eco_node_fraction', 0.50))
")
  SETTLE_SECONDS=$(python3 -c "
import yaml, sys
with open('$BENCHMARK_CFG') as f: c=yaml.safe_load(f)
print(c.get('run',{}).get('settle_seconds', 30))
")
else
  ECO_CAP_CPU_PCT=65
  ECO_CAP_GPU_PCT=65
  ECO_NODE_FRACTION=0.50
fi

echo "=== Experiment 04: Running scenarios ==="
echo "  Benchmark config: $BENCHMARK_CFG"
echo "  Results dir: $RESULTS_DIR"
echo "  Settle seconds: $SETTLE_SECONDS"
echo "  Eco CPU cap: ${ECO_CAP_CPU_PCT}%  GPU cap: ${ECO_CAP_GPU_PCT}%"
echo ""

# Get all managed node names
ALL_NODES=$(kubectl get nodes -l joulie.io/managed=true -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || \
            kubectl get nodes -o jsonpath='{.items[*].metadata.name}')
NODE_COUNT=$(echo "$ALL_NODES" | wc -w)
ECO_COUNT=$(python3 -c "import math; print(int(math.ceil($NODE_COUNT * $ECO_NODE_FRACTION)))")

reset_state() {
  echo "  Resetting cluster state..."
  # Delete NodePowerProfiles
  kubectl delete nodepowerprofile --all --ignore-not-found 2>/dev/null || true
  # Delete NodeTwinStates
  kubectl delete nodetwinstate --all --ignore-not-found 2>/dev/null || true
  # Reset power-profile labels to performance
  for NODE in $ALL_NODES; do
    kubectl label node "$NODE" joulie.io/power-profile=performance --overwrite 2>/dev/null || true
  done
  # Delete previous workload pods
  kubectl delete pods -l app.kubernetes.io/part-of=joulie-exp04-benchmark \
    --ignore-not-found --wait=false 2>/dev/null || true
}

apply_eco_profiles() {
  local cap_cpu="$1"
  local cap_gpu="$2"
  local eco_count="$3"

  echo "  Applying eco profiles (CPU ${cap_cpu}%, GPU ${cap_gpu}%) to ${eco_count}/${NODE_COUNT} nodes..."
  local i=0
  for NODE in $ALL_NODES; do
    i=$((i + 1))
    if [ "$i" -le "$eco_count" ]; then
      # Eco node
      kubectl apply -f - <<EOF
apiVersion: joulie.io/v1alpha1
kind: NodePowerProfile
metadata:
  name: $NODE
spec:
  nodeName: $NODE
  profile: eco
  cpu:
    packagePowerCapPctOfMax: $cap_cpu
  gpu:
    powerCap:
      scope: perGpu
      capPctOfMax: $cap_gpu
EOF
      kubectl label node "$NODE" joulie.io/power-profile=eco --overwrite
    else
      # Performance node (no cap)
      kubectl label node "$NODE" joulie.io/power-profile=performance --overwrite
    fi
  done
}

deploy_workloads() {
  echo "  Generating and deploying workload pods..."
  python3 "$SCRIPT_DIR/25_generate_workloads.py" \
    --config "$BENCHMARK_CFG" \
    | kubectl apply -f -
}

wait_for_convergence() {
  local seconds="$1"
  echo "  Waiting ${seconds}s for operator convergence..."
  sleep "$seconds"
}

collect_snapshot() {
  local scenario_dir="$1"
  echo "  Collecting cluster snapshots..."

  kubectl get nodetwinstate -o yaml > "$scenario_dir/nodetwinstates.yaml" 2>/dev/null || true
  kubectl get nodehardware -o yaml > "$scenario_dir/nodehardwares.yaml" 2>/dev/null || true
  kubectl get nodepowerprofile -o yaml > "$scenario_dir/nodepowerprofiles.yaml" 2>/dev/null || true
  kubectl get nodes -o json > "$scenario_dir/nodes.json"
  kubectl get pods -A -o json > "$scenario_dir/pods.json"

  # Collect per-scenario metrics as JSON
  python3 -c "
import json, subprocess, sys

def kubectl_json(args):
    r = subprocess.run(['kubectl'] + args, capture_output=True, text=True)
    if r.returncode != 0:
        return {}
    try:
        return json.loads(r.stdout)
    except json.JSONDecodeError:
        return {}

nodes = kubectl_json(['get', 'nodes', '-o', 'json'])
pods = kubectl_json(['get', 'pods', '-A', '-l', 'app.kubernetes.io/part-of=joulie-exp04-benchmark', '-o', 'json'])

node_items = nodes.get('items', [])
pod_items = pods.get('items', [])

eco_nodes = sum(1 for n in node_items
    if n.get('metadata',{}).get('labels',{}).get('joulie.io/power-profile') == 'eco')

pending = sum(1 for p in pod_items if p.get('status',{}).get('phase') == 'Pending')
running = sum(1 for p in pod_items if p.get('status',{}).get('phase') == 'Running')
succeeded = sum(1 for p in pod_items if p.get('status',{}).get('phase') == 'Succeeded')

metrics = {
    'total_nodes': len(node_items),
    'eco_nodes': eco_nodes,
    'performance_nodes': len(node_items) - eco_nodes,
    'total_pods': len(pod_items),
    'pending_pods': pending,
    'running_pods': running,
    'succeeded_pods': succeeded,
}

json.dump(metrics, sys.stdout, indent=2)
print()
" > "$scenario_dir/metrics.json"
}

for SCENARIO in A B C; do
  echo ""
  echo "--- Scenario $SCENARIO ---"
  SCENARIO_DIR="$RESULTS_DIR/scenario_$SCENARIO"
  mkdir -p "$SCENARIO_DIR"
  START_TIME=$(date +%s)

  # 1. Reset state
  reset_state

  # 2. Install components for this scenario
  bash "$SCRIPT_DIR/15_install_components.sh" "$SCENARIO"

  # 3. Apply scenario-specific configuration
  case "$SCENARIO" in
    A)
      echo "  Scenario A: No Joulie - baseline (no NodePowerProfiles, no scheduler extender)"
      ;;
    B)
      echo "  Scenario B: Caps only - eco profiles on ${ECO_COUNT} nodes, no scheduler"
      apply_eco_profiles "$ECO_CAP_CPU_PCT" "$ECO_CAP_GPU_PCT" "$ECO_COUNT"
      ;;
    C)
      echo "  Scenario C: Caps + Scheduler - eco profiles + scheduler extender"
      apply_eco_profiles "$ECO_CAP_CPU_PCT" "$ECO_CAP_GPU_PCT" "$ECO_COUNT"
      ;;
  esac

  # 4. Deploy test workloads
  deploy_workloads

  # 5. Wait for convergence
  wait_for_convergence "$SETTLE_SECONDS"

  # 6. Collect snapshots
  collect_snapshot "$SCENARIO_DIR"

  END_TIME=$(date +%s)
  ELAPSED=$((END_TIME - START_TIME))
  echo "  Scenario $SCENARIO completed in ${ELAPSED}s. Artifacts: $SCENARIO_DIR"
done

echo ""
echo "=== All scenarios complete. Artifacts in: $RESULTS_DIR ==="
echo ""

# Post-processing: collect and plot
echo "Running collection..."
python3 "$SCRIPT_DIR/30_collect.py" "$RESULTS_DIR"

echo "Running plots..."
python3 "$SCRIPT_DIR/40_plot.py" "$RESULTS_DIR"

echo ""
echo "=== Benchmark complete ==="
echo "  Results: $RESULTS_DIR"
echo "  Summary: $RESULTS_DIR/summary.csv"
echo "  Plots:   $RESULTS_DIR/plots/"
echo ""
echo "Fast simulation alternative (no cluster):"
echo "  cd $REPO_ROOT && go run ./experiments/04-heterogeneous_cluster_control_loop/"
