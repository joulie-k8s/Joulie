---
title: "Workload Simulator"
weight: 22
---

This page says how the simulator runs a workload trace: which Kubernetes objects it creates, how each job progresses under caps, and how jobs load the hardware model. [Workload Generation]({{< relref "/docs/simulator/workload-generation.md" >}}) describes the traces; [Power Simulator]({{< relref "/docs/simulator/power-simulator.md" >}}) the power side of a node.

## Two engines: legacy and v1

`SIM_WORKLOAD_MODEL` selects the engine that runs a trace.

| | legacy (outside emulate mode) | v1 (emulate mode) |
|---|---|---|
| Started by | `SIM_WORKLOAD_TRACE_PATH`, replayed from startup | the run API, or `SIM_WORKLOAD_TRACE_PATH` as an implicit run |
| Kubernetes objects | one bare pod per job | one `batch/v1` Job per group (`workloadId`, `podRole`, submit offset), Indexed when larger than one, `backoffLimit: 0`, deleted `SIM_WORKLOAD_JOB_TTL` after it finishes |
| Class | inferred from the pod's affinity | declared by the annotation `joulie.io/workload-class` (`SIM_WORKLOAD_INTENT`) |
| Placement | `type=kwok` and the KWOK toleration | the trace's placement, plus the selector `sim.joulie.io/cluster=<spec>` and the KWOK toleration |
| Completion | the pod is deleted | the pod is annotated `sim.joulie.io/complete: "true"` and KWOK's `pod-complete` Stage moves it to `Succeeded` |
| A job that cannot progress | completed by force after `SIM_WORKLOAD_STALL_FORCE_COMPLETE` (120 s) | reported as stalled (`joulie.sim.job.stalled`, `joulie.sim.run.stalled`), never completed |
| Creates per tick | 20 | no cap |
| Speed | the node model's frequency scale and GPU cap ratio | the throughput of the emulated hardware the job runs on, under the limits the agent wrote |
| Energy | node and cluster totals at `/debug/energy` | each node's energy ledger delta, attributed per job and class by the jobs' requests; the unrequested share of idle package power is class `idle` |
| Results | `joulie_sim_job_*` counters at `/metrics` | OTLP JSON Lines in the run directory, plus the run API's status |

In a v1 run a job is submitted at the first tick after its create call returns, scheduled at the first tick that sees its pod bound, and done at the tick its work runs out, so its times are exact to one `SIM_EMULATE_TICK`. A finish with cleanup deletes the run's Job objects or bare pods by the run's label, one call per namespace ([Run API]({{< relref "/docs/getting-started/05-configuration-reference.md#simulator-workloads-and-scenarios" >}})).

The v1 engine needs the emulated nodes, so `v1` without emulate mode, and `legacy` with it, stop the start. The variables and their defaults are in [Simulator workloads and scenarios]({{< relref "/docs/getting-started/05-configuration-reference.md#simulator-workloads-and-scenarios" >}}). [Workload Scenarios]({{< relref "/docs/simulator/scenarios.md" >}}) run v1 workloads on the simulated cluster and check their completion, placement, slowdown and energy.

## Class

The legacy engine infers the class of a pod from its scheduling constraints: `performance` when its affinity avoids eco nodes, `standard` otherwise. It counts completions per class. The v1 engine takes the class from the trace's `intentClass` and declares it on the pod with the `joulie.io/workload-class` annotation, the declaration Joulie reads ([Pod Compatibility]({{< relref "/docs/getting-started/03-pod-compatibility.md" >}})).

## Per-job execution model

Each job tracks:

- requested CPU cores `C_req`, total and remaining CPU work (`CPUUnitsTotal`, `CPUUnitsRemaining`),
- CPU sensitivity `S_cpu` and target utilization `U_cpu`, in `[0,1]`, and its CPU workload class,
- requested GPUs `G_req` (optional), total and remaining GPU work (`GPUUnitsTotal`, `GPUUnitsRemaining`),
- GPU sensitivity `S_gpu` and target utilization `U_gpu`, in `[0,1]`, and its GPU workload class,
- memory intensity `M`, IO intensity `I` and CPU feed intensity for GPU work `F_feed`, in `[0,1]`.

A trace record without `sensitivity` has `S_cpu = S_gpu = 1`: a cap slows the job in full.

**CPU work** combines the base CPU throughput per core, the node's current frequency scale (`FreqScale`, from the power model), the CPU workload class, and the job's utilization and intensity profile:

`cpuSpeed ~= C_req * BaseSpeedPerCore * throttleImpact(FreqScale, workloadClass, U_cpu, M, I)`

