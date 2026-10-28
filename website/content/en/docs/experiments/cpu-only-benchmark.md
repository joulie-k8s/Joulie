---
title: "CPU-Only Benchmark"
---

These results come from the Joulie simulator, Joulie's test bed for its behaviour (caps applied, placement, policies), not evidence of energy savings. Their energy figures rest on hardware behaviour the simulator assumes: every CPU core does the same work per second whatever its model, and cap responses, power curves and workload intensities are assumed or fitted, not measured on real nodes. They do not show what Joulie saves on real hardware; what meaningful experiments need is tracked in the milestone [Evidence-based evaluation](https://github.com/joulie-k8s/Joulie/milestone/1).

This page summarizes the CPU-only cluster benchmark (KWOK, 40 nodes); the full report, with every table and its sources, is [`experiments/01-cpu-only-benchmark/REPORT.md`](https://github.com/joulie-k8s/Joulie/tree/main/experiments/01-cpu-only-benchmark/REPORT.md).

Three baselines, measured at regime (after the ramp-up and before the drain), 2 seeds and a repeat of the first run, on 2026-10-28:

- `A`: the simulator only, no power management.
- `B`: Joulie with the static partition policy (19 of 40 nodes performance).
- `C`: Joulie with the queue-aware policy.

## Setup

| Nodes | Count | CPU | Cores |
|---|---:|---|---:|
| `kwok-cpu-highcore` | 10 | 2 x AMD EPYC 9965 | 384 |
| `kwok-cpu-highfreq` | 10 | 2 x AMD EPYC 9375F | 64 |
| `kwok-cpu-intensive` | 20 | 2 x AMD EPYC 9655 | 192 |

- **Power models.** The 9655 and 9965 run their SPECpower curves; the 9375F, with no published curve, runs the generic model at sockets x TDP. A cap lowers the frequency, and the dynamic power falls with `f^1.43` (`SIM_BETA_FREQ`, fitted to published caps; see [Hardware modeling]({{< relref "/docs/hardware/hardware-modeling.md" >}})).
- **Caps.** Eco nodes are capped at 60 % of sockets x TDP: 480, 600 and 384 W per node.
- **Load.** The CPU utilization is 0.55 to 0.58 under A.

## Results

| Baseline | Energy per CPU unit against A, seed 1 / 2 | Avg IT power against A | CPU work done against A |
|---|---:|---:|---:|
| B (static) | +2.7 / +1.4 % | +2.7 / +1.4 % | +0.0 / -0.0 % |
| C (queue-aware) | +2.1 / +2.5 % | +1.8 / +2.5 % | -0.3 / -0.0 % |

A's repeat differed by 0.24 % in energy per unit.

## Interpretation

B and C did the same work as A and used 1.4 to 2.7 % more energy per unit, in all four pairs. A's repeat differed by 0.24 %; B's and C's own noise was not measured ([#113](https://github.com/joulie-k8s/Joulie/issues/113)).

An accounting split by node family puts most of the difference on the EPYC 9375F family. Under B and C it does more of the work, and its dynamic energy per unit rises as that work turns compute-bound: B and C send performance jobs, mostly compute-bound, to performance nodes, which include half of the 9375F nodes.

In the simulator every core does the same work, so the 9375F's higher clock buys nothing while its power per core is several times the others' ([#107](https://github.com/joulie-k8s/Joulie/issues/107) tracks per-device speed). The eco caps bind in about a third of the eco 9655 nodes' control intervals, up to a sixth on the 9375F and almost never on the 9965. Separating the caps from the placement would take a run with the caps off ([#113](https://github.com/joulie-k8s/Joulie/issues/113)). These results rest on simulator assumptions and do not show what B and C would do on real hardware.

## Plots

{{< img src="images/experiments/01-cpu-only-benchmark/timeseries.png" alt="Power, utilization and running pods over the window" >}}

{{< img src="images/experiments/01-cpu-only-benchmark/energy_comparison.png" alt="Energy Comparison" >}}

{{< img src="images/experiments/01-cpu-only-benchmark/cumulative_energy.png" alt="Cumulative Energy" >}}

{{< img src="images/experiments/01-cpu-only-benchmark/utilization_summary.png" alt="Utilization Summary" >}}

{{< img src="images/experiments/01-cpu-only-benchmark/pue_analysis.png" alt="PUE Analysis" >}}

{{< img src="images/experiments/01-cpu-only-benchmark/facility_power_breakdown.png" alt="Facility Power Breakdown" >}}
