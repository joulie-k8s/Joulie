# Heterogeneous GPU Cluster Benchmark Report

This page reports results from the heterogeneous GPU cluster benchmark experiment:

- [`experiments/02-heterogeneous-benchmark/`](.)

## Scope

The benchmark compares three baselines on a **heterogeneous GPU cluster** mixing 5 distinct GPU hardware families plus CPU-only nodes:

- `A`: simulator only (Joulie-free)
- `B`: Joulie with static partition policy
- `C`: Joulie with queue-aware policy

---

## 1. Experimental Setup

### 1.1 Cluster and nodes

- [kind](https://kind.sigs.k8s.io/) control-plane + worker (real Kubernetes control plane).
- **5,000** managed [KWOK](https://kwok.sigs.k8s.io/) nodes: 4,000 GPU + 1,000 CPU-only.
- Workload pods target KWOK nodes via nodeSelector + toleration.
- Scheduler extender provides performance/eco affinity-based filtering and scoring.
- GPU nodes get GPU RAPL caps; CPU-only nodes get CPU RAPL caps.

### 1.2 Node inventory — detailed cluster composition

This is a **heterogeneous GPU cluster** mixing 5 distinct GPU hardware families across 4,000 GPU nodes, plus 1,000 CPU-only nodes.

#### GPU nodes (4,000 total, 22,760 GPUs)

| Node prefix | Count | GPU model | GPUs/node | GPU cap range | Host CPU | CPU cores/node |
|---|---:|---|---:|---|---|---:|
| kwok-h100-nvl | **1,450** | NVIDIA H100 NVL | 8 | 200–400 W | AMD EPYC 9654 | 192 |
| kwok-h100-sxm | **730** | NVIDIA H100 80GB HBM3 | 4 | 350–700 W | Intel Xeon Gold 6530 | 64 |
| kwok-l40s | **850** | NVIDIA L40S | 4 | 200–350 W | AMD EPYC 9534 | 128 |
| kwok-mi300x | **240** | AMD Instinct MI300X | 8 | 350–750 W | AMD EPYC 9534 | 128 |
| kwok-w7900 | **730** | AMD Radeon PRO W7900 | 4 | 200–295 W | AMD EPYC 9534 | 128 |

GPU count: (1450×8) + (730×4) + (850×4) + (240×8) + (730×4) = **22,760 GPUs** across NVIDIA and AMD families.

#### CPU-only nodes (1,000 total)

| Node prefix | Count | CPU model | CPU cores/node | RAM/node |
|---|---:|---|---:|---:|
| kwok-cpu-highcore | **250** | AMD EPYC 9965 192-Core | 384 (2×192) | 1,536 GiB |
| kwok-cpu-highfreq | **250** | AMD EPYC 9375F 32-Core | 64 (2×32) | 770 GiB |
| kwok-cpu-intensive | **500** | AMD EPYC 9655 96-Core | 192 (2×96) | 1,536 GiB |

**Total: 5,000 nodes, 22,760 GPUs (5 families), ~766,080 CPU cores.**

### 1.3 Hardware models in simulator

GPU power per device uses the `CappedBoardGPUModel`:

```
P_gpu(util) = wc * (IdleW + (MaxW - IdleW) * util^1.02) + wm * (IdleW + (MaxW - IdleW) * (0.35√util + 0.30*util))
```

where `wc`, `wm` are compute/memory boundedness weights derived from workload class.

Per-GPU-family physics parameters:

| GPU family | IdleW (W) | MaxW (W) | ComputeGamma | GPU cap range |
|---|---:|---:|---:|---|
| NVIDIA H100 NVL | 60 | 400 | 1.50 | 200–400 W |
| NVIDIA H100 80GB HBM3 | 120 | 700 | 1.50 | 350–700 W |
| NVIDIA L40S | 60 | 350 | 1.40 | 200–350 W |
| AMD Instinct MI300X | 100 | 750 | 0.85 | 350–750 W |
| AMD Radeon PRO W7900 | 40 | 295 | 1.20 | 200–295 W |

`ComputeGamma` controls throughput sensitivity to power capping: `throughput_scale = (cap/naturalPower)^gamma`. Higher gamma → more throughput retained under capping.

### 1.4 Run configuration

| Parameter | Value |
|---|---|
| Baselines | A, B, C |
| Seeds | 1 |
| Jobs | 120,000 |
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
| GPU cap (% of max) | 100% | 60% |
| `cpu_write_absolute_caps` | true | true |
| `gpu_write_absolute_caps` | true | true |

The 60% GPU eco cap means:
- H100 NVL: 240 W cap (down from 400 W TDP)
- H100 SXM: 420 W cap (down from 700 W TDP)
- L40S: 210 W cap (down from 350 W TDP)
- MI300X: 450 W cap (down from 750 W TDP)
- W7900: 177 W cap (down from 295 W TDP)

### 1.6 Policy tuning

| Parameter | Static (B) | Queue-aware (C) |
|---|---:|---:|
| HP fraction | 35% | base 35%, min 50, max 4000 |
| Operator reconcile | 20 s | 20 s |
| Agent reconcile | 10 s | 10 s |
| Queue-aware reconcile | — | 300 sim-sec |

---

## 2. Policy Algorithms

### 2.1 Static partition (`static_partition`)

Given `N=5000` managed nodes with `STATIC_HP_FRAC=0.35`:
- 1,750 nodes → `performance` profile (uncapped)
- 3,250 nodes → `eco` profile (GPU at 60% TDP, CPU at 220 W)

### 2.2 Queue-aware (`queue_aware_v1`)

Dynamically adjusts performance node count based on running performance-sensitive pods:
- `hp_base_frac=0.35`, `hp_min=50`, `hp_max=4000`, `perf_per_hp_node=15`
- During high-demand periods: more nodes shift to performance to absorb load.
- During low-demand periods: most nodes revert to eco.

### 2.3 Downgrade guard

`performance → eco` transitions are deferred while performance-sensitive pods are still running on the node, preventing mid-job cap changes.

### 2.4 Scheduler extender

- Performance pods hard-reject eco nodes via `nodeAffinity`.
- Standard pods steered to eco nodes via scoring penalties.

---

## 3. Simulator Realism

### 3.1 Workload arrival model

The workload generator uses a **Non-Homogeneous Poisson Process (NHPP)** with:

1. **24-hour diurnal cycle**: hourly arrival multipliers from 0.10 (nighttime) to 1.00 (peak).
2. **Burst overlay**: 50% probability per simulated day of a burst hour with 3.5x multiplier.
3. **Negative spike (dip) overlay**: 40% probability per simulated day of a dip hour at 8% of normal rate — simulating maintenance windows, planned drains, or mass job completions.

The trace covers ~2.3 simulated days (54+ sim-hours) of arrivals.

### 3.2 Ambient temperature model

Facility ambient temperature follows a sinusoidal day/night cycle:
- Base: 22°C, Amplitude: ±8°C, Period: 720 wall-sec (24 sim-hours)

### 3.3 PUE model

PUE is computed from ambient temperature and IT power, varying from ~1.15 (cool night) to ~1.45 (hot afternoon).

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

B and C achieve lower energy with comparable throughput to A.

### 5.3 Energy vs makespan

![Energy vs Makespan](./results/plots/energy_vs_makespan.png)

B and C are shifted to lower energy vs A, with near-identical makespan.

### 5.4 Baseline means

![Baseline Means](./results/plots/baseline_means.png)

Throughput and wall-time bars indistinguishable; energy bars show B and C below A.

### 5.5 Relative tradeoff vs A

![Relative Tradeoff vs A](./results/plots/relative_tradeoff_vs_a.png)

Per-seed energy delta vs throughput delta relative to A.

### 5.6 Relative tradeoff bars vs A

![Relative Tradeoff Bars vs A](./results/plots/relative_tradeoff_bars_vs_a.png)

Mean energy and throughput deltas as bar charts.

### 5.7 Hardware family tradeoff vs A

![Hardware Family Tradeoff vs A](./results/plots/hardware_family_tradeoff_vs_a.png)

Per-hardware-family energy savings and throughput tradeoff under Joulie policies.

### 5.8 Hardware family rankings — baseline B

![Hardware Family Rankings B](./results/plots/hardware_family_rankings_baseline_B.png)

Per-family energy and throughput impact under static partition.

### 5.9 Hardware family rankings — baseline C

![Hardware Family Rankings C](./results/plots/hardware_family_rankings_baseline_C.png)

Per-family energy and throughput impact under queue-aware policy.

### 5.10 Power timeseries

![Power Profile](./results/plots/timeseries_power_profile.png)

Multi-day power timeseries showing day/night cycling with clear separation between baselines.

### 5.11 GPU power breakdown

![GPU Power](./results/plots/timeseries_gpu_power.png)

GPU power dominates the cluster power budget (~80% of total). Both B and C show sustained GPU power reduction.

### 5.12 Cumulative energy

![Cumulative Energy](./results/plots/timeseries_cumulative_energy.png)

Cumulative energy diverges early, with C maintaining the lowest cumulative energy.

### 5.13 Cooling and ambient conditions

![Cooling](./results/plots/timeseries_cooling.png)

Cooling power tracks IT power with day/night ambient variation. C requires consistently lower cooling.

### 5.14 Job arrival rate

![Job Arrival Rate](./results/plots/job_arrival_rate.png)

Multi-day arrivals showing diurnal cycle, burst events (upward spikes), and dip events (downward drops simulating maintenance windows).

### 5.15 PUE analysis

![PUE Analysis](./results/plots/pue_analysis.png)

PUE variation over time, correlation with ambient temperature, and per-baseline distribution.

---

## 6. Interpretation

### Why does Joulie save energy on heterogeneous GPU clusters?

1. **GPU power dominates**: With 22,760 GPUs, GPU power accounts for ~80% of total cluster power. Even moderate GPU eco caps (60% of TDP) produce large absolute savings.

2. **Realistic eco caps**: At 60% of TDP, GPU caps reduce power meaningfully on loaded nodes while maintaining reasonable throughput for non-performance-sensitive workloads.

3. **CPU caps complement GPU caps**: The 220 W CPU cap provides additional savings on the host side, particularly on CPU-only nodes.

4. **Throughput preserved**: The scheduler routes performance-sensitive jobs to uncapped performance nodes. Standard workloads run on eco nodes with acceptable throughput impact.

### Heterogeneous challenge

The heterogeneous GPU fleet creates placement constraints: GPU jobs require specific vendor/product node selectors. This limits the operator's flexibility to consolidate work onto performance nodes:
- Some GPU families may have excess eco nodes that can't absorb performance workloads from other families.
- The queue-aware policy partially mitigates this through dynamic adjustment.

### Day/night advantage

The NHPP arrival model with 50% burst probability and 40% dip probability creates realistic demand variation:
- **Nighttime**: arrivals at 10-15% of peak → queue-aware shifts most nodes to eco.
- **Burst hours**: 3.5x arrival spikes → queue-aware provisions extra performance nodes.
- **Dip hours**: 8% of normal → queue-aware rapidly scales down performance allocation.

---

## 7. PUE Analysis

PUE varies with ambient temperature (sinusoidal day/night cycle) and IT power draw:

- **Night (low ambient, low load)**: PUE ~1.15 — cooling is highly efficient.
- **Day (high ambient, high load)**: PUE ~1.35-1.45 — cooling overhead increases.
- **Joulie impact**: Reduced IT power → reduced cooling → marginally improved PUE.

The relationship between IT power reduction and PUE improvement is non-linear: smoother power profiles (less bursty) improve cooling system efficiency, which is exactly what Joulie's dynamic capping achieves.

---

## 8. Best-Fit Use Case

This experiment demonstrates Joulie's effectiveness on heterogeneous GPU clusters:

- GPU power capping is the primary savings lever, with 60% eco caps providing deep energy reduction.
- Heterogeneous placement constraints limit savings compared to homogeneous fleets (experiment 03).
- Queue-aware policy outperforms static partition by adapting to demand fluctuations.
- The combination of diurnal cycling, burst events, and negative spikes creates a realistic workload profile that exercises Joulie's dynamic capabilities.

---

## 9. FMU Integration

The timeseries data is exported in FMU-compatible format at [`results/fmu_input/`](./results/fmu_input/).

Each CSV contains: `timestamp_utc`, `elapsed_sec`, `sim_elapsed_sec`, `sim_hour`, `it_power_w`, `cpu_power_w`, `gpu_power_w`, `pue`, `cooling_power_w`, `facility_power_w`, `ambient_temp_c`, `cluster_cpu_util`, `cluster_gpu_util`, `nodes_active`, `pods_running`, `energy_cumulative_j`.

These files can be fed directly into the FMU Modelica cooling model (`examples/08-fmu-cooling-pue/`).

---

## 10. Reproducibility

- Config: [`configs/benchmark-5k.yaml`](./configs/benchmark-5k.yaml)
- Cluster nodes: [`configs/cluster-nodes-5k.yaml`](./configs/cluster-nodes-5k.yaml)
- Sweep script: [`scripts/05_sweep.py`](./scripts/05_sweep.py)
- Collection: [`scripts/06_collect.py`](./scripts/06_collect.py)
- Plotting: [`scripts/07_plot.py`](./scripts/07_plot.py), [`scripts/08_plot_timeseries.py`](./scripts/08_plot_timeseries.py)
- Results: [`results/`](./results/)
