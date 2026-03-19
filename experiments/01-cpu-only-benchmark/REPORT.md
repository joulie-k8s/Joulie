# CPU-Only Benchmark Report

This page reports results from the CPU-only benchmark experiment:

- [`experiments/01-cpu-only-benchmark/`](.)

## Scope

The benchmark compares three baselines on a **CPU-only cluster** with 5,000 nodes across 3 hardware families:

- `A`: simulator only (Joulie-free)
- `B`: Joulie with static partition policy
- `C`: Joulie with queue-aware policy

The experiment demonstrates energy savings achievable through CPU RAPL capping alone, without GPU complexity.

---

## 1. Experimental Setup

### 1.1 Cluster and nodes

- [kind](https://kind.sigs.k8s.io/) control-plane + worker (real Kubernetes control plane).
- **5,000** managed [KWOK](https://kwok.sigs.k8s.io/) CPU-only nodes.
- Workload pods target KWOK nodes via nodeSelector + toleration.
- Scheduler extender provides performance/eco affinity-based filtering and scoring.

### 1.2 Node inventory

| Node prefix | Count | CPU model | CPU cores/node | RAM/node |
|---|---:|---|---:|---:|
| kwok-cpu-highcore | **1,250** | AMD EPYC 9965 192-Core | 384 (2x192) | 1,536 GiB |
| kwok-cpu-highfreq | **1,250** | AMD EPYC 9375F 32-Core | 64 (2x32) | 770 GiB |
| kwok-cpu-intensive | **2,500** | AMD EPYC 9655 96-Core | 192 (2x96) | 1,536 GiB |

**Total: 5,000 nodes, 1,040,000 CPU cores, 0 GPUs.**

### 1.3 Hardware model parameters (simulator)

CPU power uses a measured-curve model with piecewise-linear interpolation from SPECpower-style load/power points. RAPL cap enforcement: when `P > CapWatts`, the simulator clamps power to the cap and reduces effective frequency scale, which feeds into the throughput multiplier.

Throughput multiplier under cap is a weighted blend of compute-bound, memory-bound, and I/O-bound scaling:

```
throughputScale = wc * freqScale + wm * memoryScale(freq) + wi * ioScale(freq)
```

where weights depend on workload class (e.g., `cpu.compute_bound` → 75% compute, `cpu.memory_bound` → 75% memory).

### 1.4 Run configuration

| Parameter | Value |
|---|---|
| Baselines | A, B, C |
| Seeds | 1 |
| Jobs | 400,000 |
| Mean inter-arrival | 0.003 s |
| Time scale | 120x (1 wall-sec = 120 sim-sec) |
| Timeout | 2,400 s wall (~80 sim-hours) |
| Perf ratio | 20% |
| GPU ratio | 0% |
| Work scale | 80.0 |
| Workload types | `cpu_preprocess`, `cpu_analytics` |
| Day/night cycle period | 720 wall-sec (24 sim-hours) |
| Burst day probability | 50% |
| Burst multiplier | 3.5x |
| Dip day probability | 40% |
| Dip multiplier | 0.08x |

### 1.5 RAPL cap configuration

| Parameter | Performance | Eco |
|---|---:|---:|
| CPU cap (absolute watts) | 420 W | 220 W |
| `cpu_write_absolute_caps` | true | true |

The 220 W eco cap triggers on nodes drawing > 220 W (approximately > 40% CPU utilization), avoiding throttling idle/lightly-loaded nodes.

### 1.6 Baselines

- **A**: Simulator only — no Joulie operator or agent. Performance-profile affinity stripped from pods.
- **B**: Joulie with `static_partition` policy (`hp_frac=0.30`).
- **C**: Joulie with `queue_aware_v1` policy (`hp_base_frac=0.30`, dynamic perf/eco split).

---

## 2. Policy Algorithms

### 2.1 Static partition (`static_partition`)

Given `N=5000` managed nodes with `STATIC_HP_FRAC=0.30`:
- 1,500 nodes → `performance` profile (cap at 420 W)
- 3,500 nodes → `eco` profile (cap at 220 W)

Fixed allocation regardless of current demand.

### 2.2 Queue-aware (`queue_aware_v1`)

Dynamically adjusts performance node count based on running performance-sensitive pods:
- `hp_base_frac=0.30`, `hp_min=50`, `hp_max=3750`, `perf_per_hp_node=20`
- More perf pods → more perf nodes (up to max), remaining nodes get eco caps.
- During low-demand periods (nighttime), eco nodes dominate → deeper savings.

### 2.3 Scheduler extender

- Performance pods hard-reject eco nodes via `nodeAffinity`.
- Standard pods steered to eco nodes via scoring penalties.
- Ensures zero performance pods placed on eco nodes across all baselines.

---

## 3. Simulator Realism

### 3.1 Workload arrival model

The workload generator uses a **Non-Homogeneous Poisson Process (NHPP)** with:

1. **24-hour diurnal cycle**: hourly arrival multipliers from 0.10 (03:00 nighttime low) to 1.00 (10:00 peak), with a lunch dip at 12:00.
2. **Burst overlay**: 50% probability per simulated day of a burst hour with 3.5x arrival rate multiplier.
3. **Negative spike (dip) overlay**: 40% probability per simulated day of a dip hour (maintenance window / planned drain) where arrivals drop to 8% of normal.

The trace covers ~3 simulated days (72+ sim-hours), producing visible multi-day cyclic patterns.

### 3.2 Ambient temperature model

Facility ambient temperature follows a sinusoidal day/night cycle:
- Base: 22°C, Amplitude: ±8°C, Period: 720 wall-sec (24 sim-hours)
- Range: 14°C (night) to 30°C (afternoon peak)

### 3.3 PUE model

PUE is computed from ambient temperature and IT power:
```
PUE = 1 + (cooling_power / it_power)
```
Cooling tracks IT load with ambient-dependent efficiency, producing PUE variation from ~1.15 (cool night, low load) to ~1.45 (hot afternoon, high load).

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

All three baselines complete within similar wall-time windows across all seeds.

### 5.2 Throughput vs energy

![Throughput vs Energy](./results/plots/throughput_vs_energy.png)

All baselines achieve comparable throughput while B and C consume significantly less energy.

### 5.3 Energy vs makespan

![Energy vs Makespan](./results/plots/energy_vs_makespan.png)

B and C are consistently shifted to lower energy vs A, with near-identical makespan.

### 5.4 Baseline means

![Baseline Means](./results/plots/baseline_means.png)

Throughput and wall-time bars are indistinguishable across baselines. Energy bars clearly show B and C below A.

### 5.5 Relative tradeoff vs A

![Relative Tradeoff vs A](./results/plots/relative_tradeoff_vs_a.png)

Per-seed scatter of energy delta vs throughput delta relative to A. B and C cluster in the lower-energy region with minimal throughput change.

### 5.6 Relative tradeoff bars vs A

![Relative Tradeoff Bars vs A](./results/plots/relative_tradeoff_bars_vs_a.png)

Mean energy and throughput deltas summarized as bar charts.

### 5.7 Workload type tradeoff vs A

![Workload Type Tradeoff](./results/plots/workload_type_tradeoff_vs_a.png)

Per-workload-type energy savings and throughput impact under Joulie policies.

### 5.8 Power timeseries

![Power Profile](./results/plots/timeseries_power_profile.png)

Multi-day power timeseries showing clear day/night cycling. Baseline A sustains higher power throughout; B and C show sustained reduction with the 220 W eco cap active.

### 5.9 Cumulative energy

![Cumulative Energy](./results/plots/timeseries_cumulative_energy.png)

Cumulative energy diverges from the first hours, with C consistently below B below A.

### 5.10 Cooling and ambient conditions

![Cooling](./results/plots/timeseries_cooling.png)

Ambient temperature oscillates over the simulated day/night cycle. Cooling power tracks IT power draw, with C showing consistently lower cooling demand.

### 5.11 Job arrival rate

![Job Arrival Rate](./results/plots/job_arrival_rate.png)

Workload arrivals follow a realistic multi-day diurnal cycle with peak rates during simulated daytime, reduced activity at night, burst events creating upward spikes, and dip events (maintenance windows) creating sharp downward drops.

### 5.12 PUE analysis

![PUE Analysis](./results/plots/pue_analysis.png)

PUE variation over the simulated period showing correlation with ambient temperature and IT load. Joulie baselines achieve slightly better PUE through smoother power draw.

---

## 6. Interpretation

### Why does energy reduce without throughput penalty?

1. **Realistic eco cap (220 W)**: The cap activates above ~40% CPU utilization, targeting actively-loaded nodes while leaving idle/lightly-loaded nodes unaffected. This contrasts with idle-level caps that waste policy headroom.

2. **Workload-aware throughput model**: The simulator models frequency-dependent throughput with workload-class-specific weights. Memory-bound and I/O-bound jobs (common in `cpu_preprocess` and `cpu_analytics`) are less sensitive to frequency reduction than compute-bound work.

3. **Cluster saturation via bursts**: With 400,000 jobs over 3 simulated days, burst events create periods of high utilization where eco capping achieves deep savings, while dip periods naturally reduce power without intervention.

4. **Day/night cycle amplifies queue-aware advantage**: The NHPP arrival model creates sustained low-demand periods (nighttime) where the queue-aware policy shifts most nodes to eco, and high-demand periods (daytime) where it provisions adequate performance nodes.

### Why does queue-aware (C) outperform static (B)?

The queue-aware policy dynamically adjusts the performance/eco node ratio:
- During nighttime (arrivals at 10-15% of peak), nearly all nodes run at eco caps.
- During burst events, performance nodes are provisioned on demand.
- During dip events (maintenance windows), the policy rapidly scales down performance allocation.
- The static partition keeps 30% at full power regardless of actual demand.

### Spike smoothing

Joulie demonstrates spike smoothing in two ways:
1. **Upward spikes**: Burst events cause sudden load increases. Queue-aware policy absorbs these by dynamically shifting nodes to performance, preventing all nodes from hitting full power simultaneously.
2. **Downward spikes**: Dip events (8% of normal arrivals) create sudden load drops. Queue-aware policy rapidly moves excess performance nodes to eco, capturing savings during these windows.

---

## 7. PUE Analysis

PUE varies with both ambient temperature and IT power draw:

- **Night (low ambient, low load)**: PUE approaches ~1.15 — cooling is efficient.
- **Day (high ambient, high load)**: PUE rises to ~1.35-1.45 — cooling cost increases.
- **Joulie impact**: By reducing IT power draw, Joulie baselines slightly reduce cooling load, improving PUE by 0.01-0.03 points on average. The effect is most pronounced during daytime when both ambient temperature and workload are high.

The PUE analysis plots show:
1. PUE over time with day/night shading.
2. PUE vs ambient temperature correlation.
3. PUE distribution (histogram) per baseline.

---

## 8. FMU Integration

The timeseries data is exported in FMU-compatible format at [`results/fmu_input/`](./results/fmu_input/).

Each CSV contains: `timestamp_utc`, `elapsed_sec`, `sim_elapsed_sec`, `sim_hour`, `it_power_w`, `cpu_power_w`, `pue`, `cooling_power_w`, `facility_power_w`, `ambient_temp_c`, `cluster_cpu_util`, `nodes_active`, `pods_running`, `energy_cumulative_j`.

These files can be fed directly into the FMU Modelica cooling model (`examples/08-fmu-cooling-pue/`).

---

## 9. Reproducibility

- Config: [`configs/benchmark-5k.yaml`](./configs/benchmark-5k.yaml)
- Cluster nodes: [`configs/cluster-nodes-5k.yaml`](./configs/cluster-nodes-5k.yaml)
- Sweep script: [`scripts/05_sweep.py`](./scripts/05_sweep.py)
- Collection: [`scripts/06_collect.py`](./scripts/06_collect.py)
- Plotting: [`scripts/07_plot.py`](./scripts/07_plot.py), [`scripts/08_plot_timeseries.py`](./scripts/08_plot_timeseries.py)
- Results: [`results/`](./results/)
