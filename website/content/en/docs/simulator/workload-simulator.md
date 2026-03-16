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

`gpuSpeed ~= G_req * BaseSpeedPerGPU * gpuCapImpact * gpuClassImpact * cpuFeedImpact`

where CPU throttling can also slow GPU jobs when CPU-side feeding is part of the bottleneck.

State update:

`GPUUnitsRemaining -= gpuProgress`

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

## WorkloadProfile fields in simulation

The simulator generates `WorkloadProfile`-compatible fields for each job. These fields mirror the `joulie.io/v1alpha1` `WorkloadProfile` CRD and are consumed by the simulated operator/twin and scheduler extender.

Generated fields:

| Field | Values | Effect |
|-------|--------|--------|
| `criticality.class` | `performance`, `standard` | Scheduler hard-rejects eco placement for performance workloads |
| `migratability.reschedulable` | `true` / `false` | Operator considers workload for rescheduling under pressure |
| `cpu.capSensitivity` | `high`, `medium`, `low` | Scheduler prefers uncapped nodes for high-sensitivity workloads |
| `gpu.capSensitivity` | `high`, `medium`, `low` | As above, for GPU headroom |
| `cpu.bound` | `compute`, `memory`, `io`, `mixed` | Affects slowdown model under CPU throttling |
| `gpu.bound` | `compute`, `memory`, `mixed`, `none` | Affects slowdown model under GPU cap |

Standard jobs are marked reschedulable by default. Performance jobs are not reschedulable.

These fields are used by:
- the simulated operator twin (`pkg/operator/twin`) to compute headroom and stress scores,
- the scheduler extender (`cmd/scheduler`) to apply workload-class-aware scoring,
- the migration controller (`pkg/operator/migration`) to generate reschedule recommendations.

The heterogeneous benchmark (`experiments/02-heterogeneous-benchmark/`) exercises all three baselines using a mixed batch with all the above profile variants.

## Scheduling class inference

Workload class is inferred from pod scheduling constraints:

- `performance` (avoids eco nodes via affinity)
- `standard` (unconstrained)

This is used for workload counters and class-based completion metrics.

## Why this model

- keeps Kubernetes scheduling behavior real,
- makes control impact visible through completion-time changes,
- supports reproducible benchmark traces across policy baselines.
