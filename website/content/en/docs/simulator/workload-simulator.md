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
- CPU sensitivity `S_cpu` in `[0,1]`.

Node effective speed factor comes from power simulator output (`FreqScale`).

Per-job speed on node:

`speed = C_req * BaseSpeedPerCore * (1 - (1 - FreqScale) * S_cpu)`

If `k` jobs run on same node, progress is approximated by fair share:

`progress = speed * dt / max(1, k)`

State update:

`CPUUnitsRemaining -= progress`

Completion condition:

- job completes when `CPUUnitsRemaining <= 0`,
- pod is deleted on completion.

## Slowdown estimate under throttling

Approximate remaining runtime at a given instant:

`T_remaining ~= CPUUnitsRemaining / (speed / max(1, k))`

As `FreqScale` decreases (due to cap/throttle), speed drops and completion time rises.

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
