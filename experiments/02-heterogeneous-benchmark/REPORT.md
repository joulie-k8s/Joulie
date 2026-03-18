# Heterogeneous GPU Cluster Benchmark Report

## Scope

This report documents the benchmark results from:

- [`experiments/02-heterogeneous-benchmark/`](.)

It covers a mixed CPU+GPU cluster with multiple GPU families (NVIDIA H100 NVL, H100 SXM, L40S, AMD MI300X, AMD W7900) and CPU-only nodes.

---

## 1. Experimental Setup

### 1.1 Cluster and node topology

- Kind control-plane + worker (real Kubernetes control path).
- 41 fake KWOK worker nodes (33 GPU + 8 CPU-only).
- Scheduler extender provides performance/eco affinity-based filtering and scoring.
- GPU nodes get GPU RAPL caps; CPU-only nodes get CPU RAPL caps.

### 1.2 Node inventory

| Node prefix | Count | CPU | Cores | GPU | GPU Count | GPU Max Cap (W) |
|---|---:|---|---:|---|---:|---:|
| kwok-h100-nvl | 12 | AMD EPYC 9654 | 192 | NVIDIA H100 NVL | 8 | 400 |
| kwok-h100-sxm | 6 | Intel Xeon Gold 6530 | 64 | NVIDIA H100 SXM | 4 | 700 |
| kwok-l40s | 7 | AMD EPYC 9534 | 128 | NVIDIA L40S | 4 | 350 |
| kwok-mi300x | 2 | AMD EPYC 9534 | 128 | AMD Instinct MI300X | 8 | 750 |
| kwok-w7900 | 6 | AMD EPYC 9534 | 128 | AMD Radeon PRO W7900 | 4 | 295 |
| kwok-cpu-highcore | 2 | AMD EPYC 9965 | 384 | — | 0 | — |
| kwok-cpu-highfreq | 2 | AMD EPYC 9375F | 64 | — | 0 | — |
| kwok-cpu-intensive | 4 | AMD EPYC 9655 | 192 | — | 0 | — |

Total: **41 nodes**, **168 GPUs**, **5888 CPU cores**.

### 1.3 Run configuration

| Parameter | Value |
|---|---|
| Baselines | A, B, C |
| Seeds | 1 |
| Jobs | 476 |
| GPU ratio | 35% |
| GPU request per job | 1 |
| Time scale | 10x |
| Timeout | 300 s |
| Base speed per core | 2.0 |

### 1.4 RAPL cap configuration

| Parameter | Performance | Eco |
|---|---:|---:|
| CPU cap (absolute watts) | 600 W | 280 W |
| GPU cap (% of max) | 100% | 70% |
| `cpu_write_absolute_caps` | true | true |
| `gpu_write_absolute_caps` | true | true |

---

## 2. Measured Results

### 2.1 Baseline summary

| Baseline | Wall (s) | Throughput (jobs/sim-hr) | Energy (kWh) | Avg Power (W) |
|---|---:|---:|---:|---:|
| A (no Joulie) | 309.9 | 553 | 18.28 | 21,239 |
| B (static) | 309.9 | 553 | 18.79 | 21,832 |
| C (queue-aware) | 309.3 | 554 | 15.95 | 18,569 |

### 2.2 Relative to baseline A

| Baseline | Energy Delta | Power Delta | Throughput Delta |
|---|---:|---:|---:|
| B (static) | **+2.8%** | **+2.8%** | 0.0% |
| C (queue-aware) | **-12.7%** | **-12.6%** | +0.2% |

---

## 3. Interpretation

### Why does static partition (B) perform worse than no Joulie (A)?

On a heterogeneous GPU cluster, the static partition concentrates performance-sensitive GPU workloads on a fixed subset of nodes. This creates an uneven distribution:
- Performance nodes run at 100% GPU cap (same as A) but carry more concentrated load.
- Eco nodes' 70% GPU cap reduces their power, but the static partition doesn't optimize which GPU families get capped.
- The net effect is slightly higher total power due to thermal overhead from concentrated load on performance nodes.

### Why does queue-aware (C) succeed?

The queue-aware policy dynamically shifts nodes between performance and eco profiles based on current GPU demand:
- When GPU demand is high, more nodes run uncapped to avoid throughput loss.
- When demand drops, nodes shift to eco (70% GPU cap), saving significant power.
- The dynamic response captures more eco opportunities than the fixed static split.
- Result: **12.7% energy savings** with zero throughput cost.

### GPU-specific dynamics

- GPU power dominates the cluster power budget (>80% of total).
- The 70% GPU eco cap was chosen to balance savings vs throughput (50% cap would cause ~50% throughput loss due to `ThroughputMultiplier = (cap/naturalPower)^computeGamma`).
- CPU caps also apply to CPU-only nodes, adding incremental savings.

### Cross-experiment comparison

| Experiment | B (static) vs A | C (queue-aware) vs A |
|---|---:|---:|
| 01 CPU-only | -20.1% | **-25.7%** |
| 02 Heterogeneous GPU | +2.8% | **-12.7%** |
| 03 Homogeneous H100 | -12.4% | **-20.6%** |

The heterogeneous cluster shows the smallest C-savings because GPU vendor/model placement constraints limit the operator's ability to consolidate workloads onto a minimal set of performance nodes.

---

## 4. Plots

### 4.1 Power timeseries

![Power Profile](./results/plots/timeseries_power_profile.png)

Clear separation between C (queue-aware) and A/B in the IT power panel. B tracks A closely due to the static partition's inefficiency on heterogeneous hardware.

### 4.2 GPU power breakdown

![GPU Power](./results/plots/timeseries_gpu_power.png)

GPU power is the dominant contributor. C shows sustained reduction in GPU power draw through dynamic eco allocation.

### 4.3 Cumulative energy

![Cumulative Energy](./results/plots/timeseries_cumulative_energy.png)

C diverges from A/B early and maintains lower cumulative energy throughout the run.

### 4.4 Cooling and ambient conditions

![Cooling](./results/plots/timeseries_cooling.png)

Cooling power tracks IT power draw. C requires consistently less cooling.

### 4.5 Throughput vs energy

![Throughput vs Energy](./results/plots/throughput_vs_energy.png)

All three baselines achieve identical throughput (~553 jobs/sim-hr), while C consumes 12.7% less energy than A.

---

## 5. FMU Integration

The timeseries data is exported in FMU-compatible format at [`results/fmu_input/`](./results/fmu_input/).

Each CSV contains: `timestamp_utc`, `elapsed_sec`, `sim_elapsed_sec`, `sim_hour`, `it_power_w`, `cpu_power_w`, `gpu_power_w`, `pue`, `cooling_power_w`, `facility_power_w`, `ambient_temp_c`, `cluster_cpu_util`, `cluster_gpu_util`, `nodes_active`, `pods_running`, `energy_cumulative_j`.

These files can be fed directly into the FMU Modelica cooling model (`examples/08-fmu-cooling-pue/`).

---

## 6. Reproducibility

- Config: [`configs/benchmark-5k.yaml`](./configs/benchmark-5k.yaml)
- Sweep: [`scripts/05_sweep.py`](./scripts/05_sweep.py)
- Collection: [`scripts/06_collect.py`](./scripts/06_collect.py)
- Plotting: [`scripts/07_plot.py`](./scripts/07_plot.py), [`scripts/08_plot_timeseries.py`](./scripts/08_plot_timeseries.py)
