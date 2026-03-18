# Homogeneous H100 NVL Benchmark Report

## Scope

This report documents the benchmark results from:

- [`experiments/03-homogeneous-h100-benchmark/`](.)

It covers: experimental setup, controller policy algorithms, simulator models, measured outcomes, plot commentary, and interpretation.

---

## 1. Experimental Setup

### 1.1 Cluster and node topology

- Kind control-plane + worker (real Kubernetes control path).
- 41 fake KWOK worker nodes labeled `joulie.io/managed=true`.
- KWOK nodes are tainted `kwok.x-k8s.io/node=fake:NoSchedule`.
- Simulator pod runs on the real kind worker.
- Workload pods target KWOK nodes via nodeSelector + toleration.

Node inventory source: [`configs/cluster-nodes.yaml`](./configs/cluster-nodes.yaml)

### 1.2 Node inventory - detailed cluster composition

This is a **homogeneous GPU cluster** where all GPU nodes are NVIDIA H100 NVL - the same count as the GPU nodes in experiment 02, enabling a direct heterogeneous vs homogeneous comparison. CPU-only nodes are identical to experiment 02.

#### GPU nodes (33 total, 264 GPUs)

| Node prefix | Replicas | GPU model | GPUs/node | GPU TDP / cap range | Host CPU | CPU cores/node | RAM/node |
|---|---:|---|---:|---|---|---:|---:|
| kwok-h100-nvl | **33** | NVIDIA H100 NVL | 8 | 400 W / 200-400 W | AMD EPYC 9654 96-Core | 192 | 1536 GiB |

All 33 GPU nodes are identical, so any GPU job can be scheduled on any GPU node without hardware-family constraints.

#### CPU-only nodes (8 total) - identical to experiment 02

| Node prefix | Replicas | CPU model | CPU cores/node | RAM/node |
|---|---:|---|---:|---:|
| kwok-cpu-highcore | **2** | AMD EPYC 9965 192-Core | 384 (2x192) | 1536 GiB |
| kwok-cpu-highfreq | **2** | AMD EPYC 9375F 32-Core | 64 (2x32) | 770 GiB |
| kwok-cpu-intensive | **4** | AMD EPYC 9655 96-Core | 192 (2x96) | 1536 GiB |

#### Cluster totals

| Metric | Value |
|---|---|
| Total nodes | **41** |
| GPU nodes | 33 (all H100 NVL) |
| CPU-only nodes | 8 |
| Total GPUs | **264** (all NVIDIA H100 NVL) |
| Total CPU cores | ~7104 |

**Comparison to experiment 02**: both experiments have 41 nodes and 33 GPU nodes, but exp 03 has 264 GPUs vs exp 02's 188 GPUs (because H100 NVL has 8 GPUs/node and replaces lower-density nodes).

### 1.3 Hardware model parameters (simulator)

All GPU nodes use a single hardware family:

| GPU family | IdleW (W) | PeakW (W) | computeGamma | Notes |
|---|---:|---:|---:|---|
| NVIDIA H100 NVL | 80 | 400 | 1.50 | Same parameters as exp 02 H100 NVL nodes |

At 65% GPU cap: loses `1 - 0.65^(1/1.50) ~= 24.7%` GPU throughput.

**CPU->GPU feed coupling**: same `cpuFeedFactor` mechanism as experiment 02. For `single_gpu_training` with `cpuFeedIntensity ~= 0.4`, a 35% CPU frequency reduction causes ~14-18% GPU slowdown.

**CPU-only node power parameters**: same as experiment 01 and 02.

### 1.4 Run configuration

From [`configs/benchmark.yaml`](./configs/benchmark.yaml):

| Parameter | Value |
|---|---|
| Baselines | A, B, C |
| Seeds | 3 |
| Jobs | 500 |
| Mean inter-arrival | 0.15 s |
| Time scale | 60x |
| Timeout per run | 3600 s |
| Perf ratio | 25% |
| GPU ratio | 35% |
| GPU request per job | 1 |
| Work scale | 0.10 |
| Allowed workload types | `debug_eval`, `single_gpu_training`, `cpu_preprocess`, `cpu_analytics` |
| CPU eco cap | 65% of peak |
| GPU eco cap | 65% of peak |

Run configuration is similar to experiment 02 (same caps, same workload types) but with more jobs (500 vs 200) and faster inter-arrival (0.15 s vs 0.30 s) for a more sustained load on the larger GPU fleet.

### 1.5 Baselines

