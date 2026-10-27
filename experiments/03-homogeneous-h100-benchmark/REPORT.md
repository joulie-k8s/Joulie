# Homogeneous H100 NVL Benchmark Report (KWOK, 41 Nodes)

This report compares three baselines on a cluster whose GPU nodes are all H100 NVL, at regime: after the ramp-up and before the drain. The runs are of 2026-10-27, with the Joulie and simulator images `joulie.local/*:rerun-2026-10-27` built from this tree ([`experiments/README.md`](../README.md), "Images").

- `A`: the simulator only, no power management.
- `B`: Joulie with the static partition policy.
- `C`: Joulie with the queue-aware policy.

In short: B and C used 7 to 10 % less GPU-node energy per unit of GPU work than A, well above the run-to-run noise of 1.6 %, and they did so by delaying jobs: they finished 7 to 11 % less GPU work over the window, with 8 to 11 performance jobs waiting on average where A had 0.6. B and C differ by less than the noise, so this report does not rank one above the other.

## 1. Experimental Setup

### 1.1 Cluster and nodes

A [kind](https://kind.sigs.k8s.io/) cluster (control plane and one worker) runs the real Kubernetes control plane and the Joulie components; 41 [KWOK](https://kwok.sigs.k8s.io/) nodes stand for the managed hardware, and the Joulie simulator models their power and runs the workload's pods.

### 1.2 Node inventory

| Nodes | Count | CPU | Cores | GPUs |
|---|---:|---|---:|---|
| `kwok-h100-nvl` | 33 | 2 x AMD EPYC 9654 | 192 | 8 x NVIDIA H100 NVL |
| `kwok-cpu-highcore` | 2 | 2 x AMD EPYC 9965 | 384 | none |
| `kwok-cpu-highfreq` | 2 | 2 x AMD EPYC 9375F | 64 | none |
| `kwok-cpu-intensive` | 4 | 2 x AMD EPYC 9655 | 192 | none |

264 GPUs and 8,000 cores in all, of which 1,664 on the 8 CPU-only nodes. GPU nodes carry `joulie.io/hw.kind=gpu`, CPU-only nodes `cpu-only`.

### 1.3 Hardware models in the simulator

As resolved by the simulator in these runs (`sim_debug_nodes.json`):

- H100 NVL: 400 W maximum board power and 200 W lowest cap per GPU (the H100 PCIe product brief's minimum; no NVL minimum is published), 60 W idle.
- CPUs: SPECpower curves for the EPYC 9654 (128 W idle to 817 W at full load, two sockets), 9655 (126 to 751 W) and 9965 (157 to 800 W); the 9375F has no published curve and borrows the 9655's.
- A cap below the hardware's lowest is refused, never raised (`simulator/CHANGES.md`).

### 1.4 Run configuration

| Parameter | Value |
|---|---|
| Baselines | A, B, C |
| Seeds | 1 and 2, counterbalanced: A1 B1 C1, then C2 B2 A2, then A1 again for the run-to-run noise |
| Time scale | 120 (one wall second is two simulated minutes) |
| Trace window | 900 wall seconds (30 simulated hours), after a prefill of 1,500 wall seconds |
| Measured window | 120 to 900 wall seconds |
| Jobs in the window | 2,239 (seed 1) and 2,259 (seed 2), 46 and 48 % of them performance jobs |
| GPU requests | 1, 2, 4 or 8 GPUs (seed 1: 1,242, 155, 93 and 51 jobs), 698 CPU-only jobs |
| `work_scale`, `gpu_util_scale` | 0.0646, 1.45 |

The load comes from the sizing rule of [`experiments/README.md`](../README.md) ("Sizing the load"): 0.55 of the GPUs at regime by `size_load.py`, kept because baseline A passed its gates. A measured a GPU utilization of 0.664 and 0.694 (seeds 1 and 2): `size_load.py` under-predicts it.

Every run passed its gates (`04_run_one.py`): its polls cover the window, no control command was refused, its mean 1-minute host load stayed at most 10 (4.8 to 8.5), and baseline A kept on average at most 1 % of its running pods queued and reached at least 45 % utilization. No job without GPUs ran on a GPU node (2,091 to 2,108 finished jobs per run).

### 1.5 Caps

| | Performance | Eco |
|---|---:|---:|
| GPU cap | 100 % of 400 W | 70 %, 280 W |
| CPU cap, CPU-only nodes | 100 % of the TDP | 60 % of the TDP |
| CPU cap, GPU nodes | none | none |

A GPU node's CPU gets no eco cap (Joulie's default). A percent CPU cap is that share of the TDP per socket, within the catalog's range (0.55 x TDP to the TDP).

### 1.6 Policy sizing

Performance jobs may run only on performance nodes under B and C. Both policies are sized on seeds 3 and 4, which these runs do not use (`experiments/kwok-cluster/pool_load.py`):

- B: `hp_frac` 0.66, 27 of 41 nodes, enough for the performance demand at its peak. `PerformanceSet` splits them over hardware families in proportion to their nodes: 22 H100 NVL nodes (176 GPUs) and 5 CPU-only nodes.
- C: holds max(16, ceil(performance pods / 5)) performance nodes, the pods running or pending (`hp_base_frac` 0.39, `perf_per_hp_node` 5), up to all 41. It held 22.8 and 25.2 nodes on average.

These counts hold the performance GPUs the jobs request, not their placement: they ignore that an 8-GPU job needs a whole node.

## 2. Policy Algorithms

### 2.1 Static partition (`static_partition`)

A fixed set of performance nodes, chosen by `PerformanceSet`; every other node is eco.

### 2.2 Queue-aware (`queue_aware_v1`)

The number of performance nodes follows the performance pods, running and pending: `max(hp_base, ceil(pods / perf_per_hp_node))`, within `hp_min` and `hp_max`, recomputed every 300 simulated seconds.

### 2.3 Scheduler

kube-scheduler bin-packs the GPUs (`NodeResourcesFit` with `MostAllocated` over the GPU resources), so whole nodes stay free for 8-GPU jobs. Under B and C the Joulie scheduler extender also scores nodes, at weight 1; its power headroom score favours emptier nodes. Performance pods carry `joulie.io/power-profile NotIn [eco]`, so they never run on eco nodes.

## 3. Simulator Realism

### 3.1 Workload

The trace comes from `simulator/cmd/workloadgen` (schema v2): Poisson arrivals at a rate that follows a 24-hour pattern in simulated time, with a burst hour on half of the days and a dip hour on 40 % of them. Over the window, simulated 02:00 of the third day to 08:00 of the fourth, the arrivals cover two nights and a day. A GPU job's GPU utilization is the generator's, raised by 45 % (at most 1), since the generated GPU jobs use about half of what they request.

### 3.2 Facility

The outside air follows a sinusoid of 22 ± 8 °C with a period of 24 simulated hours. The PUE is an assumed analytic function of that temperature and of the IT load (1.08 at 15 °C, plus 0.006 per degree above, plus up to 0.04 at full load, plus an interaction of the two; `simulator/pkg/facility`), not fitted to a facility; cooling power is IT power x (PUE - 1).

## 4. Measured Results

From 120 to 900 wall seconds. Work is counted in the simulator's units: a job advances `base_speed_per_core` (2) units per requested GPU, or core, per wall second at full speed; a GPU unit is one unit of GPU work. Energy, work and backlog are given in simulated terms, wall values times the time scale.

### 4.1 Per run

| Baseline | Seed | GPU-node energy per GPU unit (J) | Avg IT power (W) | GPU work (units) | GPU util (%) | Perf. nodes | Pending perf. jobs | Pending 8-GPU jobs | Backlog start / end (GPU units) | Host load |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| A | 1 | 197.7 | 76,098 | 34,850,897 | 66.4 | | 0.6 | 0.6 | 13,805,813 / 9,098,195 | 6.4 |
| A | 2 | 196.9 | 78,366 | 35,978,903 | 69.4 | | 0.6 | 0.1 | 12,843,053 / 11,337,161 | 5.8 |
| B | 1 | 183.3 | 63,404 | 31,074,401 | 52.2 | 27.0 | 10.5 | 9.3 | 14,753,231 / 13,814,881 | 4.8 |
| B | 2 | 178.3 | 66,196 | 33,332,243 | 54.6 | 27.0 | 8.0 | 7.2 | 13,266,399 / 14,407,166 | 5.5 |
| C | 1 | 181.6 | 63,511 | 31,391,270 | 52.0 | 22.8 | 8.5 | 8.0 | 14,434,402 / 13,257,739 | 7.2 |
| C | 2 | 178.0 | 65,406 | 32,894,753 | 53.4 | 25.2 | 10.8 | 10.1 | 13,279,873 / 14,870,943 | 8.5 |

### 4.2 Change against A, paired by seed

| Baseline | GPU-node energy per GPU unit | GPU work done | Avg IT power | Backlog at end | CPU-only-node energy per CPU-only unit |
|---|---:|---:|---:|---:|---:|
| B | -7.3 % / -9.4 % | -10.8 % / -7.4 % | -16.7 % / -15.5 % | +51.8 % / +27.1 % | +2.5 % / +0.2 % |
| C | -8.2 % / -9.6 % | -9.9 % / -8.6 % | -16.5 % / -16.5 % | +45.7 % / +31.2 % | +5.1 % / +5.9 % |

### 4.3 Run-to-run noise

Baseline A, seed 1, run again at the end: GPU-node energy per GPU unit -1.6 %, IT power -1.7 %, GPU work -0.1 % (host load 6.4 and 7.7). The two runs ran the same jobs for the same times (2,103 jobs in common, median difference in run time 0 s) but placed them differently: no GPU node ran the same number of jobs in both, and node energies differed from -60 % to +149 %. kube-scheduler picks at random among equally scored nodes, and a node's power is not linear in its load (a GPU node turns on whole devices), so the cluster's energy moves with placement. Differences between baselines below about 2 % are noise.

## 5. Plot Commentary

The time plots show seed 1 over the whole run, 0 to 30 simulated hours, with the measured window shaded; the bar charts average both seeds over the window.

### 5.1 Power timeseries

![Timeseries](./img/kwok/timeseries.png)

Seed 1: IT power, CPU and GPU utilization and running pods per baseline. Section 4.1 gives the window's averages for both seeds.

### 5.2 Energy comparison

![Energy Comparison](./img/kwok/energy_comparison.png)

GPU-node energy per GPU unit over the window: the mean of the two seeds per baseline, each seed as a dot (section 4.1).

### 5.3 Cumulative energy and work

![Cumulative Energy](./img/kwok/cumulative_energy.png)

Seed 1: IT energy and GPU work accumulated from the start of the run. Section 4.1 gives the window's totals.

### 5.4 Utilization

![Utilization Summary](./img/kwok/utilization_summary.png)

Seed 1: histograms of CPU and GPU utilization over the window.

### 5.5 PUE

![PUE Analysis](./img/kwok/pue_analysis.png)

Seed 1: IT power (solid) and cooling power (dashed), the analytic PUE (section 3.2) and the outside temperature over the run.

### 5.6 Facility power

![Facility Power Breakdown](./img/kwok/facility_power_breakdown.png)

IT and cooling power over the window, the mean of the two seeds per baseline.

## 6. Interpretation

B and C used 7 to 10 % less GPU-node energy per unit of GPU work than A, about five times the run-to-run noise. With the eco GPUs at 280 W they drew 16 to 17 % less IT power and finished 7 to 11 % less GPU work over the window, and their backlog at the end was 27 to 52 % larger.

The work they lost waited. Performance jobs may run only on performance nodes, and under B and C 8 to 11 of them waited on average, against 0.6 under A; jobs asking for 8 GPUs, of either class, waited 7 to 10 on average, against 0.1 to 0.6 under A. The partitions were sized on the GPUs the performance jobs request, and B's 176 performance GPUs exceed that demand on average (0.56 to 0.60 of the 264 GPUs on these seeds); but an 8-GPU job needs a whole performance node, which the sizing ignores, and under B and C the extender's headroom score favours emptier nodes (section 2.3). These runs do not separate the two. The static check of `pool_load.py` on these seeds already showed the partitions short at peaks: performance jobs request up to 0.74 of the GPUs, 195 against B's 176, and B's 27 nodes hold that demand in 146 of 158 samples, C (leaving its pending pods aside) in 34. The energy saving of B and C is therefore not free: it comes with delayed performance work, which an operator weighing these policies has to accept.

B and C differ by less than the noise (about 1 % against 1.6 %), so this report does not rank them. On the CPU-only nodes, B and C used 0.2 to 5.9 % more energy per unit of CPU-only work than A.

## 7. Limitations

- Two seeds and one repeat: the noise estimate comes from one pair of runs.
- The hardware models and the PUE are the simulator's; GPU power follows the catalog's board limits and an assumed power curve.
- The load (0.66 to 0.69 GPU utilization under A) is higher than the 0.55 it was sized for.
- The host is shared: each run's load is listed, and none exceeded the gate of 10.

## Reproduce

Build and load the images as [`experiments/README.md`](../README.md) ("Images") shows, run `scripts/20_run_benchmark.sh configs/benchmark-40n.yaml`, then:

```bash
python experiments/kwok-cluster/report_figures.py RESULTS_DIR experiments/03-homogeneous-h100-benchmark/img/kwok "H100 NVL, 41 nodes" experiments/03-homogeneous-h100-benchmark/configs/benchmark-40n.yaml
```
