---
title: "Homogeneous H100 NVL Benchmark"
---

Three baselines on a KWOK cluster of 33 H100 NVL nodes (8 GPUs each) and 8 CPU-only nodes, measured at regime, after the ramp-up and before the drain. The full report, with every run and figure, is [`experiments/03-homogeneous-h100-benchmark/REPORT.md`](https://github.com/joulie-k8s/Joulie/blob/main/experiments/03-homogeneous-h100-benchmark/REPORT.md); how the runs are set up and sized is in [`experiments/README.md`](https://github.com/joulie-k8s/Joulie/blob/main/experiments/README.md).

- `A`: the simulator only, no power management.
- `B`: Joulie with the static partition policy (27 of 41 performance nodes).
- `C`: Joulie with the queue-aware policy (16 to 41 performance nodes, 23 to 25 on average).

## Result

Change against A, paired by seed (seeds 1 and 2), over 120 to 900 wall seconds at time scale 120:

| Baseline | GPU-node energy per GPU unit | GPU work done | Backlog at end | Pending performance jobs (A: 0.6) |
|---|---:|---:|---:|---:|
| B | -7.3 % / -9.4 % | -10.8 % / -7.4 % | +51.8 % / +27.1 % | 10.5 / 8.0 |
| C | -8.2 % / -9.6 % | -9.9 % / -8.6 % | +45.7 % / +31.2 % | 8.5 / 10.8 |

B and C used 7 to 10 % less GPU-node energy per unit of GPU work than A, with the eco GPUs capped at 280 W, and they delayed work to do it: 7 to 11 % less GPU work over the window and a larger backlog, as performance jobs waited for performance nodes and jobs asking for 8 GPUs waited for whole nodes (7 to 10 on average, against 0.1 to 0.6 under A).

The same baseline run twice moved energy per GPU unit by 1.6 %: kube-scheduler breaks ties at random, so the jobs land on different nodes. B and C differ by less than that, so neither ranks above the other here.

Baseline A ran at a GPU utilization of 0.66 to 0.69. Every run passed its gates: no refused control, a mean host load of at most 10, and for A no sustained queue and at least 45 % utilization.
