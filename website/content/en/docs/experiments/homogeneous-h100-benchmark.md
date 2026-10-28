---
title: "Homogeneous H100 NVL Benchmark"
---

This page summarizes the homogeneous H100 NVL benchmark (KWOK, 41 nodes); the full report, with every table and its sources, is [`experiments/03-homogeneous-h100-benchmark/REPORT.md`](https://github.com/joulie-k8s/Joulie/tree/main/experiments/03-homogeneous-h100-benchmark/REPORT.md).

Three baselines on 33 H100 NVL nodes (8 GPUs each) and 8 CPU-only nodes, measured at regime (after the ramp-up and before the drain), 2 seeds and a repeat of the first run, on 2026-10-28:

- `A`: the simulator only, no power management.
- `B`: Joulie with the static partition policy (27 of 41 nodes performance).
- `C`: Joulie with the queue-aware policy.

Eco GPUs are capped at 280 W, 70 % of 400 W. The H100 NVL's throughput under a cap follows the H100 PCIe's published response, `computeGamma` 0.50, since the NVL has no published data ([Hardware modeling]({{< relref "/docs/hardware/hardware-modeling.md" >}})). The GPU utilization is 0.55 to 0.58 under A.

## Results

| Baseline | GPU-node energy per GPU unit against A, seed 1 / 2 | Avg IT power against A | GPU work done against A | Pending performance jobs (A: 0.7) |
|---|---:|---:|---:|---:|
| B (static) | +0.5 / -0.2 % | -5.7 / -7.1 % | -6.5 / -7.5 % | 9.2 |
| C (queue-aware) | +0.3 / +0.7 % | -5.8 / -8.8 % | -6.4 / -10.0 % | 9.8 |

A's repeat differed by 0.01 % in energy per unit.

## Interpretation

B and C traded power for throughput: they drew 6 to 9 % less power and did 6 to 10 % less GPU work, with more performance jobs waiting, at about the same energy per unit of work.

An accounting split finds two parts that cancel: the H100 NVL's dynamic energy per unit falls by 1.6 to 2.6 points, where its eco GPUs are capped, while the GPU nodes' idle power, spread over less work, adds 2.3 to 3.5. On the 8 CPU-only nodes energy per unit rose by 3 to 7 %, as in the [CPU-only benchmark]({{< relref "cpu-only-benchmark.md" >}}).

## Plots

{{< img src="images/experiments/03-homogeneous-h100-benchmark/timeseries.png" alt="Power, utilization and running pods over the window" >}}

{{< img src="images/experiments/03-homogeneous-h100-benchmark/energy_comparison.png" alt="Energy Comparison" >}}

{{< img src="images/experiments/03-homogeneous-h100-benchmark/cumulative_energy.png" alt="Cumulative Energy" >}}

{{< img src="images/experiments/03-homogeneous-h100-benchmark/utilization_summary.png" alt="Utilization Summary" >}}

{{< img src="images/experiments/03-homogeneous-h100-benchmark/pue_analysis.png" alt="PUE Analysis" >}}

{{< img src="images/experiments/03-homogeneous-h100-benchmark/facility_power_breakdown.png" alt="Facility Power Breakdown" >}}