With `k` jobs on the same node, progress is a fair share:

`cpuProgress = cpuSpeed * dt / max(1, k)` and `CPUUnitsRemaining -= cpuProgress`

**GPU work** combines the requested GPUs, the per-GPU cap, the GPU workload class, the target GPU utilization and the CPU feed pressure:

`gpuSpeed ~= G_req * BaseSpeedPerGPU * gpuThroughput * cpuFeedImpact`

`gpuThroughput` is the share of its throughput a GPU keeps under its cap for the job's class and utilization: 1 while the cap is above what the job draws. In the legacy engine the cap counts once, through `gpuThroughput`, and `S_gpu` does not change the GPU speed. Under `SIM_WORKLOAD_MODEL=v1` the cap costs `1 - S_gpu * (1 - gpuThroughput)`. `cpuFeedImpact` slows a GPU job when its CPU is throttled and `F_feed` is high.

`GPUUnitsRemaining -= gpuProgress`

A job completes when both `CPUUnitsRemaining <= 0` and `GPUUnitsRemaining <= 0`. The pods of a `gang=true` workload make no progress until all of them run.

### Peer transfers

A v1 job whose trace record gives `workloadProfile.peerTransferIntensity` `p > 0` and that holds `n >= 2` GPUs of its node moves data between them. Each step it loads the emulated node with a ring of `n` copies over its GPUs in ascending order, GPU `k` to the next and the last to the first (two GPUs send to each other), each demanding `p * peerLinkGBps`, the `gpus.transfer.peerLinkGBps` of the node's hardware profile. The node gives each copy a rate: the copies into one GPU share its link, and with the receiver gate on (`SIM_GPU_RECEIVER_GATE`) a receiver whose data path is down lets through only a fraction of it, whatever the sender does. With `g` the lowest ratio of delivered to demanded rate over the job's ring, since a ring moves at its slowest link, the job's GPU progress becomes:

`gpuSpeed ~= G_req * BaseSpeedPerGPU * 1 / ((1 - p) / thr + p / g) * cpuFeedImpact`

where `thr = 1 - S_gpu * (1 - gpuThroughput)`, the v1 GPU factor above: `p` is the share of the job's GPU time spent on its copies, which a cap does not slow, and `1 - p` the share spent on work. Copies that get their demand (`g = 1`) cost nothing beyond the work the trace already sizes. A job with `p = 0`, the default for a record without the key, a job on one GPU, a job on a node whose profile has no peer link, and every legacy job make no copies.

## Slowdown under throttling

The remaining runtime at a given instant is about:

`T_remaining ~= CPUUnitsRemaining / (cpuSpeed / max(1, k))`

As `FreqScale` falls under a cap or a throttle, speed drops and completion time rises, by workload:

- CPU compute-bound jobs slow down strongly under CPU throttling.
- CPU memory-bound and IO-bound jobs slow down more gently.
- GPU compute-bound jobs slow down strongly under GPU caps.
- GPU memory-bound and bandwidth-bound jobs slow down less for the same cap.
- CPU throttling still hurts GPU jobs when `cpuFeedIntensityGpu` is high.

## Coupling with the hardware model

Every node tick, the simulator aggregates from the jobs running on the node a demand-weighted `cpuUtilization` and `gpuUtilization`, and weighted `memoryIntensity`, `ioIntensity` and `cpuFeedIntensityGpu`, and passes them to the CPU and GPU models. The same cap therefore has a different effect on different work:

- high `memoryIntensity` lowers CPU switching activity, so power rises more slowly with utilization and throughput degrades more gently under DVFS or RAPL;
- high `ioIntensity` softens the CPU's sensitivity to throttling further;
- high `cpuFeedIntensityGpu` makes GPU jobs sensitive to CPU throttling, because the CPU side becomes part of the bottleneck;
- high `memoryIntensity` on a GPU job moves it toward the memory-bound regime, where a moderate cap often costs less throughput than on compute-bound kernels.

## Averaged power and temperature

CPU and GPU power are tracked twice: an instantaneous modelled power, and an exported average over the hardware profile's telemetry window (`cpuTelemetryWindowMs`, `gpu.telemetryWindowMs`). Many NVIDIA GPUs report power through NVML as a 1 s average, so the exported value lags short spikes as real telemetry does.

Temperature approaches ambient plus power over the watts per degree with a first-order lag. Thermal throttling is off by default; when enabled ([Simulator thermal model]({{< relref "/docs/getting-started/05-configuration-reference.md#simulator-thermal-model" >}})), a temperature above the start threshold reduces the attainable throughput, down to 0 at the full threshold.
