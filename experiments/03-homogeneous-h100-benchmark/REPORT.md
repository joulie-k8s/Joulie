# Homogeneous H100 NVL Benchmark Report

## Scope

This report documents the benchmark results from:

- [`experiments/03-homogeneous-h100-benchmark/`](.)

It covers a homogeneous GPU cluster (33 NVIDIA H100 NVL nodes) plus 8 CPU-only nodes — the same total node count as experiment 02, but with a single GPU family.

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
| kwok-h100-nvl | 33 | AMD EPYC 9654 | 192 | NVIDIA H100 NVL | 8 | 400 |
| kwok-cpu-highcore | 2 | AMD EPYC 9965 | 384 | — | 0 | — |
| kwok-cpu-highfreq | 2 | AMD EPYC 9375F | 64 | — | 0 | — |
| kwok-cpu-intensive | 4 | AMD EPYC 9655 | 192 | — | 0 | — |

Total: **41 nodes**, **264 GPUs (H100 NVL)**, **8064 CPU cores**.

### 1.3 Hypothesis

Joulie performs better on a homogeneous cluster because every node can accept any GPU job, eliminating the vendor/product-specific placement constraints that restrict policy flexibility in the heterogeneous case (experiment 02).

### 1.4 Run configuration

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

### 1.5 RAPL cap configuration

Same as experiment 02:

| Parameter | Performance | Eco |
|---|---:|---:|
| CPU cap (absolute watts) | 600 W | 280 W |
| GPU cap (% of max) | 100% | 70% |
| GPU eco cap per GPU | 280 W | 280 W |

---

## 2. Measured Results

### 2.1 Baseline summary

| Baseline | Wall (s) | Throughput (jobs/sim-hr) | Energy (kWh) | Avg Power (W) |
|---|---:|---:|---:|---:|
| A (no Joulie) | 312.4 | 549 | 41.54 | 47,869 |
| B (static) | 310.6 | 552 | 36.39 | 42,179 |
| C (queue-aware) | 309.6 | 553 | 32.97 | 38,333 |

### 2.2 Relative to baseline A

| Baseline | Energy Delta | Power Delta | Throughput Delta |
|---|---:|---:|---:|
| B (static) | **-12.4%** | **-11.9%** | +0.6% |
| C (queue-aware) | **-20.6%** | **-19.9%** | +0.9% |

---

## 3. Interpretation

### Homogeneous advantage confirmed

Comparing with experiment 02 (heterogeneous):

| Metric | Exp 02 (hetero) C vs A | Exp 03 (homo) C vs A |
|---|---:|---:|
| Energy savings | -12.7% | **-20.6%** |
| Throughput delta | +0.2% | +0.9% |

The homogeneous cluster delivers **62% larger energy savings** than the heterogeneous cluster. This confirms the hypothesis: when every GPU node is identical, the operator has full flexibility to assign any node to eco or performance mode. In the heterogeneous case, placement constraints (GPU vendor/model affinity) limit which nodes can accept which workloads, reducing the operator's ability to consolidate work onto performance nodes and free up eco nodes.

### Both policies achieve >10% savings

Unlike experiment 02 where static partition (B) increased energy by 2.8%, here B achieves -12.4% savings. The homogeneous GPU fleet eliminates the mismatch problem: any node can serve any job, so the static partition's fixed eco/perf split works correctly.

### Queue-aware is consistently superior

Across all three experiments:

| Experiment | B (static) vs A | C (queue-aware) vs A |
|---|---:|---:|
| 01 CPU-only | -20.1% | **-25.7%** |
| 02 Heterogeneous GPU | +2.8% | **-12.7%** |
| 03 Homogeneous H100 | -12.4% | **-20.6%** |

The queue-aware policy consistently outperforms static partition by 5-15 percentage points.

---

## 4. Plots

### 4.1 Power timeseries

![Power Profile](./results/plots/timeseries_power_profile.png)

Three clear power levels: A ~48 kW, B ~42 kW, C ~38 kW. The separation is sustained throughout the run.

### 4.2 GPU power breakdown

![GPU Power](./results/plots/timeseries_gpu_power.png)

GPU power dominates (>85% of total). CPU power is flat at ~5 kW across all baselines. The GPU panel shows A ~44 kW, B ~38 kW, C ~34 kW.

### 4.3 Cumulative energy

![Cumulative Energy](./results/plots/timeseries_cumulative_energy.png)

Linear divergence from the start — all three baselines have steady power draw, so cumulative energy grows linearly with different slopes.

### 4.4 Cooling and ambient conditions

![Cooling](./results/plots/timeseries_cooling.png)

Cooling power tracks IT power. C requires consistently lower cooling, reducing both direct energy cost and PUE overhead.

### 4.5 Throughput vs energy

![Throughput vs Energy](./results/plots/throughput_vs_energy.png)

All baselines achieve comparable throughput (~549-553 jobs/sim-hr) while C saves 20.6% in energy.

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
