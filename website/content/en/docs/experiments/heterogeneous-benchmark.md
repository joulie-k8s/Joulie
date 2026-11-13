---
title: "Heterogeneous GPU Cluster Benchmark"
---

What Joulie's policies do to energy per unit of GPU work on a 41-node cluster of five GPU families (KWOK). These results come from the Joulie simulator, a test bed for Joulie's behaviour, not evidence of energy savings: its hardware behaviour is assumed or fitted, not measured on real nodes ([Evidence-based evaluation](https://github.com/joulie-k8s/Joulie/milestone/1)).

Full report: [`experiments/02-heterogeneous-benchmark/REPORT.md`](https://github.com/joulie-k8s/Joulie/tree/main/experiments/02-heterogeneous-benchmark/REPORT.md).

## Setup

| Nodes | Count | GPUs per node |
|---|---:|---|
| `kwok-h100-nvl` | 12 | 8 x NVIDIA H100 NVL |
| `kwok-h100-sxm` | 6 | 4 x NVIDIA H100 80GB HBM3 |
| `kwok-l40s` | 7 | 4 x NVIDIA L40S |
| `kwok-mi300x` | 2 | 8 x AMD Instinct MI300X |
| `kwok-w7900` | 6 | 4 x AMD Radeon PRO W7900 |
| CPU-only (EPYC 9965, 9375F, 9655) | 8 | none |

| Item | Value |
|---|---|
| Baselines | A: simulator only, no power management; B: static partition (31 of 41 nodes performance); C: queue-aware |
| Eco cap | 70 % of each GPU's maximum |
| Cap response | exponent fitted per family to published data: H100 SXM 0.43, H100 NVL 0.50, MI300X 0.62, L40S 1.20, W7900 1.0 (no data) |
| Load | GPU utilization 0.46 to 0.49 under A |
| Runs | 2026-10-28, 2 seeds and a repeat of the first run, measured at regime |

## Results

| Baseline | GPU-node energy per GPU unit against A, seed 1 / 2 | Avg IT power against A | GPU work done against A | Pending performance jobs (A: 0.7) |
|---|---:|---:|---:|---:|
| B (static) | +0.1 / +4.6 % | +3.6 / +3.0 % | +3.6 / -1.4 % | 3.5 |
| C (queue-aware) | +1.2 / +2.9 % | -1.0 / +0.8 % | -2.3 / -2.1 % | 5.8 |

A's repeat differed by 0.56 % in energy per unit.

## Interpretation

B and C did not lower GPU-node energy per unit of work at this load, in all four pairs, and kept more performance jobs waiting. The difference comes from placement: more of the GPU work runs on the MI300X and the H100 NVL, which cost more per unit in the simulator. The simulator gives every GPU the same speed whatever its power ([#107](https://github.com/joulie-k8s/Joulie/issues/107)), so these per-family costs are not those of real hardware.

## Plots

{{< img src="images/experiments/02-heterogeneous-benchmark/timeseries.png" alt="Power, utilization and running pods over the window" >}}

{{< img src="images/experiments/02-heterogeneous-benchmark/energy_comparison.png" alt="Energy Comparison" >}}

{{< img src="images/experiments/02-heterogeneous-benchmark/cumulative_energy.png" alt="Cumulative Energy" >}}

{{< img src="images/experiments/02-heterogeneous-benchmark/utilization_summary.png" alt="Utilization Summary" >}}

{{< img src="images/experiments/02-heterogeneous-benchmark/pue_analysis.png" alt="PUE Analysis" >}}

{{< img src="images/experiments/02-heterogeneous-benchmark/facility_power_breakdown.png" alt="Facility Power Breakdown" >}}

Setup rationale and the full reading: [Benchmark experiments](https://github.com/joulie-k8s/Joulie/wiki/Benchmark-Experiments#02-heterogeneous-gpu-benchmark).