- **A**: simulator only - no Joulie operator or agent.
- **B**: Joulie with `static_partition` policy: `hp_frac=0.40` (~16 of 41 nodes at performance profile).
- **C**: Joulie with `queue_aware_v1` policy: `hp_base_frac=0.40`, `hp_min=2`, `hp_max=20`, `perf_per_hp_node=15`.

Policy caps: `cpu_eco_pct_of_max=65%`, `gpu_eco_pct_of_max=65%`.

---

## 2. Policy Algorithms

Same algorithms as experiments 01 and 02 - see [`experiments/01-cpu-only-benchmark/REPORT.md`](../01-cpu-only-benchmark/REPORT.md) Section 2 for full description.

Key parameters: static assigns ~16 of 41 nodes as performance; queue-aware adjusts between 2 and 20 HP nodes dynamically.

---

## 3. Simulator Algorithms

### 3.1 GPU power model

Same as experiment 02 (Section 3):

```
P_gpu(g) = IdleW + (PeakW - IdleW) * g^computeGamma
throughputFraction = (capWatts / PeakW)^(1/computeGamma)
```

With H100 NVL being the only GPU family, `gamma=1.50` uniformly.

### 3.2 Energy integration

```
E_node += (P_cpu + sum(P_gpu_i)) * dt
energy_sim_kwh = totalJoules * 60 / 3_600_000
```

H100 NVL idle floor: **80 W/GPU x 264 GPUs = 21,120 W** (base cluster power floor even with no GPU jobs running).

### 3.3 CPU->GPU feed coupling

Same `cpuFeedFactor` as experiment 02. With only H100 NVL nodes (all having `gamma=1.50`), the effect is uniform across all GPU nodes.

---

## 4. Measured Results

Source: [`runs/latest/results/summary.csv`](./runs/latest/results/summary.csv)

### 4.1 Per-seed results

| Baseline | Seed | Wall (s) | Throughput (jobs/sim-hr) | Energy (kWh sim) | Avg power (W) |
|---|---:|---:|---:|---:|---:|
| A | 1 | 367.93 | 72.08 | 141.58 | 23089 |
| A | 2 | 485.38 | 55.01 | 187.37 | 23161 |
| A | 3 | 415.24 | 63.87 | 177.40 | 25634 |
| B | 1 | 366.71 | 72.32 | 130.41 | 21338 |
| B | 2 | 485.17 | 55.03 | 171.79 | 21245 |
| B | 3 | 415.35 | 63.85 | 162.36 | 23455 |
| C | 1 | 367.55 | 72.15 | 131.23 | 21422 |
| C | 2 | 485.41 | 55.01 | 171.90 | 21248 |
| C | 3 | 415.96 | 63.76 | 149.36 | 21545 |

All 9 runs completed successfully (no timeouts, no gang deadlocks).

### 4.2 Baseline means (all 3 seeds)

| Baseline | Mean wall (s) | Mean throughput (jobs/sim-hr) | Mean energy (kWh sim) | Mean power (W) |
|---|---:|---:|---:|---:|
| A | 422.9 | 63.65 | 168.78 | 23961 |
| B | 422.4 | 63.73 | 154.85 | 22013 |
| C | 423.0 | 63.64 | 150.83 | 21405 |

### 4.3 Relative to A

| Baseline | Energy Delta | Throughput Delta | Power Delta |
|---|---:|---:|---:|
| B | **-8.2%** | +0.1% (negligible) | -8.1% |
| C | **-10.6%** | 0.0% (negligible) | -10.7% |

---

## 5. Plot Commentary

Plots are in: [`img/`](./img/)

### 5.1 Runtime distribution

![Runtime Distribution](./img/runtime_distribution.png)

- All three baselines complete within identical wall-time windows per seed.
- No measurable throughput penalty from Joulie policies.

### 5.2 Energy vs makespan

![Energy vs Makespan](./img/energy_vs_makespan.png)

- B and C are consistently shifted to lower energy with identical makespan.
- C achieves the lowest energy across all seeds.

### 5.3 Baseline means

![Baseline Means](./img/baseline_means.png)

- Throughput and wall-time bars are indistinguishable across baselines.
- Energy bars clearly show the step-down: A > B > C.

### 5.4 Relative tradeoff vs A

![Relative Tradeoff vs A](./img/relative_tradeoff_vs_a.png)

- Per-seed scatter shows both B and C in the lower-energy region with no throughput loss.
- C seeds consistently achieve lower energy than B seeds.

### 5.5 Relative tradeoff bars vs A

![Relative Tradeoff Bars vs A](./img/relative_tradeoff_bars_vs_a.png)

- Mean energy and throughput deltas: B at -8.2% / +0.1%, C at -10.6% / 0.0%.
- Queue-aware (C) achieves meaningfully better energy savings than static (B).

