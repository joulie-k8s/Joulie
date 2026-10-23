# Experiments

The measurements behind Joulie's claims, each with its scripts, configs and a
report. The [docs site](https://joulie-k8s.github.io/Joulie/main/docs/experiments/)
has a page per experiment. To learn Joulie, start with the
[examples](../examples/README.md) instead.

| Experiment | What it measures | How it runs |
|---|---|---|
| [01 CPU-only benchmark](01-cpu-only-benchmark/README.md) | Throughput and energy of baselines A (no Joulie), B (static partition) and C (queue-aware) on a CPU cluster | kind with KWOK fake nodes and the simulator in HTTP mode ([`kwok-cluster`](kwok-cluster/README.md)) |
| [02 Heterogeneous benchmark](02-heterogeneous-benchmark/README.md) | The same baselines on a mixed cluster of five GPU families and CPU-only nodes | as 01 |
| [03 Homogeneous H100 benchmark](03-homogeneous-h100-benchmark/README.md) | The same baselines on H100 NVL nodes only | as 01 |
| [04 Scoring formula validation](04-scoring-formula-validation/REPORT.md) | The scheduler extender's scoring formula against bin packing, with a Modelica FMU for the cooling | Python models of the cluster, no Joulie code |
| [05 Governor exploration](05-governor-exploration/README.md) | Energy and time per CPU and GPU governor preset, workload kind and cap | Tier A: emulated nodes in `govsweep`; Tier B: the simulated cluster |

The reports of 01 to 04 were produced with an older simulator model; each
README says what changed since. Every experiment still runs on the current
code: the smallest configuration of each was run for #89, as its README
records.
