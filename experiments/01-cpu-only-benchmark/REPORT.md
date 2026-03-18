# CPU-Only Benchmark Report

## Scope

This report documents the benchmark results from:

- [`experiments/01-cpu-only-benchmark/`](.)

It covers: experimental setup, controller policy algorithms, simulator models, measured outcomes, plot commentary, and interpretation.

---

## 1. Experimental Setup

### 1.1 Cluster and node topology

- Kind control-plane + worker (real Kubernetes control path).
- 40 fake KWOK worker nodes labeled `joulie.io/managed=true`.
- KWOK nodes are tainted `kwok.x-k8s.io/node=fake:NoSchedule`.
- Simulator pod runs on the real kind worker.
- Workload pods target KWOK nodes via nodeSelector + toleration.
- Scheduler extender provides performance/eco affinity-based filtering and scoring.

Node inventory source: [`configs/cluster-nodes.yaml`](./configs/cluster-nodes.yaml)

### 1.2 Node inventory

CPU-only cluster - no GPU nodes.

| Node prefix | Count | CPU | Cores | RAM |
|---|---:|---|---:|---:|
| kwok-cpu-highcore | 10 | AMD EPYC 9965 192-Core | 384 (2x192) | 1536 GiB |
| kwok-cpu-highfreq | 10 | AMD EPYC 9375F 32-Core | 64 (2x32) | 770 GiB |
| kwok-cpu-intensive | 20 | AMD EPYC 9655 96-Core | 192 (2x96) | 1536 GiB |

Total: **40 nodes**, **8320 CPU cores**, **0 GPUs**.

### 1.3 Hardware model parameters (simulator)

CPU power model:

```
P(u, f) = BaseIdleW + (PMaxW - BaseIdleW) * (u * activity)^alpha * f^beta
```

where `u` = CPU utilization, `f` = frequency scale, `activity` = workload activity factor.

| Parameter | Value |
|---|---:|
| BaseIdleW | 80 W |
| PMaxW | 420 W |
| AlphaUtil | 1.15 |
| BetaFreq | 1.35 |

RAPL cap enforcement: when `P > CapWatts`, the simulator binary-searches for the maximum frequency `f` that satisfies the cap.

### 1.4 Run configuration

| Parameter | Value |
|---|---|
| Baselines | A, B, C |
| Seeds | 1 |
| Jobs | 1,600 |
| Time scale | 10x |
| Timeout per run | 300 s |
| Base speed per core | 2.0 |
| Perf ratio | 20% |
| GPU ratio | 0% |
| Work scale | 0.50 |

### 1.5 RAPL cap configuration

| Parameter | Performance | Eco |
|---|---:|---:|
| CPU cap (absolute watts) | 420 W | 150 W |
| `cpu_write_absolute_caps` | true | true |

The 150 W eco cap triggers on nodes drawing > 150 W (approximately > 24% CPU utilization).

### 1.6 Baselines

- **A**: simulator only - no Joulie operator or agent (power-profile affinity stripped from pods).
- **B**: Joulie with `static_partition` policy (`hp_frac=0.30`, 12 perf / 28 eco).
- **C**: Joulie with `queue_aware_v1` policy (`hp_base_frac=0.30`, dynamic perf/eco split).

---

## 2. Policy Algorithms

### 2.1 Static partition (`static_partition`)

Given `N=40` managed nodes with `STATIC_HP_FRAC=0.30`:
- 12 nodes -> `performance` profile (cap at 420 W)
- 28 nodes -> `eco` profile (cap at 150 W)

### 2.2 Queue-aware (`queue_aware_v1`)

Dynamically adjusts performance node count based on running performance-sensitive pods:
- `hp_base_frac=0.30`, `hp_min=50`, `hp_max=3750`, `perf_per_hp_node=20`
- More perf pods -> more perf nodes (up to max), remaining nodes get eco caps.

### 2.3 Scheduler extender

- Performance pods hard-reject eco nodes via `nodeAffinity`.
- Standard pods steered to eco nodes via scoring penalties.
- Verified: 0 performance pods placed on eco nodes across all baselines.

---

## 3. Measured Results

### 3.1 Baseline summary

| Baseline | Wall (s) | Throughput (jobs/sim-hr) | Energy (kWh) | Avg Power (W) |
|---|---:|---:|---:|---:|
| A (no Joulie) | 302.8 | 1,902 | 7.03 | 8,362 |
| B (static) | 302.4 | 1,905 | 5.62 | 6,694 |
| C (queue-aware) | 302.4 | 1,905 | 5.23 | 6,220 |

