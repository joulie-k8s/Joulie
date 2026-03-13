---
title: "Workload Simulator"
weight: 20
---

This page documents the workload-side simulation model.

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

## Scheduling class inference

Workload class is inferred from pod scheduling constraints:

- `performance`
- `eco`
- `general` (unconstrained)

This is used for workload counters and class-based completion metrics.

## Why this model

- keeps Kubernetes scheduling behavior real,
- makes control impact visible through completion-time changes,
- supports reproducible benchmark traces across policy baselines.
