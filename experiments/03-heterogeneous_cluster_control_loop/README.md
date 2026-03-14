# Experiment 03: Heterogeneous Cluster Control Loop

This experiment benchmarks the full Joulie architecture on a heterogeneous
cluster, comparing three scenarios with increasing levels of power management.

## Execution modes

### 1. Cluster-based benchmark (primary)

Runs against a KWOK simulated cluster. This is the primary benchmark mode
and produces the most realistic results.

```bash
# Check prerequisites
bash scripts/00_prereqs_check.sh

# Set up KWOK cluster with 8 heterogeneous nodes
bash scripts/10_setup_cluster.sh

# Run all scenarios (A-C), collect results, generate plots
bash scripts/20_run_scenarios.sh
```

### 2. Fast simulation (alternative)

Standalone Go simulation that does not require a cluster. Useful for quick
iteration on scoring logic and scenario parameters.

```bash
go run ./experiments/03-heterogeneous_cluster_control_loop/
```

Or run the unit tests:

```bash
go test ./experiments/03-heterogeneous_cluster_control_loop/
```

## Prerequisites

- **kwokctl** -- KWOK cluster management
- **kubectl** -- Kubernetes CLI
- **Go >= 1.22** -- for fast simulation mode
- **Python 3** with PyYAML, matplotlib, pandas -- for workload generation and plotting

Install Python dependencies:

```bash
pip install -r experiments/03-heterogeneous_cluster_control_loop/requirements.txt
```

## Scenarios

| Scenario | Caps (65%) | Scheduler Steering | Description |
|----------|------------|--------------------|-------------|
| A | No | No | Baseline -- no Joulie components |
| B | Yes | No | Operator applies CPU/GPU caps; no scheduling awareness |
| C | Yes | Yes | Caps + scheduler extender for workload-class-aware placement |

## Workload placement

Pod placement is controlled via the `joulie.io/workload-class` annotation on pods
(not node affinity). The scheduler extender reads this annotation to make placement
decisions:

- **performance** workloads: should avoid eco (capped) nodes
- **standard** workloads: prefer performance nodes, tolerate eco
- **best-effort** workloads: prefer eco nodes (lower power, acceptable slowdown)

## Cluster topology

8 nodes defined in `configs/cluster-nodes.yaml`:

- 4 GPU nodes (H100/A100-class, 4-8 GPUs each)
- 4 CPU-only nodes (64-192 cores)

In scenarios B-C, half the nodes are set to eco profile (65% CPU/GPU power caps).

## Benchmark configuration

See `configs/benchmark.yaml` for tunable parameters:

- Job count, settle time, timeout
- Workload class ratios (performance/standard/best-effort)
- GPU workload ratio
- Eco cap percentages and node fraction

## Output

Results are written to `results/` with:

- `scenario_<X>/` -- per-scenario cluster snapshots (nodes, pods, CRs)
- `scenario_<X>_metrics.json` -- aggregate metrics per scenario (simulation mode)
- `summary.csv` -- cross-scenario comparison table
- `comparison.json` -- detailed comparison (simulation mode)
- `plots/` -- generated charts:
  - `energy_vs_makespan.png` -- energy and makespan bar chart
  - `cooling_stress.png` -- peak/avg cooling and PSU stress
  - `edp_comparison.png` -- Energy-Delay Product comparison
  - `relative_improvement.png` -- relative improvement vs baseline A

## Workload mix (simulation mode)

| Type | Fraction | Bound | Class | Reschedulable |
|------|----------|-------|-------|---------------|
| GPU compute (LLM training) | 30% | compute | standard | yes |
| GPU memory (inference) | 20% | memory | performance | no |
| CPU compute (data prep) | 25% | compute | standard | yes |
| Best-effort low-util | 15% | mixed | best-effort | yes |
| Short checkpointable | 10% | mixed | standard | yes |

## Scripts

| Script | Purpose |
|--------|---------|
| `00_prereqs_check.sh` | Verify required tools are installed |
| `10_setup_cluster.sh` | Create KWOK cluster and apply CRDs + nodes |
| `15_install_components.sh` | Install/uninstall Joulie components per scenario |
| `20_run_scenarios.sh` | Main benchmark driver: runs A-C, collects, plots |
| `25_generate_workloads.py` | Generate pod manifests with workload-class annotations |
| `30_collect.py` | Aggregate per-scenario results into summary.csv |
| `40_plot.py` | Generate comparison plots from summary.csv |
| `generate_kwok_nodes.py` | Generate KWOK Node manifests from cluster config |
