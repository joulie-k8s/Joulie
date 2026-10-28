---
title: "Experiments"
linkTitle: "Experiments"
weight: 40
---

Reproducible experiments measuring what Joulie's policies do to power, energy per unit of work and waiting jobs, across cluster configurations and scheduling strategies.

- [CPU-Only Benchmark]({{< relref "cpu-only-benchmark.md" >}}): 40-node CPU cluster; at regime, B and C used 1.4 to 2.7 % more energy per unit of work than A for the same work, most of it on the EPYC 9375F nodes that get more of the work
- [Heterogeneous GPU Benchmark]({{< relref "heterogeneous-benchmark.md" >}}): 41-node mixed GPU cluster (5 families); at regime, B and C used 0.1 to 4.6 % more GPU-node energy per unit of GPU work than A, with more performance jobs waiting
- [Homogeneous H100 NVL Benchmark]({{< relref "homogeneous-h100-benchmark.md" >}}): 41-node identical H100 cluster; at regime, B and C drew 6 to 9 % less power and did 6 to 10 % less GPU work than A, at about the same energy per unit
- [Scoring Formula Validation]({{< relref "scoring-formula-validation.md" >}}): Energy-aware scheduling formula tuning on a 2,500-node cluster; not a valid result yet (see the page), being redone
- [Governor Exploration]({{< relref "governor-exploration.md" >}}): CPU and GPU governor presets on emulated nodes, energy and time per preset, workload kind and cap, with the assumed parameters swept; Tier A is being rerun after simulator changes (see the page)

Experiments 01 to 03 ran on 2026-10-28 with the images `rerun-2026-10-28`, 2 seeds and a repeat of the first run each, measured at regime; [`experiments/README.md`](https://github.com/joulie-k8s/Joulie/tree/main/experiments/README.md) gives the method and the gates. Experiment 04 (scoring formula) is not a valid result yet and is being redone; experiment 05's Tier A predates later simulator changes and is being rerun.
