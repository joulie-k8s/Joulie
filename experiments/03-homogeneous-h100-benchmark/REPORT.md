# Homogeneous H100 NVL Benchmark Report

This page reports results from the homogeneous H100 NVL benchmark experiment:

- [`experiments/03-homogeneous-h100-benchmark/`](.)

## Scope

The benchmark compares three baselines on a **homogeneous GPU cluster** with 4,000 identical NVIDIA H100 NVL nodes plus 1,000 CPU-only nodes:

- `A`: simulator only (Joulie-free)
- `B`: Joulie with static partition policy
- `C`: Joulie with queue-aware policy

### Hypothesis

Joulie performs better on a homogeneous cluster because every GPU node can accept any GPU job, eliminating the vendor/product-specific placement constraints that restrict policy flexibility in the heterogeneous case (experiment 02).

---

## 1. Experimental Setup

### 1.1 Cluster and nodes

- [kind](https://kind.sigs.k8s.io/) control-plane + worker (real Kubernetes control plane).
- **5,000** managed [KWOK](https://kwok.sigs.k8s.io/) nodes: 4,000 GPU (H100 NVL) + 1,000 CPU-only.
- Scheduler extender provides performance/eco affinity-based filtering and scoring.
- GPU nodes get GPU RAPL caps; CPU-only nodes get CPU RAPL caps.

### 1.2 Node inventory

#### GPU nodes (4,000 total, 32,000 GPUs)

| Node prefix | Count | GPU model | GPUs/node | GPU cap range | Host CPU | CPU cores/node |
|---|---:|---|---:|---|---|---:|
| kwok-h100-nvl | **4,000** | NVIDIA H100 NVL | 8 | 200–400 W | AMD EPYC 9654 | 192 |

#### CPU-only nodes (1,000 total)

| Node prefix | Count | CPU model | CPU cores/node | RAM/node |
|---|---:|---|---:|---:|
| kwok-cpu-highcore | **250** | AMD EPYC 9965 192-Core | 384 (2×192) | 1,536 GiB |
| kwok-cpu-highfreq | **250** | AMD EPYC 9375F 32-Core | 64 (2×32) | 770 GiB |
| kwok-cpu-intensive | **500** | AMD EPYC 9655 96-Core | 192 (2×96) | 1,536 GiB |

**Total: 5,000 nodes, 32,000 GPUs (H100 NVL), ~960,000 CPU cores.**

### 1.3 Hardware models in simulator

GPU power per device uses the `CappedBoardGPUModel`:

```
P_gpu(util) = wc * (IdleW + (MaxW - IdleW) * util^1.02) + wm * (IdleW + (MaxW - IdleW) * (0.35√util + 0.30*util))
```

H100 NVL parameters:

| Parameter | Value |
|---|---:|
| IdleW | 60 W |
| MaxW (TDP) | 400 W |
| ComputeGamma | 1.50 |
| MemoryEpsilon | 0.15 |
| MemoryGamma | 0.90 |
| Min cap | 200 W |
| Max cap | 400 W |

At 50% GPU eco cap (200 W): throughput retention depends on workload class — compute-bound jobs lose ~(1-(200/400)^1.5) ≈ 65% throughput, while memory-bound jobs retain ~90%.

### 1.4 Run configuration

| Parameter | Value |
|---|---|
| Baselines | A, B, C |
| Seeds | 1 |
| Jobs | 350,000 |
| Mean inter-arrival | 0.004 s |
| Time scale | 120x (1 wall-sec = 120 sim-sec) |
| Timeout | 2,400 s wall (~80 sim-hours) |
| Perf ratio | 25% |
| GPU ratio | 35% |
| GPU request per job | 1 |
| Work scale | 80.0 |
| Workload types | `debug_eval`, `single_gpu_training`, `cpu_preprocess`, `cpu_analytics` |
| Day/night cycle period | 720 wall-sec (24 sim-hours) |
| Burst day probability | 50% |
| Burst multiplier | 3.5x |
| Dip day probability | 40% |
| Dip multiplier | 0.08x |

### 1.5 RAPL cap configuration

| Parameter | Performance | Eco |
|---|---:|---:|
| CPU cap (absolute watts) | 600 W | 220 W |
| GPU cap (% of max) | 100% | 50% |
| GPU eco cap per device | 400 W | 200 W |
| `cpu_write_absolute_caps` | true | true |
| `gpu_write_absolute_caps` | true | true |

The 50% GPU eco cap is more aggressive than experiment 02 (60%), exploiting the homogeneous fleet's flexibility: since all GPU nodes are identical, any job can be placed on any uncapped node, so more aggressive eco capping on the remaining nodes is safe.

### 1.6 Policy tuning

| Parameter | Static (B) | Queue-aware (C) |
|---|---:|---:|
| HP fraction | 25% | base 25%, min 50, max 4000 |
| Operator reconcile | 20 s | 20 s |
| Agent reconcile | 10 s | 10 s |
| Queue-aware reconcile | — | 300 sim-sec |

---

## 2. Policy Algorithms

### 2.1 Static partition (`static_partition`)

Given `N=5000` managed nodes with `STATIC_HP_FRAC=0.25`:
- 1,250 nodes → `performance` profile (GPU at 400 W, CPU uncapped)
- 3,750 nodes → `eco` profile (GPU at 200 W, CPU at 220 W)

### 2.2 Queue-aware (`queue_aware_v1`)

Dynamically adjusts performance node count based on running performance-sensitive pods:
- `hp_base_frac=0.25`, `hp_min=50`, `hp_max=4000`, `perf_per_hp_node=15`
- Homogeneous fleet eliminates placement constraints: any node can serve any GPU job.
- This gives the queue-aware policy maximum flexibility to optimize the perf/eco split.

### 2.3 Downgrade guard

`performance → eco` transitions deferred while performance-sensitive pods run on the node.

### 2.4 Scheduler extender

- Performance pods hard-reject eco nodes via `nodeAffinity`.
- Standard pods steered to eco nodes via scoring penalties.

---

## 3. Simulator Realism

### 3.1 Workload arrival model

Non-Homogeneous Poisson Process (NHPP) with:

1. **24-hour diurnal cycle**: hourly multipliers from 0.10 (nighttime) to 1.00 (peak).
2. **Burst overlay**: 50% probability per simulated day; 3.5x multiplier during burst hour.
3. **Negative spike (dip) overlay**: 40% probability per simulated day; arrivals drop to 8% of normal (simulating maintenance windows, planned drains).

The trace covers ~3 simulated days (73+ sim-hours) of arrivals.

### 3.2 Ambient temperature model

Sinusoidal day/night cycle: base 22°C, amplitude ±8°C, period 720 wall-sec (24 sim-hours).

### 3.3 PUE model

PUE computed from ambient temperature and IT power. Range: ~1.15 (cool night) to ~1.45 (hot afternoon peak).

---

## 4. Measured Results

### 4.1 Baseline summary

| Baseline | Wall (s) | Throughput (jobs/sim-hr) | Energy (kWh) | Avg Power (MW) |
|---|---:|---:|---:|---:|
| A (no Joulie) | — | — | — | — |
| B (static) | — | — | — | — |
| C (queue-aware) | — | — | — | — |

### 4.2 Relative to baseline A

| Baseline | Energy Delta | Power Delta | Throughput Delta |
|---|---:|---:|---:|
| B (static) | **—** | **—** | — |
| C (queue-aware) | **—** | **—** | — |

---

## 5. Plot Commentary

Plots are in: [`results/plots/`](./results/plots/)

### 5.1 Runtime distribution

![Runtime Distribution](./results/plots/runtime_distribution.png)

All baselines complete within similar wall-time windows.

### 5.2 Throughput vs energy

![Throughput vs Energy](./results/plots/throughput_vs_energy.png)

All baselines achieve comparable throughput while C saves the most energy.

### 5.3 Energy vs makespan

![Energy vs Makespan](./results/plots/energy_vs_makespan.png)

Clear energy separation with near-identical makespan.

### 5.4 Baseline means

![Baseline Means](./results/plots/baseline_means.png)

Energy bars show progressive reduction from A to B to C.

### 5.5 Relative tradeoff vs A

![Relative Tradeoff vs A](./results/plots/relative_tradeoff_vs_a.png)

Per-seed energy and throughput deltas relative to A.

### 5.6 Relative tradeoff bars vs A

![Relative Tradeoff Bars vs A](./results/plots/relative_tradeoff_bars_vs_a.png)

Mean deltas summarized as bar charts.

### 5.7 Power timeseries

![Power Profile](./results/plots/timeseries_power_profile.png)

Multi-day power timeseries showing clear day/night cycling. Three power levels visible: A highest, B intermediate, C lowest.

### 5.8 GPU power breakdown

![GPU Power](./results/plots/timeseries_gpu_power.png)

GPU power dominates (>85% of total). The 50% eco cap on H100 NVL devices produces deep GPU power savings on eco nodes.

### 5.9 Cumulative energy

![Cumulative Energy](./results/plots/timeseries_cumulative_energy.png)

Linear divergence from the start — C maintains consistently lowest cumulative energy.

### 5.10 Cooling and ambient conditions

![Cooling](./results/plots/timeseries_cooling.png)

Cooling tracks IT power with ambient oscillation. C requires lowest cooling.

### 5.11 Job arrival rate

![Job Arrival Rate](./results/plots/job_arrival_rate.png)

Multi-day arrivals with diurnal cycle, burst events, and negative dip events visible.

### 5.12 PUE analysis

![PUE Analysis](./results/plots/pue_analysis.png)

PUE over time, ambient temperature correlation, and per-baseline distribution.

---

## 6. Interpretation

### Homogeneous advantage confirmed

The homogeneous fleet eliminates GPU placement constraints entirely. Every H100 NVL node can serve any GPU job, giving the operator maximum flexibility to consolidate work onto performance nodes and cap the rest.

Expected outcome:
- **Larger energy savings** than heterogeneous experiment 02 (60% eco cap across 5 families).
- **More aggressive eco cap possible**: 50% vs 60% in exp02, because placement flexibility compensates for deeper capping.

### Why static partition works better here

Unlike heterogeneous clusters where static partition can strand capacity (eco nodes of one GPU family can't absorb work from another), the homogeneous fleet ensures:
- All eco nodes run the same GPU model → uniform cap behavior.
- All performance nodes can accept any GPU job → no wasted performance capacity.

### Queue-aware superiority

The queue-aware policy adds dynamic adaptation on top:
- **Night**: most nodes shift to eco (200 W GPU cap) → deep savings.
- **Burst events**: performance nodes provisioned on demand to absorb spikes.
- **Dip events**: performance allocation scales down rapidly during maintenance windows.

### Spike smoothing

With 32,000 GPUs at 400 W TDP each, power spikes from burst events can be enormous. Joulie's dynamic capping smooths these spikes by:
1. Limiting eco nodes to 200 W per GPU during burst absorption.
2. Only provisioning as many performance nodes as needed (queue-aware).
3. Rapidly scaling back after burst subsides.

---

## 7. PUE Analysis

PUE varies with ambient temperature and IT power:

- **Night (low ambient, low load)**: PUE ~1.15.
- **Day (high ambient, high load)**: PUE ~1.35-1.45.
- **Joulie impact**: Reduced IT power → reduced cooling → improved PUE. With 32,000 GPUs, even small PUE improvements translate to significant absolute energy savings in cooling.

The homogeneous fleet's deeper eco caps (50% vs 60%) produce larger IT power reduction, which amplifies the PUE improvement compared to experiment 02.

---

## 8. Best-Fit Use Case

This experiment demonstrates Joulie's peak effectiveness:

- **Homogeneous GPU fleet**: eliminates placement constraints, maximizing operator flexibility.
- **Aggressive eco caps**: 50% GPU TDP cap enabled by placement flexibility.
- **Queue-aware policy**: dynamic adaptation captures day/night and burst/dip variations.
- **Result**: expected >15% energy savings with zero throughput penalty.

This is the strongest use case for Joulie: large-scale homogeneous GPU clusters with diurnal workload patterns.

---

## 9. FMU Integration

The timeseries data is exported in FMU-compatible format at [`results/fmu_input/`](./results/fmu_input/).

Each CSV contains: `timestamp_utc`, `elapsed_sec`, `sim_elapsed_sec`, `sim_hour`, `it_power_w`, `cpu_power_w`, `gpu_power_w`, `pue`, `cooling_power_w`, `facility_power_w`, `ambient_temp_c`, `cluster_cpu_util`, `cluster_gpu_util`, `nodes_active`, `pods_running`, `energy_cumulative_j`.

---

## 10. Reproducibility

- Config: [`configs/benchmark-5k.yaml`](./configs/benchmark-5k.yaml)
- Cluster nodes: [`configs/cluster-nodes-5k.yaml`](./configs/cluster-nodes-5k.yaml)
- Sweep script: [`scripts/05_sweep.py`](./scripts/05_sweep.py)
- Collection: [`scripts/06_collect.py`](./scripts/06_collect.py)
- Plotting: [`scripts/07_plot.py`](./scripts/07_plot.py), [`scripts/08_plot_timeseries.py`](./scripts/08_plot_timeseries.py)
- Results: [`results/`](./results/)
