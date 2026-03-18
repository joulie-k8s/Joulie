---
title: "CPU-Only Benchmark"
---

This page reports results from the CPU-only cluster benchmark experiment:

- [`experiments/01-cpu-only-benchmark/`](https://github.com/joulie-k8s/Joulie/tree/main/experiments/01-cpu-only-benchmark)

## Scope

The benchmark compares three baselines on a pure CPU cluster:

- `A`: simulator only (Joulie-free)
- `B`: Joulie with static partition policy
- `C`: Joulie with queue-aware policy

It evaluates energy and throughput under real Kubernetes scheduling with [KWOK](https://kwok.sigs.k8s.io/) nodes and simulated power control.

## Experimental setup

### Cluster and nodes

- [kind](https://kind.sigs.k8s.io/) control-plane + worker (real control plane)
- 8 managed [KWOK](https://kwok.sigs.k8s.io/) nodes - **CPU only, no GPUs**
- Workload pods target KWOK nodes via selector + toleration

### Node inventory

| Node prefix | Count | CPU model | CPU cores | RAM |
|---|---:|---|---:|---:|
| kwok-cpu-highcore | 2 | AMD EPYC 9965 192-Core | 384 (2x192) | 1536 GiB |
| kwok-cpu-highfreq | 2 | AMD EPYC 9375F 32-Core | 64 (2x32) | 770 GiB |
| kwok-cpu-intensive | 4 | AMD EPYC 9655 96-Core | 192 (2x96) | 1536 GiB |

**Total: 8 nodes, 2304 CPU cores, 0 GPUs.**

### Hardware models in simulator

CPU power per node:

```
P(u, f) = IdleW + (PeakW - IdleW) * u^AlphaUtil * f^BetaFreq
```

| CPU family | IdleW (W) | PeakW (W) | AlphaUtil | BetaFreq |
|---|---:|---:|---:|---:|
| AMD EPYC 9965 192-Core | 120 | 960 | 1.15 | 1.30 |
| AMD EPYC 9375F 32-Core | 60 | 480 | 1.10 | 1.25 |
| AMD EPYC 9655 96-Core | 95 | 760 | 1.12 | 1.28 |

Full power-model details: [Power Simulator]({{< relref "/docs/simulator/power-simulator.md" >}})

### Run configuration

- Seeds: `3`
- Jobs: `300`
- Mean inter-arrival: `0.20 s`
- Time scale: `60x`
- Timeout: `1800 s`
- Perf ratio: `20%`, GPU ratio: `0%`
- Workload types: `cpu_preprocess`, `cpu_analytics`
- Policy caps: CPU eco at `65%` of peak

## Algorithms used

### Controller policies

- `static_partition`:
  - `hpCount = round(N * 0.30)` -> 2 performance nodes, 6 eco nodes
- `queue_aware_v1`:
  - `baseCount = round(N * 0.30)`, dynamic from live perf-pod count
  - `hpCount = clamp(max(baseCount, queueNeed), 2, 15, N)`
- Downgrade guard: `performance -> eco` deferred while performance-sensitive pods still run on node

## Results summary

### Per-seed results

| Baseline | Seed | Wall (s) | Throughput (jobs/sim-hr) | Energy (kWh sim) | Avg power (W) |
|---|---:|---:|---:|---:|---:|
| A | 1 | 317.98 | 113.21 | 17.63 | 3326 |
| A | 2 | 276.18 | 130.35 | 15.01 | 3261 |
| A | 3 | 239.74 | 150.17 | 13.25 | 3315 |
| B | 1 | 330.14 | 109.04 | 12.22 | 2221 |
| B | 2 | 275.86 | 130.50 | 10.10 | 2197 |
| B | 3 | 240.20 | 149.87 | 8.98 | 2242 |
| C | 1 | 328.92 | 109.45 | 12.25 | 2235 |
| C | 2 | 275.26 | 130.78 | 9.99 | 2177 |
| C | 3 | 239.66 | 150.21 | 9.02 | 2259 |

### Baseline means (3 seeds, all completed)

| Baseline | Mean wall (s) | Mean throughput (jobs/sim-hr) | Mean energy (kWh sim) | Mean cluster power (W) |
|---|---:|---:|---:|---:|
| A | 278.0 | 131.24 | 15.30 | 3301 |
| B | 282.1 | 129.80 | 10.43 | 2220 |
| C | 281.3 | 130.15 | 10.42 | 2224 |

Relative to A:

- B: energy **-31.8%**, throughput -1.1% (negligible)
- C: energy **-31.9%**, throughput -0.8% (negligible)

## Plot commentary

### Runtime distribution

{{< img src="images/experiments/01-cpu-only-benchmark/runtime_distribution.png" alt="Runtime Distribution by Baseline" >}}

- All three baselines complete in nearly identical wall-time windows.
- Run-to-run seed jitter is larger than any inter-baseline difference.

### Energy vs makespan

{{< img src="images/experiments/01-cpu-only-benchmark/energy_vs_makespan.png" alt="Energy vs Makespan" >}}

- B and C are consistently lower-energy than A with near-identical makespan across all 3 seeds.
- Both Joulie baselines cluster tightly together.

### Baseline means

{{< img src="images/experiments/01-cpu-only-benchmark/baseline_means.png" alt="Baseline Mean Metrics" >}}

- Energy is the main differentiator; throughput and wall-time bars are indistinguishable.

### Completion summary

{{< img src="images/experiments/01-cpu-only-benchmark/completion_summary.png" alt="Completion Summary" >}}

- All 3 seeds completed for all baselines; no timeouts or gang-scheduling issues.

## Interpretation

Joulie reduces energy by ~32% without throughput penalty on a CPU-only cluster because:

1. The cluster is over-provisioned (2304 cores, lightweight jobs) - eco nodes have spare CPU cores to compensate for throttled frequency.
2. CPU `sensitivityCPU` for `cpu_preprocess`/`cpu_analytics` is moderate (0.7-0.9): a 35% frequency reduction causes 25-32% per-job slowdown, but job completion time stays flat because the scheduler redistributes load.
3. Eco nodes draw significantly less power for the same simulated duration -> energy falls without extending makespan.
4. The aggressive 65% eco cap maximizes power savings on eco nodes compared to milder caps.

## Best-fit use case

The strongest observed benefit is:

- **energy reduction (-31.8% static, -31.9% queue-aware) with negligible throughput penalty** in CPU-only mixed workload clusters.

Both policies perform equivalently on CPU-only clusters. `static_partition` is simpler to configure; `queue_aware_v1` becomes more valuable when the performance-sensitive fraction is larger or more bursty.

## Implementation details and scripts

- [Experiment folder](https://github.com/joulie-k8s/Joulie/tree/main/experiments/01-cpu-only-benchmark)
- Main scripts:
  - [05_sweep.py](https://github.com/joulie-k8s/Joulie/blob/main/experiments/01-cpu-only-benchmark/scripts/05_sweep.py)
  - [06_collect.py](https://github.com/joulie-k8s/Joulie/blob/main/experiments/01-cpu-only-benchmark/scripts/06_collect.py)
  - [07_plot.py](https://github.com/joulie-k8s/Joulie/blob/main/experiments/01-cpu-only-benchmark/scripts/07_plot.py)
- [Full report](https://github.com/joulie-k8s/Joulie/blob/main/experiments/01-cpu-only-benchmark/REPORT.md)
