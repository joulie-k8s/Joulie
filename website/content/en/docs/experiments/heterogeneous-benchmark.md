---
title: "Heterogeneous Cluster Benchmark"
weight: 20
---

The heterogeneous cluster benchmark exercises all three Joulie control scenarios in a single experiment run using a mixed workload on a simulated heterogeneous GPU + CPU cluster.

It is the successor to the KWOK benchmark and adds the scheduler extender to the comparison matrix.

## Goal

Demonstrate and measure the incremental effect of each Joulie control layer:

| Scenario | Control layers active |
|---|---|
| A - Baseline | None (Joulie-free) |
| B - Caps only | Operator power caps (CPU + GPU) |
| C - Caps + Scheduler | Operator caps + scheduler extender |

Each scenario runs the same workload mix on the same simulated cluster.
The difference in energy, makespan, and per-workload-class completion time isolates the contribution of each layer.

## Workload mix

The benchmark injects four workload classes simultaneously:

| Class | Profile annotation | GPU usage | CPU usage | Expected scenario effect |
|---|---|---|---|---|
| GPU compute-bound | `performance`, `gpu-sensitivity: high` | High GPU utilization | Low | Scenario C steers away from capped GPU nodes |
| GPU memory-bound | `standard`, `gpu-sensitivity: medium` | High GPU memory BW | Medium | Neutral |
| CPU compute-bound | `performance`, `cpu-sensitivity: high` | None | High CPU utilization | Scenario C steers to performance nodes |
| Best-effort | `best-effort` | Low | Low | All scenarios may concentrate on eco nodes |

Mix proportions (default):

- GPU compute-bound: 25%
- GPU memory-bound: 20%
- CPU compute-bound: 25%
- Best-effort: 30%

## Simulated cluster topology

The benchmark uses a heterogeneous node set to surface placement decisions:

| Node class | Count | CPU model | GPU model | Default profile |
|---|---|---|---|---|
| High-density GPU | 2 | AMD EPYC 9654 (2S) | NVIDIA H100 NVL x4 | performance |
| Mid-range GPU | 2 | Intel Xeon 8480+ (2S) | NVIDIA A100 x2 | eco |
| CPU-only | 2 | AMD EPYC 9654 (1S) | None | performance |
| Lightweight | 2 | Intel Xeon 6448Y (1S) | None | eco |

Hardware identity is established via node labels and resolved through the Joulie inventory, consistent with the operator's heterogeneous planning path.

## Metrics collected

For each scenario run:

| Metric | Source | What it measures |
|---|---|---|
| Total simulated energy (kWh) | `simulator/debug/energy` | Aggregate cluster energy |
| Mean cluster power (W) | Prometheus `joulie_sim_node_power_watts` | Time-averaged power draw |
| Makespan (s) | Benchmark script wall time | Time to complete all submitted jobs |
| Per-class completion time (s) | Workload trace completion records | How each class is affected |
| Scheduling filter decisions | Extender metrics | How often nodes were rejected per class |
| GPU cap headroom (W) | `joulie_sim_gpu_cap_headroom_watts` | Spare GPU power budget |
| PSU stress (%) | `joulie_sim_psu_stress_pct` | Facility PSU load |
| Cooling stress (%) | `joulie_sim_cooling_stress_pct` | Facility cooling load |

## How to run

### Prerequisites

- [kind](https://kind.sigs.k8s.io/) and [KWOK](https://kwok.sigs.k8s.io/) installed.
- Joulie operator, agent, simulator, and scheduler extender images built or pulled.
- `kubectl` configured for the target cluster.

### Setup

```bash
# Create kind cluster with KWOK fake nodes
experiments/02-heterogeneous-benchmark/scripts/10_setup_cluster.sh

# Verify nodes are ready
kubectl get nodes -l joulie.io/managed=true
```

### Run all three scenarios

```bash
# Activate the experiment virtualenv
source experiments/02-heterogeneous-benchmark/.venv/bin/activate

# Run the full sweep (scenarios A-C, 3 seeds each)
experiments/02-heterogeneous-benchmark/scripts/20_run_sweep.sh
```

Each scenario is isolated by:

1. deploying the corresponding Joulie component set (operator only / operator + extender),
2. injecting the same seeded workload trace,
3. collecting metrics until all jobs complete or timeout is reached,
4. tearing down and resetting cluster state between scenarios.

### Collect and plot results

```bash
experiments/02-heterogeneous-benchmark/scripts/30_collect.py
experiments/02-heterogeneous-benchmark/scripts/40_plot.py
```

Output files land in `experiments/02-heterogeneous-benchmark/results/`.

### Configuration knobs

The sweep script reads a YAML config file:

```yaml
# experiments/02-heterogeneous-benchmark/config.yaml
seeds: [1, 2, 3]
jobsPerSeed: 400
meanInterArrivalSeconds: 0.15
timeoutSeconds: 2400
timeScale: 60
workloadMix:
  gpuComputeBound: 0.25
  gpuMemoryBound: 0.20
  cpuComputeBound: 0.25
  bestEffort: 0.30
```

## What to expect from results

**Scenario A vs B (caps only):**
- Energy reduction of 5-10% with negligible makespan change for best-effort and batch jobs.
- GPU compute-bound jobs may show mild slowdown under GPU cap; CPU compute-bound jobs may show mild slowdown under CPU cap.

**Scenario B vs C (add scheduler extender):**
- GPU compute-bound and CPU compute-bound performance jobs complete faster because the extender steers them to uncapped, high-headroom nodes.
- Best-effort jobs concentrate on eco nodes; energy per best-effort job may decrease slightly.
- Scheduling filter decisions visible in extender metrics: a non-trivial fraction of eco nodes are rejected for performance pods.

Energy and timing results reflect simulator model fidelity, not real power measurements.

## Implementation details

Manifests, scripts, and trace generation tools:

- Experiment folder: `experiments/02-heterogeneous-benchmark/`
- Simulator configuration: `experiments/02-heterogeneous-benchmark/sim-node-classes.yaml`
- Workload trace generator: `simulator/cmd/workloadgen` with `--profile heterogeneous-mix`
- Full report template: `experiments/02-heterogeneous-benchmark/REPORT.md`

## Related pages

- [KWOK Benchmark Experiment]({{< relref "/docs/experiments/kwok-benchmark.md" >}})
- [Scheduler Extender]({{< relref "/docs/architecture/scheduler.md" >}})
- [Workload and Power Simulator]({{< relref "/docs/simulator/simulator.md" >}})
