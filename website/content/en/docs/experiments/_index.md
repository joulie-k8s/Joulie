---
title: "Experiments"
linkTitle: "Experiments"
weight: 40
---

Reproducible experiments validating Joulie's energy savings across different cluster configurations and scheduling strategies.

- [CPU-Only Benchmark]({{< relref "cpu-only-benchmark.md" >}}): 40-node CPU cluster, 21-29% power savings
- [Heterogeneous GPU Benchmark]({{< relref "heterogeneous-benchmark.md" >}}): 41-node mixed GPU cluster (5 families), 20-25% savings
- [Homogeneous H100 NVL Benchmark]({{< relref "homogeneous-h100-benchmark.md" >}}): 41-node identical H100 cluster, 20-25% savings
- [Scoring Formula Validation]({{< relref "scoring-formula-validation.md" >}}): Energy-aware scheduling formula tuning on 2,500-node cluster, 6.4% IT energy reduction
- [Governor Exploration]({{< relref "governor-exploration.md" >}}): CPU and GPU governor presets on emulated nodes, energy and time per preset, workload kind and cap, with the assumed parameters swept

The reports of the first four were produced with an older simulator model; each experiment's README says what changed since. The smallest configuration of every experiment was run again on 2026-10-23 with the 0.2.4 release, and still runs. The cluster experiments 01 to 03 deploy is in [`experiments/kwok-cluster`](https://github.com/joulie-k8s/Joulie/tree/main/experiments/kwok-cluster).
