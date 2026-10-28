---
title: "Heterogeneous GPU Cluster Benchmark"
---

These results come from the Joulie simulator, Joulie's test bed for its behaviour (caps applied, placement, policies), not evidence of energy savings. Their energy figures rest on hardware behaviour the simulator assumes: every CPU core and every GPU does the same work per second whatever its model, and cap responses, power curves and workload intensities are assumed or fitted, not measured on real nodes. They do not show what Joulie saves on real hardware; what meaningful experiments need is tracked in the milestone [Evidence-based evaluation](https://github.com/joulie-k8s/Joulie/milestone/1).

This page summarizes the heterogeneous GPU cluster benchmark (KWOK, 41 nodes); the full report, with every table and its sources, is [`experiments/02-heterogeneous-benchmark/REPORT.md`](https://github.com/joulie-k8s/Joulie/tree/main/experiments/02-heterogeneous-benchmark/REPORT.md).

Three baselines, measured at regime (after the ramp-up and before the drain), 2 seeds and a repeat of the first run, on 2026-10-28:

- `A`: the simulator only, no power management.
- `B`: Joulie with the static partition policy (31 of 41 nodes performance).
- `C`: Joulie with the queue-aware policy.

## Setup

| Nodes | Count | GPUs per node |
|---|---:|---|
| `kwok-h100-nvl` | 12 | 8 x NVIDIA H100 NVL |
| `kwok-h100-sxm` | 6 | 4 x NVIDIA H100 80GB HBM3 |
| `kwok-l40s` | 7 | 4 x NVIDIA L40S |
| `kwok-mi300x` | 2 | 8 x AMD Instinct MI300X |
| `kwok-w7900` | 6 | 4 x AMD Radeon PRO W7900 |
| CPU-only (EPYC 9965, 9375F, 9655) | 8 | none |

- **Caps.** Eco GPUs are capped at 70 % of their maximum.
- **Cap response.** Each family's throughput under a cap follows an exponent fitted to published throughput at several power limits (H100 SXM 0.43, H100 NVL 0.50, MI300X 0.62, L40S 1.20; the W7900, with no data, 1.0); see [Hardware modeling]({{< relref "/docs/hardware/hardware-modeling.md" >}}).
- **Load.** The GPU utilization is 0.46 to 0.49 under A.

## Results

| Baseline | GPU-node energy per GPU unit against A, seed 1 / 2 | Avg IT power against A | GPU work done against A | Pending performance jobs (A: 0.7) |
|---|---:|---:|---:|---:|
| B (static) | +0.1 / +4.6 % | +3.6 / +3.0 % | +3.6 / -1.4 % | 3.5 |
| C (queue-aware) | +1.2 / +2.9 % | -1.0 / +0.8 % | -2.3 / -2.1 % | 5.8 |

A's repeat differed by 0.56 % in energy per unit.

## Interpretation

B and C did not lower GPU-node energy per unit of work at this load, in all four pairs, and they kept more performance jobs waiting. A's repeat differed by 0.56 %; B's and C's own noise was not measured; repeats for every arm are tracked in [#113](https://github.com/joulie-k8s/Joulie/issues/113).

An accounting split by GPU family puts the difference on placement: under B and C more of the GPU work runs on the MI300X and the H100 NVL, which cost more per unit in the simulator. Against that, the H100 NVL's own dynamic energy per unit falls by 1.7 to 2.8 points.

The per-family costs differ because the simulator gives every GPU the same speed, whatever its power; on real hardware the faster GPUs do more work. Per-device speed is tracked in [#107](https://github.com/joulie-k8s/Joulie/issues/107). Separating the caps from the placement would take a run with the caps off, tracked in [#113](https://github.com/joulie-k8s/Joulie/issues/113).

## Plots

{{< img src="images/experiments/02-heterogeneous-benchmark/timeseries.png" alt="Power, utilization and running pods over the window" >}}

{{< img src="images/experiments/02-heterogeneous-benchmark/energy_comparison.png" alt="Energy Comparison" >}}

{{< img src="images/experiments/02-heterogeneous-benchmark/cumulative_energy.png" alt="Cumulative Energy" >}}

{{< img src="images/experiments/02-heterogeneous-benchmark/utilization_summary.png" alt="Utilization Summary" >}}

{{< img src="images/experiments/02-heterogeneous-benchmark/pue_analysis.png" alt="PUE Analysis" >}}

{{< img src="images/experiments/02-heterogeneous-benchmark/facility_power_breakdown.png" alt="Facility Power Breakdown" >}}
