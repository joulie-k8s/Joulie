---
title: "Workload Simulator"
weight: 20
---

This page documents the workload-side simulation model.

Trace generation methodology, statistical priors, multi-pod workload structure, and workload-generation references are documented in [Workload Generation]({{< relref "/docs/simulator/workload-generation.md" >}}).

## Scope

The workload simulator handles:

- trace/job ingestion,
- pod creation and placement via real scheduler,
- per-job progress updates,
- completion and pod deletion,
- class inference from scheduling constraints.

Power/control dynamics are documented separately in:

- [Power Simulator]({{< relref "/docs/simulator/power-simulator.md" >}})

## Trace-driven workload model

Enable with:

- `SIM_WORKLOAD_TRACE_PATH=/path/to/trace.jsonl`

The simulator loads `type=job` records and schedules pods over time according to submit offsets.

Helper tools:

- `simulator/cmd/workloadgen` (synthetic traces)
- `simulator/cmd/traceextract` (normalize/extract traces)

## Two engines: legacy and v1

`SIM_WORKLOAD_MODEL` selects the engine that runs a trace.

| | legacy (outside emulate mode) | v1 (emulate mode) |
|---|---|---|
| Kubernetes objects | one bare pod per job | one `batch/v1` Job per group (`workloadId`, `podRole`, submit offset), Indexed when larger than one, `backoffLimit: 0`, deleted `SIM_WORKLOAD_JOB_TTL` after it finishes |
| Class | inferred from the pod's affinity | declared by the annotation `joulie.io/workload-class` (`SIM_WORKLOAD_INTENT`) |
| Placement | `type=kwok` and the KWOK toleration | the trace's placement, plus the selector `sim.joulie.io/cluster=<spec>` and the KWOK toleration |
| Completion | the pod is deleted | the pod is annotated `sim.joulie.io/complete: "true"` and KWOK's `pod-complete` Stage moves it to `Succeeded` |
| A job that cannot progress | completed by force after `SIM_WORKLOAD_STALL_FORCE_COMPLETE` (120 s) | reported as stalled (`joulie.sim.job.stalled`, `joulie.sim.run.stalled`), never completed |
| Creates per tick | 20 | no cap |
| Speed | the node model's frequency scale and GPU cap ratio | the throughput of the emulated hardware the job runs on, under the limits the agent wrote |
| Energy | node and cluster totals at `/debug/energy` | each node's energy ledger delta, attributed per job and class by the jobs' requests; the unrequested share of idle package power is class `idle` |
| Results | `joulie_sim_job_*` counters at `/metrics` | OTLP JSON Lines in the run directory, plus the run API's status |

The v1 engine needs the emulated nodes, so `v1` without emulate mode, and `legacy` with it, stop the start. The legacy engine keeps the behaviour experiments 01 to 03 were measured with, except two loader fixes: trace lines longer than 64 KiB load, and jobs whose pod names collide get a suffix instead of a failed create. The variables and their defaults are in [Simulator workloads and scenarios]({{< relref "/docs/getting-started/05-configuration-reference.md#simulator-workloads-and-scenarios" >}}).

