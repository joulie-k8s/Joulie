---
title: "CPU-Only Benchmark"
---

What Joulie's policies do to energy per unit of work on a 40-node CPU cluster (KWOK). These results come from the Joulie simulator, a test bed for Joulie's behaviour, not evidence of energy savings: its hardware behaviour is assumed or fitted, not measured on real nodes ([Evidence-based evaluation](https://github.com/joulie-k8s/Joulie/milestone/1)).

Full report: [`experiments/01-cpu-only-benchmark/REPORT.md`](https://github.com/joulie-k8s/Joulie/tree/main/experiments/01-cpu-only-benchmark/REPORT.md).

## Setup

| Nodes | Count | CPU | Cores | Eco cap |
|---|---:|---|---:|---:|
| `kwok-cpu-highcore` | 10 | 2 x AMD EPYC 9965 | 384 | 600 W |
| `kwok-cpu-highfreq` | 10 | 2 x AMD EPYC 9375F | 64 | 384 W |
| `kwok-cpu-intensive` | 20 | 2 x AMD EPYC 9655 | 192 | 480 W |

| Item | Value |
|---|---|
| Baselines | A: simulator only, no power management; B: static partition (19 of 40 nodes performance); C: queue-aware |
| Power models | SPECpower curves (9655, 9965); analytic model at sockets x TDP (9375F) |
| Eco cap | 60 % of sockets x TDP |
| Load | CPU utilization 0.55 to 0.58 under A |
| Runs | 2026-10-28, 2 seeds and a repeat of the first run, measured at regime |

## Results

| Baseline | Energy per CPU unit against A, seed 1 / 2 | Avg IT power against A | CPU work done against A |
|---|---:|---:|---:|
| B (static) | +2.7 / +1.4 % | +2.7 / +1.4 % | +0.0 / -0.0 % |
| C (queue-aware) | +2.1 / +2.5 % | +1.8 / +2.5 % | -0.3 / -0.0 % |

A's repeat differed by 0.24 % in energy per unit.

## Interpretation

B and C did the same work as A and used 1.4 to 2.7 % more energy per unit, in all four pairs. Most of the difference is on the EPYC 9375F nodes, which do more of the compute-bound work under B and C. In the simulator every core does the same work, so the 9375F's higher clock buys nothing ([#107](https://github.com/joulie-k8s/Joulie/issues/107)), and these results do not show what B and C would do on real hardware.

## Plots

{{< img src="images/experiments/01-cpu-only-benchmark/timeseries.png" alt="Power, utilization and running pods over the window" >}}

{{< img src="images/experiments/01-cpu-only-benchmark/energy_comparison.png" alt="Energy Comparison" >}}

{{< img src="images/experiments/01-cpu-only-benchmark/cumulative_energy.png" alt="Cumulative Energy" >}}

{{< img src="images/experiments/01-cpu-only-benchmark/utilization_summary.png" alt="Utilization Summary" >}}

{{< img src="images/experiments/01-cpu-only-benchmark/pue_analysis.png" alt="PUE Analysis" >}}

{{< img src="images/experiments/01-cpu-only-benchmark/facility_power_breakdown.png" alt="Facility Power Breakdown" >}}

Setup rationale and the full reading: [Benchmark experiments](https://github.com/joulie-k8s/Joulie/wiki/Benchmark-Experiments#01-cpu-only-benchmark).
