---
title: "Workload Simulator"
weight: 22
---

This page says how the simulator runs a workload trace: which Kubernetes objects it creates and how caps slow each job. [Workload Generation]({{< relref "/docs/simulator/workload-generation.md" >}}) describes the traces; [Power Simulator]({{< relref "/docs/simulator/power-simulator.md" >}}) the power side of a node.

## Two engines: legacy and v1

`SIM_WORKLOAD_MODEL` selects the engine that runs a trace.

| Engine | Where | Started by | Kubernetes objects | A job that cannot progress | Results |
|---|---|---|---|---|---|
| legacy | outside emulate mode | `SIM_WORKLOAD_TRACE_PATH`, replayed from startup | one bare pod per job, deleted when done | completed by force after `SIM_WORKLOAD_STALL_FORCE_COMPLETE` (120 s) | `joulie_sim_job_*` counters at `/metrics` |
| v1 | emulate mode | the run API, or `SIM_WORKLOAD_TRACE_PATH` as an implicit run | one `batch/v1` Job per group, completed through KWOK's `pod-complete` Stage | reported as stalled, never completed | OTLP JSON Lines in the run directory, plus the run API's status |

`v1` without emulate mode, and `legacy` with it, stop the start. The variables are in [Simulator workloads and scenarios]({{< relref "/docs/getting-started/05-configuration-reference.md#simulator-workloads-and-scenarios" >}}). [Workload Scenarios]({{< relref "/docs/simulator/scenarios.md" >}}) run v1 workloads on the simulated cluster and check their completion, placement, slowdown and energy.

The v1 engine declares each job's class (the trace's `intentClass`) with the `joulie.io/workload-class` annotation, the declaration Joulie reads ([Pod Compatibility]({{< relref "/docs/getting-started/03-pod-compatibility.md" >}})).

## How a job progresses

A job carries CPU and GPU work units. At full speed each requested core or GPU does `SIM_BASE_SPEED_PER_CORE` units per second; a CPU cap, a throttle or a GPU cap lowers that rate according to the job's class, utilisation and intensities, and jobs on one node share it. A job completes when both its CPU and GPU work run out; the pods of a `gang=true` workload make no progress until all of them run.

- Compute-bound jobs slow down strongly under a cap; memory-bound, IO-bound and bandwidth-bound jobs more gently.
- CPU throttling still slows GPU jobs whose `cpuFeedIntensityGpu` is high.
- A trace record without `sensitivity` is slowed by a cap in full.

### Peer transfers

A v1 job with `workloadProfile.peerTransferIntensity` above 0 on two or more GPUs spends that share of its GPU time copying data in a ring over its GPUs, at the node profile's `gpus.transfer.peerLinkGBps`. A cap does not slow the copies, but a congested or gated link does ([Hardware Modeling]({{< relref "/docs/hardware/hardware-modeling.md#peer-transfers" >}}), `SIM_GPU_RECEIVER_GATE`). Legacy jobs and jobs on one GPU make no copies.

The rate formulas, what each job tracks, peer transfer mechanics and how jobs load the CPU and GPU models: [Simulator time and work](https://github.com/joulie-k8s/Joulie/wiki/Simulator-Time-and-Work) on the wiki.
