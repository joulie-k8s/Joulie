---
title: "Experiments"
linkTitle: "Experiments"
weight: 40
---

Reproducible experiments measuring what Joulie's policies do to power, energy per unit of work and waiting jobs, across cluster configurations and scheduling strategies.

These results come from the simulator, Joulie's test bed for its behaviour (caps applied, placement, policies, governors), not evidence of energy savings. Their energy figures rest on hardware behaviour the simulator assumes: every CPU core and every GPU does the same work per second whatever its model, and cap responses, power curves and workload intensities are assumed or fitted, not measured on our nodes. What meaningful experiments need is tracked in the milestone [Evidence-based evaluation](https://github.com/joulie-k8s/Joulie/milestone/1).

- [CPU-Only Benchmark]({{< relref "cpu-only-benchmark.md" >}}): 40-node CPU cluster; at regime, B and C used 1.4 to 2.7 % more energy per unit of work than A for the same work, most of it on the EPYC 9375F nodes that get more of the work
- [Heterogeneous GPU Benchmark]({{< relref "heterogeneous-benchmark.md" >}}): 41-node mixed GPU cluster (5 families); at regime, B and C used 0.1 to 4.6 % more GPU-node energy per unit of GPU work than A, with more performance jobs waiting
- [Homogeneous H100 NVL Benchmark]({{< relref "homogeneous-h100-benchmark.md" >}}): 41-node identical H100 cluster; at regime, B and C drew 6 to 9 % less power and did 6 to 10 % less GPU work than A, at about the same energy per unit
- [Scoring Formula Validation]({{< relref "scoring-formula-validation.md" >}}): Energy-aware scheduling formula tuning on a 2,500-node cluster; not a valid result (see the page); it is to be redone on the extracted scheduling library ([#122](https://github.com/joulie-k8s/Joulie/issues/122)) as a behaviour scenario ([#115](https://github.com/joulie-k8s/Joulie/issues/115))
- [Governor Exploration]({{< relref "governor-exploration.md" >}}): CPU and GPU governor presets on emulated nodes, energy and time per preset, workload kind and cap, with the assumed parameters swept; Tier A runs on emulated nodes, and the cluster-level question (Q4) needs measured hardware behaviour ([#106](https://github.com/joulie-k8s/Joulie/issues/106)) and a pilot on a real cluster ([#114](https://github.com/joulie-k8s/Joulie/issues/114))

Experiments 01 to 03 run with the images `rerun-2026-10-28`, 2 seeds and a repeat of the first run each, measured at regime; [`experiments/README.md`](https://github.com/joulie-k8s/Joulie/tree/main/experiments/README.md) gives the method and the gates. Experiment 04 (scoring formula) is not a valid result; experiment 05's Tier A, its sensitivity sweep and its receiver gate runs ran on 2026-10-28 with `govsweep` on emulated nodes.