### 3.2 Relative to baseline A

| Baseline | Energy Delta | Power Delta | Throughput Delta |
|---|---:|---:|---:|
| B (static) | **-20.1%** | **-20.0%** | +0.1% (negligible) |
| C (queue-aware) | **-25.7%** | **-25.6%** | +0.1% (negligible) |

All baselines ran for the same ~302 s wall time with identical workloads.

---

## 4. Plot Commentary

Plots are in: [`results/plots/`](./results/plots/)

### 4.1 Power profile timeseries

![Power Profile](./results/plots/timeseries_power_profile.png)

The timeseries shows clear power separation between baselines throughout the run:
- Baseline A sustains ~8.4 kW steady-state.
- Baseline B drops to ~6.7 kW with the 150 W eco cap active on 28 nodes.
- Baseline C achieves ~6.2 kW with the queue-aware dynamic split optimizing eco node count.

### 4.2 Cumulative energy

![Cumulative Energy](./results/plots/timeseries_cumulative_energy.png)

The cumulative energy plot shows divergence starting from the first seconds of the run, with C consistently below B below A — demonstrating that the energy savings are sustained throughout the workload.

### 4.3 Cooling and ambient conditions

![Cooling](./results/plots/timeseries_cooling.png)

Ambient temperature oscillates over the simulated day/night cycle. Cooling power tracks the IT power draw, with C showing consistently lower cooling demand.

### 4.4 Throughput vs energy

![Throughput vs Energy](./results/plots/throughput_vs_energy.png)

All three baselines achieve identical throughput (~1,905 jobs/sim-hr), while C consumes 25.7% less energy than A.

---

## 5. Interpretation

### Why does energy reduce by 20-26% without throughput penalty?

1. **CPU sensitivity = 0**: All trace jobs have `sensitivityCPU=0`, meaning RAPL caps reduce frequency and power but do NOT slow job completion. This models workloads where frequency reduction has minimal throughput impact (e.g., memory-bound, I/O-bound, or latency-insensitive batch).

2. **Aggressive eco cap (150 W)**: The eco cap is well below the natural power draw of loaded nodes (~200-300 W at typical utilization), forcing the simulator to reduce frequency scale significantly on eco nodes.

3. **Cluster saturation**: With 1,600 jobs on 40 nodes (8,320 total cores), nodes are ~50-80% utilized, placing most eco nodes above the 150 W threshold where the cap activates.

4. **Zero throughput cost**: Since `sensitivityCPU=0`, the reduced frequency on eco nodes doesn't slow job progress. This is the ideal scenario for RAPL-based energy management.

### Why does queue-aware (C) outperform static (B)?

The queue-aware policy dynamically adjusts the performance/eco node ratio based on current demand:
- When performance-sensitive pods are few, more nodes run at eco caps.
- This results in ~5% additional energy savings over the fixed static partition.
- The static partition keeps a fixed 30% of nodes at full power regardless of actual demand.

### Cross-experiment comparison

| Experiment | B (static) vs A | C (queue-aware) vs A |
|---|---:|---:|
| 01 CPU-only | **-20.1%** | **-25.7%** |
| 02 Heterogeneous GPU | +2.8% | **-12.7%** |
| 03 Homogeneous H100 | -12.4% | **-20.6%** |

The CPU-only experiment shows the largest savings because all nodes benefit from CPU RAPL caps without the complexity of GPU placement constraints.

---

## 6. FMU Integration

The timeseries data is exported in FMU-compatible format at [`results/fmu_input/`](./results/fmu_input/).

Each CSV contains: `timestamp_utc`, `elapsed_sec`, `sim_elapsed_sec`, `sim_hour`, `it_power_w`, `cpu_power_w`, `pue`, `cooling_power_w`, `facility_power_w`, `ambient_temp_c`, `cluster_cpu_util`, `nodes_active`, `pods_running`, `energy_cumulative_j`.

These files can be fed directly into the FMU Modelica cooling model (`examples/08-fmu-cooling-pue/`).

---

## 7. Reproducibility

- Config: [`configs/benchmark-5k.yaml`](./configs/benchmark-5k.yaml)
- Sweep script: [`scripts/05_sweep.py`](./scripts/05_sweep.py)
- Collection: [`scripts/06_collect.py`](./scripts/06_collect.py)
- Plotting: [`scripts/07_plot.py`](./scripts/07_plot.py), [`scripts/08_plot_timeseries.py`](./scripts/08_plot_timeseries.py)
- Results: [`results/`](./results/)
