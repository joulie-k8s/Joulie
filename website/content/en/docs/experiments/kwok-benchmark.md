---
title: "KWOK Benchmark Experiment"
---

This page reports the current benchmark results from the first experiment in:

- [`experiments/01-kwok-benchmark/`](https://github.com/joulie-k8s/Joulie/tree/main/experiments/01-kwok-benchmark)

## Scope

The benchmark compares three baselines:

- `A`: simulator only (Joulie-free)
- `B`: Joulie with static partition policy
- `C`: Joulie with queue-aware policy

It evaluates throughput/makespan vs energy under real Kubernetes scheduling with KWOK nodes and simulated power control.

## Experimental setup

### Cluster and nodes

- kind control-plane + worker (real control plane)
- 5 managed KWOK nodes (`kwok-node-0..4`)
- workload pods target KWOK nodes via selector + toleration

### Hardware models in simulator

Mapped by node labels to two classes:

- `intel-kwok`: `BaseIdleW=65`, `PMaxW=420`, `AlphaUtil=1.1`, `BetaFreq=1.25`, `FMin/FMax=1200/3200`
- `amd-kwok`: `BaseIdleW=75`, `PMaxW=460`, `AlphaUtil=1.2`, `BetaFreq=1.35`, `FMin/FMax=1200/3400`

Variable meaning:

- `BaseIdleW`: modeled CPU package idle power floor (Watts)
- `PMaxW`: modeled package power at full dynamic load before capping (Watts)
- `AlphaUtil`: exponent controlling how strongly power grows with utilization
- `BetaFreq`: exponent controlling how strongly power grows with frequency scale
- `FMin/FMax`: min/max CPU frequency bounds used to derive feasible frequency scale

Full power-model details are documented in:

- [Simulator Algorithms]({{< relref "/docs/simulator/simulator-algorithms.md" >}})

### Run configuration

- seeds: `3`
- jobs per seed: `300`
- mean inter-arrival: `0.20s`
- timeout: `1800s`
- time-scale: `60`
- workload mix:
  - `20%` performance-affinity
  - `30%` eco-affinity
  - `50%` no affinity (general)

Per-seed canonical workload class counts:

- seed 1: performance `63`, eco `94`, general `143`
- seed 2: performance `72`, eco `72`, general `156`
- seed 3: performance `55`, eco `93`, general `152`

## Algorithms used

### Controller policies

- `static_partition`:
  - `hpCount = round(N * STATIC_HP_FRAC)`
  - first `hpCount` nodes => performance, others => eco
- `queue_aware_v1`:
  - `baseCount = round(N * QUEUE_HP_BASE_FRAC)`
  - `queueNeed = ceil(perfIntentPods / QUEUE_PERF_PER_HP_NODE)`
  - `hpCount = clamp(max(baseCount, queueNeed), hpMin, hpMax, N)`
- downgrade guard:
  - `performance -> eco` deferred if performance-sensitive pods still run on node
  - node marked `draining-performance` until safe

### Simulator energy and slowdown model

Per-node power:

- `P = BaseIdleW + (PMaxW - BaseIdleW) * util^AlphaUtil * freqScale^BetaFreq`

Then cap/DVFS constraints are applied (RAPL cap range, frequency ramp, min feasible frequency, saturation flag).

Energy integration:

- at each tick: `E += P * dt`
- report uses simulator-integrated energy (`/debug/energy`) scaled by benchmark `time_scale`

Job progress and slowdown:

- `speed = reqCPUCores * baseSpeedPerCore * (1 - (1-freqScale)*sensitivityCPU)`
- `cpuUnitsRemaining -= speed * dt / max(1, concurrentJobsOnNode)`
- throttling lowers `freqScale`, reducing effective speed and increasing completion time

## Results summary

Primary metrics are in:

- <a href='{{< relURL "data/experiments/01-kwok-benchmark/summary.csv" >}}'>summary.csv</a>

Baseline means from the dataset:

| Baseline | Mean wall time (s) | Mean throughput (jobs/sim-hour) | Mean energy (kWh sim) | Mean cluster power (W) |
|---|---:|---:|---:|---:|
| A | 1802.57 | 9.9858 | 65.1705 | 2169.25 |
| B | 1802.40 | 9.9867 | 60.2284 | 2004.94 |
| C | 1802.47 | 9.9863 | 62.1140 | 2067.63 |

Relative to A:

- B: energy `-7.58%`, throughput/makespan effectively unchanged
- C: energy `-4.69%`, throughput/makespan effectively unchanged

## Plot commentary

### Runtime distribution

<img src='{{< relURL "images/experiments/01-kwok-benchmark/runtime_distribution.png" >}}' alt="Runtime Distribution by Baseline">

- Baselines overlap almost completely in wall-time distribution.

### Energy vs makespan

<img src='{{< relURL "images/experiments/01-kwok-benchmark/energy_vs_makespan.png" >}}' alt="Energy vs Makespan">

- `B` is consistently lower-energy than `A` with near-identical makespan.
- `C` is more variable; one seed is close to `A`.

### Baseline means

<img src='{{< relURL "images/experiments/01-kwok-benchmark/baseline_means.png" >}}' alt="Baseline Mean Metrics">

- Energy is the main differentiator; throughput/makespan are nearly flat.

## Best-fit use case indicated by this data

The strongest observed benefit is:

- **energy reduction with negligible throughput penalty** in mixed workload clusters.

In this experiment, `static_partition` is the most robust policy (best and most stable energy reduction), making it a good first choice when operators need predictable savings without visible scheduling-performance impact.

## Implementation details and scripts

Detailed implementation (manifests, scripts, raw outputs) is in the repository:

- Experiment folder:
  - https://github.com/joulie-k8s/Joulie/tree/main/experiments/01-kwok-benchmark
- Main scripts:
  - https://github.com/joulie-k8s/Joulie/blob/main/experiments/01-kwok-benchmark/scripts/05_sweep.py
  - https://github.com/joulie-k8s/Joulie/blob/main/experiments/01-kwok-benchmark/scripts/06_collect.py
  - https://github.com/joulie-k8s/Joulie/blob/main/experiments/01-kwok-benchmark/scripts/07_plot.py
- Full report markdown source:
  - https://github.com/joulie-k8s/Joulie/blob/main/experiments/01-kwok-benchmark/REPORT.md