**Trace schema v3.** `workloadgen` writes schema v3 by default: a job carries no sensitivity unless one is given (the simulator then uses 1), no power-profile affinity (the class is `intentClass`, rendered by `SIM_WORKLOAD_INTENT`), and its work is sized so that its ideal runtime equals `durationSec` at `-base-speed-per-core` (default 1, the simulator's `SIM_BASE_SPEED_PER_CORE`). `-arrival-window-sec` compresses the submit offsets and `-max-duration-sec` clamps durations. `-peer-transfer-intensity p` writes `workloadProfile.peerTransferIntensity` on every job that requests two or more GPUs (see [Peer transfers](#peer-transfers)). `-schema-version v2` writes the previous schema byte for byte, which the experiments' scripts pin.

[Workload Scenarios]({{< relref "/docs/simulator/scenarios.md" >}}) run v1 workloads on the simulated cluster and check their completion, placement, slowdown and energy.

## Synthetic profile generation

When traces are generated with `simulator/cmd/workloadgen`, the simulator-side workload profile is not left empty.
The generator synthesizes both a coarse workload family and a richer per-job profile.

The detailed generation methodology now lives in [Workload Generation]({{< relref "/docs/simulator/workload-generation.md" >}}). This page focuses on how those generated fields are consumed at runtime.

Generated fields:

- `workloadClass.cpu`
- `workloadClass.gpu`
- `workloadProfile.cpuUtilization`
- `workloadProfile.gpuUtilization`
- `workloadProfile.memoryIntensity`
- `workloadProfile.ioIntensity`
- `workloadProfile.cpuFeedIntensityGpu`
- `workloadProfile.peerTransferIntensity`, only with `-peer-transfer-intensity`, on jobs with two or more GPUs

### Current generator behavior

The current workload generator no longer starts by sampling CPU and GPU classes independently from two flat categorical tables.
Instead, it first samples a **logical workload family**, then derives:

- pod structure,
- requested resources,
- shared intensity profile,
- CPU class,
- GPU class.

Examples:

- `debug_eval`
  - usually a short single-pod GPU workload
  - `cpu.mixed` + `gpu.memory_bound`
- `single_gpu_training`
  - single worker pod
  - `cpu.mixed` + `gpu.compute_bound`
- `distributed_training`
  - launcher + worker pods
  - `cpu.mixed` + `gpu.compute_bound`
- `parameter_server_training`
  - PS pods + worker pods
  - `cpu.memory_bound` + `gpu.compute_bound`
- `hpo_experiment`
  - controller + trial pods
  - `cpu.mixed` + `gpu.mixed`
- `cpu_preprocess`
  - CPU-only
  - `cpu.memory_bound`
- `cpu_analytics`
  - CPU-only
  - `cpu.compute_bound`

The detailed workload-family priors and their research motivation are documented in [Workload Generation]({{< relref "/docs/simulator/workload-generation.md" >}}).

### Shared workload profile defaults

The generator produces one shared workload profile for the logical workload and then reuses it across the pod-expanded jobs.
That shared profile includes:

- `cpuUtilization`
- `gpuUtilization`
- `memoryIntensity`
- `ioIntensity`
- `cpuFeedIntensityGpu`

Current behavior in broad terms:

- short `debug_eval` jobs get lower GPU utilization and shorter durations,
- distributed and parameter-server workloads get lower effective GPU utilization and higher coordination sensitivity,
- CPU-only preprocess jobs get higher memory intensity,
- HPO experiments expand into multiple trial pods that share a workload-level prior.

### Why generate both class and profile

The simulator still uses both layers:

- `workloadClass` gives the qualitative response family,
- `workloadProfile` gives the quantitative utilization and bottleneck signals.

This lets two workloads share a coarse class such as `gpu.compute_bound` while still differing in:

- average GPU utilization,
- CPU-side feed dependence,
- memory pressure,
- pod structure.

## Per-job execution model

Each job tracks:

- requested CPU cores `C_req`,
- total work `CPUUnitsTotal`,
- remaining work `CPUUnitsRemaining`,
- CPU sensitivity `S_cpu` in `[0,1]`,
- CPU target utilization `U_cpu` in `[0,1]`,
- CPU workload class (`cpu.compute_bound`, `cpu.memory_bound`, `cpu.io_bound`, `cpu.mixed`),
- requested GPUs `G_req` (optional),
- total/remaining GPU work (`GPUUnitsTotal`, `GPUUnitsRemaining`),
- GPU sensitivity `S_gpu` in `[0,1]`,
- GPU target utilization `U_gpu` in `[0,1]`,
- GPU workload class (`gpu.compute_bound`, `gpu.memory_bound`, `gpu.bandwidth_bound`, `gpu.mixed`),
- memory intensity `M` in `[0,1]`,
- IO intensity `I` in `[0,1]`,
- CPU feed intensity for GPU work `F_feed` in `[0,1]`.

Node effective CPU speed factor comes from power simulator output (`FreqScale`).
GPU jobs also use a GPU speed factor derived from per-GPU cap ratio.

CPU work progression combines:

- base CPU throughput per core,
- current node frequency scale,
- CPU workload class,
- explicit job utilization/intensity profile.

At a high level:

`cpuSpeed ~= C_req * BaseSpeedPerCore * throttleImpact(FreqScale, workloadClass, U_cpu, M, I)`

If `k` jobs run on same node, progress is approximated by fair share:

`cpuProgress = cpuSpeed * dt / max(1, k)`

State update:

`CPUUnitsRemaining -= cpuProgress`

GPU work progression combines:

- requested GPUs,
- current per-GPU cap ratio,
- GPU workload class,
- target GPU utilization,
- CPU feed pressure from the job.

At a high level:

`gpuSpeed ~= G_req * BaseSpeedPerGPU * gpuThroughput * cpuFeedImpact`

where `gpuThroughput` is the share of its throughput a GPU keeps under its cap for the job's workload class and utilization: 1 while the cap is above what the job draws.
The cap is counted once, through `gpuThroughput`, so `S_gpu` does not change the legacy GPU speed; the legacy model used to multiply by `1 - (1 - cap/max) * S_gpu` as well, which charged the cap twice.
Under `SIM_WORKLOAD_MODEL=v1` the cap costs `1 - S_gpu * (1 - gpuThroughput)`.
CPU throttling can also slow GPU jobs when CPU-side feeding is part of the bottleneck.

State update:

`GPUUnitsRemaining -= gpuProgress`

### Peer transfers

A v1 job whose trace record gives `workloadProfile.peerTransferIntensity` `p > 0` and that holds `n >= 2` GPUs of its node moves data between them. Each step it loads the emulated node with a ring of `n` copies over its GPUs in ascending order, GPU `k` to the next and the last to the first (two GPUs send to each other), each demanding `p * peerLinkGBps`, the `gpus.transfer.peerLinkGBps` of the node's hardware profile. The node gives each copy a rate: the copies into one GPU share its link, and with the receiver gate on (`SIM_GPU_RECEIVER_GATE`) a receiver whose data path is down lets through only a fraction of it, whatever the sender does. With `g` the lowest ratio of delivered to demanded rate over the job's ring, since a ring moves at its slowest link, the job's GPU progress becomes:

`gpuSpeed ~= G_req * BaseSpeedPerGPU * 1 / ((1 - p) / thr + p / g) * cpuFeedImpact`

where `thr = 1 - S_gpu * (1 - gpuThroughput)`, the v1 GPU factor above: `p` is the share of the job's GPU time spent on its copies, which a cap does not slow, and `1 - p` the share spent on work. Copies that get their demand (`g = 1`) cost nothing beyond the work the trace already sizes. A job with `p = 0`, the default for a record without the key, a job on one GPU, a job on a node whose profile has no peer link, and every legacy job make no copies and run as before.

Completion condition:

- job completes when both `CPUUnitsRemaining <= 0` and `GPUUnitsRemaining <= 0`,
- pod is deleted on completion.

For multi-pod workloads marked `gang=true`, the simulator now waits until the full workload gang is running before advancing progress.
This mainly affects distributed training and parameter-server style workloads emitted by the new generator.

## Slowdown estimate under throttling

Approximate remaining runtime at a given instant:

`T_remaining ~= CPUUnitsRemaining / (cpuSpeed / max(1, k))`

As `FreqScale` decreases (due to cap/throttle), speed drops and completion time rises.

The impact is intentionally workload-aware:

- CPU compute-bound jobs slow down strongly under CPU throttling.
- CPU memory-bound and IO-bound jobs slow down more gently.
- GPU compute-bound jobs slow down strongly under GPU caps.
- GPU memory/bandwidth-bound jobs slow down less for the same cap.
- CPU throttling can still hurt GPU jobs when `cpuFeedIntensityGpu` is high.

The simulator also aggregates explicit per-job `cpuUtilization` and `gpuUtilization` into node-level utilization, so higher-utilization jobs contribute more strongly to modeled node power.

## Coupling with the hardware model

The workload model now feeds explicit bottleneck signals into the hardware model instead of only picking a dominant node class.

For every node tick, the simulator aggregates from the currently running jobs:

- CPU demand-weighted `cpuUtilization`
- GPU demand-weighted `gpuUtilization`
- weighted `memoryIntensity`
- weighted `ioIntensity`
- weighted `cpuFeedIntensityGpu`

Those aggregated signals are then passed into the CPU and GPU physical models.

This matters because the same cap does not have the same effect on all workloads:

- high `memoryIntensity` reduces effective CPU switching activity, so power rises more slowly with CPU utilization and CPU throughput degrades more gently under DVFS/RAPL
- high `ioIntensity` further softens CPU sensitivity to throttling
- high `cpuFeedIntensityGpu` makes GPU jobs more sensitive to CPU throttling, because the CPU side becomes part of the bottleneck
- high `memoryIntensity` on GPU jobs shifts behavior toward the memory/bandwidth-bound regime, where moderate power caps often hurt throughput less than on compute-bound kernels

In other words, the workload profile is no longer just descriptive metadata. It directly changes both:

- simulated power draw
- simulated slowdown under capping/throttling

## Averaged power and thermal state

The simulator now also models two practical effects that matter when comparing experiments with real telemetry:

- **telemetry averaging windows**
- **temperature-driven thermal throttling**

CPU and GPU power are tracked as:

- an **instantaneous modeled power**
- an **exported averaged power**

The averaged power uses first-order smoothing windows configured in the hardware profile:

- `cpuTelemetryWindowMs`
- `gpu.telemetryWindowMs`

This is especially important for GPUs, because real NVML power telemetry on many modern NVIDIA parts is reported as a 1-second average rather than a truly instantaneous reading.

Thermal state is also modeled with first-order settling:

- temperature approaches an ambient-plus-power equilibrium
- once temperature crosses a throttle threshold, a thermal-throttle fraction is applied
- that thermal throttle reduces the attainable throughput multiplier

So short spikes, long steady-state runs, and sustained capped operation do not all look the same in telemetry anymore.

## Workload profile fields in simulation

The simulator generates workload profile fields for each job. These fields are consumed by the simulated controller manager/twin and scheduler extender.

Generated fields:

| Field | Values | Effect |
|-------|--------|--------|
| `criticality.class` | `performance`, `standard` | Scheduler hard-rejects eco placement for performance workloads |
| `cpu.capSensitivity` | `high`, `medium`, `low` | Scheduler prefers uncapped nodes for high-sensitivity workloads |
| `gpu.capSensitivity` | `high`, `medium`, `low` | As above, for GPU headroom |
| `cpu.bound` | `compute`, `memory`, `io`, `mixed` | Affects slowdown model under CPU throttling |
| `gpu.bound` | `compute`, `memory`, `mixed`, `none` | Affects slowdown model under GPU cap |

These fields are used by:
- the simulated controller manager twin (`pkg/controller/twin`) to compute headroom and stress scores,
- the scheduler extender (`cmd/scheduler`) to apply workload-class-aware scoring.

The heterogeneous benchmark (`experiments/02-heterogeneous-benchmark/`) exercises all three baselines using a mixed batch with all the above profile variants.

## Scheduling class inference

The legacy engine infers the workload class from pod scheduling constraints:

- `performance` (avoids eco nodes via affinity)
- `standard` (unconstrained)

This is used for workload counters and class-based completion metrics. The v1 engine takes the class from the trace's `intentClass` and declares it on the pod with the `joulie.io/workload-class` annotation, the declaration Joulie reads.

## Why this model

- keeps Kubernetes scheduling behavior real,
- makes control impact visible through completion-time changes,
- supports reproducible benchmark traces across policy baselines.
