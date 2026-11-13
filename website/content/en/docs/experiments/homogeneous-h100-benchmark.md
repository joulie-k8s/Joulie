---
title: "Homogeneous H100 NVL Benchmark"
---

What Joulie's policies do to power and energy per unit of GPU work on a 41-node cluster of H100 NVL nodes (KWOK). These results come from the Joulie simulator, a test bed for Joulie's behaviour, not evidence of energy savings: its hardware behaviour is assumed or fitted, not measured on real nodes ([Evidence-based evaluation](https://github.com/joulie-k8s/Joulie/milestone/1)).

Full report: [`experiments/03-homogeneous-h100-benchmark/REPORT.md`](https://github.com/joulie-k8s/Joulie/tree/main/experiments/03-homogeneous-h100-benchmark/REPORT.md).

## Setup

| Item | Value |
|---|---|
| Nodes | 33 H100 NVL nodes (8 GPUs each) and 8 CPU-only nodes |
| Baselines | A: simulator only, no power management; B: static partition (27 of 41 nodes performance); C: queue-aware |
| Eco cap | 280 W, 70 % of 400 W |
| Cap response | `computeGamma` 0.50, the H100 PCIe's published response, since the NVL has no published data ([#106](https://github.com/joulie-k8s/Joulie/issues/106)) |
| Load | GPU utilization 0.55 to 0.58 under A |
| Runs | 2026-10-28, 2 seeds and a repeat of the first run, measured at regime |

## Results

| Baseline | GPU-node energy per GPU unit against A, seed 1 / 2 | Avg IT power against A | GPU work done against A | Pending performance jobs (A: 0.7) |
|---|---:|---:|---:|---:|
| B (static) | +0.5 / -0.2 % | -5.7 / -7.1 % | -6.5 / -7.5 % | 9.2 |
| C (queue-aware) | +0.3 / +0.7 % | -5.8 / -8.8 % | -6.4 / -10.0 % | 9.8 |

A's repeat differed by 0.01 % in energy per unit. B and C were not repeated, so their own noise is unknown ([#113](https://github.com/joulie-k8s/Joulie/issues/113)).

## Interpretation

B and C traded power for throughput: 6 to 9 % less power and 6 to 10 % less GPU work, with more performance jobs waiting, at about the same energy per unit of work. The capped GPUs' lower dynamic energy per unit is cancelled by the GPU nodes' idle power spread over less work. On the 8 CPU-only nodes energy per unit rose by 3 to 7 %, as in the [CPU-only benchmark]({{< relref "cpu-only-benchmark.md" >}}).

## Plots

{{< img src="images/experiments/03-homogeneous-h100-benchmark/timeseries.png" alt="Power, utilization and running pods over the window" >}}

{{< img src="images/experiments/03-homogeneous-h100-benchmark/energy_comparison.png" alt="Energy Comparison" >}}

{{< img src="images/experiments/03-homogeneous-h100-benchmark/cumulative_energy.png" alt="Cumulative Energy" >}}

{{< img src="images/experiments/03-homogeneous-h100-benchmark/utilization_summary.png" alt="Utilization Summary" >}}

{{< img src="images/experiments/03-homogeneous-h100-benchmark/pue_analysis.png" alt="PUE Analysis" >}}

{{< img src="images/experiments/03-homogeneous-h100-benchmark/facility_power_breakdown.png" alt="Facility Power Breakdown" >}}

Setup rationale and the full reading: [Benchmark experiments](https://github.com/joulie-k8s/Joulie/wiki/Benchmark-Experiments#03-homogeneous-h100-nvl-benchmark).
