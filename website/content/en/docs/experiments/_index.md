---
title: "Experiments"
linkTitle: "Experiments"
weight: 40
---

Reproducible experiments measuring what Joulie's policies do to power, energy per unit of work and waiting jobs, across cluster configurations and scheduling strategies.

These results come from the simulator, Joulie's test bed for its behaviour, not evidence of energy savings: its hardware behaviour is assumed or fitted, not measured ([Evidence-based evaluation](https://github.com/joulie-k8s/Joulie/milestone/1)).

- [CPU-Only Benchmark]({{< relref "cpu-only-benchmark.md" >}}): 40-node CPU cluster; at regime, B and C used 1.4 to 2.7 % more energy per unit of work than A for the same work, most of it on the EPYC 9375F nodes that get more of the work
- [Heterogeneous GPU Benchmark]({{< relref "heterogeneous-benchmark.md" >}}): 41-node mixed GPU cluster (5 families); at regime, B and C used 0.1 to 4.6 % more GPU-node energy per unit of GPU work than A, with more performance jobs waiting
- [Homogeneous H100 NVL Benchmark]({{< relref "homogeneous-h100-benchmark.md" >}}): 41-node cluster of 33 H100 NVL nodes and 8 CPU-only nodes; at regime, B and C drew 6 to 9 % less power and did 6 to 10 % less GPU work than A, at about the same energy per unit
- [Scoring Formula Validation]({{< relref "scoring-formula-validation.md" >}}): scheduling formula against bin packing on a 2,500-node Python model; not a valid result, to be redone ([#122](https://github.com/joulie-k8s/Joulie/issues/122))
- [Governor Exploration]({{< relref "governor-exploration.md" >}}): governor presets on emulated nodes; GPU `efficient` at 80 % saved 11 to 17 % energy at a p95 slowdown under 18 %, and no CPU preset passed on every driver; the cluster-level question needs measured hardware ([#106](https://github.com/joulie-k8s/Joulie/issues/106)) and a pilot ([#114](https://github.com/joulie-k8s/Joulie/issues/114))

Each campaign follows the protocol of [`experiments/README.md`](https://github.com/joulie-k8s/Joulie/tree/main/experiments/README.md). The images, seeds, measured window and the longer reading of each benchmark: [Benchmark experiments](https://github.com/joulie-k8s/Joulie/wiki/Benchmark-Experiments).