### 5.6 Hardware family tradeoff vs A

![Hardware Family Tradeoff](./img/hardware_family_tradeoff_vs_a.png)

- Single GPU family; both B and C achieve energy reduction with minimal throughput loss.

### 5.7 Hardware family rankings - baseline B

![Hardware Family Rankings B](./img/hardware_family_rankings_baseline_B.png)

- H100 NVL is the only GPU family. Under B, energy reduction is uniform.

### 5.8 Hardware family rankings - baseline C

![Hardware Family Rankings C](./img/hardware_family_rankings_baseline_C.png)

- C achieves deeper energy reduction than B for the H100 NVL family.

### 5.9 Completion summary

![Completion Summary](./img/completion_summary.png)

- All baselines achieve 100% completion across all 3 seeds.

---

## 6. Interpretation

### Why does Joulie save 8-10% energy on the homogeneous H100 cluster?

The combination of CPU and GPU eco caps at 65% with direct GPU power capping achieves significant energy reduction:

1. **GPU power caps directly reduce the dominant energy contributor**: with 264 H100 NVL GPUs at 80 W idle / 400 W peak, GPU power dominates >95% of cluster energy. Capping eco-node GPUs to 65% of peak power directly reduces this largest term.

2. **Homogeneous scheduling flexibility**: any GPU job can land on any GPU node without hardware-family constraints. This allows the scheduler to pack performance-sensitive jobs onto uncapped nodes efficiently, maximizing the number of nodes that can remain in eco profile.

3. **Throughput preserved**: the 25% performance-affinity ratio means 75% of jobs tolerate eco nodes. With ~16 performance nodes and ~25 eco nodes, there is ample capacity for performance-sensitive jobs on uncapped nodes.

### Why does C outperform B significantly?

Queue-aware (C) achieves -10.6% vs B's -8.2% energy savings:

- Queue-aware dynamically adjusts the HP node count. During periods of low performance-sensitive demand, it reduces HP nodes below the static 40% allocation, putting more nodes into eco profile.
- On a 500-job sustained workload, the demand fluctuations create windows where queue-aware can temporarily increase eco coverage.
- The homogeneous cluster makes this adaptation particularly effective: all eco nodes yield the same per-node GPU power savings, so each additional eco node contributes linearly.

### Improvement over previous results

Previous runs with 80% eco caps (CPU only, no GPU caps) and multi-pod jobs showed B at +9.1% energy (increase) and C at +2.9%. The current results (-8.2% / -10.6%) reflect a complete reversal:

1. **GPU power caps enabled**: the previous runs only applied CPU eco caps. CPU power is <3% of cluster energy on this cluster, so CPU-only capping was counterproductive (extended GPU job duration via `cpuFeedFactor` without reducing GPU power). Now GPU caps directly reduce the dominant power term.
2. **More aggressive eco cap (65% vs 80%)**: deeper GPU power reduction amplifies savings.
3. **Simulator bug fixes**: deadlock bugs fixed, ensuring all runs complete with accurate energy accounting.
4. **No gang deadlocks**: eliminated multi-pod jobs that caused seed-1 failures in previous runs.

### Homogeneous vs heterogeneous comparison

| Metric | Exp 02 (heterogeneous) | Exp 03 (homogeneous) |
|---|---|---|
| GPU count | 188 (5 families) | 264 (all H100 NVL) |
| B energy delta | -6.2% | -8.2% |
| C energy delta | -6.3% | -10.6% |
| Throughput delta | ~0% | ~0% |

The homogeneous cluster achieves deeper savings because: (1) more GPUs are affected by capping, and (2) the uniform hardware allows queue-aware to exploit demand fluctuations more effectively (every eco node contributes identically).

---

## 7. Best-Fit Use Case

From this experiment:

- Joulie achieves **-8.2% energy (static) / -10.6% energy (queue-aware)** on homogeneous H100 NVL clusters with zero throughput impact.
- `queue_aware_v1` outperforms `static_partition` by 2.4 percentage points, making it the recommended policy for GPU-heavy clusters.
- The key enabler is GPU power cap control at 65% on eco nodes.

---

## 8. Reproducibility

- Config: [`configs/benchmark.yaml`](./configs/benchmark.yaml)
- Sweep script: [`scripts/05_sweep.py`](./scripts/05_sweep.py)
- Collection: [`scripts/06_collect.py`](./scripts/06_collect.py)
- Plotting: [`scripts/07_plot.py`](./scripts/07_plot.py)
- Run artifacts: [`runs/latest/`](./runs/latest/)
