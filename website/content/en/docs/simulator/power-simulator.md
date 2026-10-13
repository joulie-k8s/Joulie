---
title: "Power Simulator"
weight: 30
---

This page describes the simulator runtime mechanics (control/state/energy paths).

The canonical physical model, provenance, and hardware assumptions are documented in:

- [Hardware Modeling]({{< relref "/docs/hardware/hardware-modeling.md" >}})

For workload progression semantics:

- [Workload Simulator]({{< relref "/docs/simulator/workload-simulator.md" >}})

## Scope

The power simulator runtime is responsible for:

- keeping per-node control state (CPU cap, DVFS throttle, GPU cap),
- applying control actions from `/control/{node}`,
- updating dynamics with settling/ramp behavior,
- exposing power telemetry on `/otlp/telemetry/{node}` and `/telemetry/{node}` (OTLP JSON; `/telemetry/{node}?format=legacy` serves the deprecated legacy JSON until v0.4.0),
- integrating energy over time (`/debug/energy`).

## The physics step

A modelled node (any node outside the emulate-mode cluster spec) advances only in a physics step, in simulated time, never when it is read:

| Mode | What steps the nodes | Step | Longest substep |
|---|---|---|---|
| Kubernetes (KWOK) | the physics tick, every `SIM_PHYSICS_TICK` (default `1s`) | the wall time since the last tick | 100 ms |
| Emulate mode | the emulator's tick (`SIM_EMULATE_TICK`), for the nodes outside the spec | the time between two snapshots | 100 ms |
| Standalone | each standalone tick | `SIM_SA_DT` seconds of virtual time | `SIM_SA_SUBSTEP` (default `SIM_SA_DT`) |

- A control sets targets only: its response, and every read until the next step, show the cap and the power the last step left.
- The telemetry routes, `/state/{node}` and the per-node gauges report the node as the last step left it, so reading a node more or less often never changes its behaviour.
- Each node carries `simTime` in `/state/{node}`: the simulated time its physics has reached.
- The energy in `/debug/energy` and in the timeseries is the integral of each node's instantaneous CPU and GPU power over the substeps, not of the averaged telemetry signal.

The order of the work inside a substep and the lag equations are in [Hardware Modeling]({{< relref "/docs/hardware/hardware-modeling.md" >}}), section 5.5.

## Runtime state and controls

Main node state includes:

- CPU:
  - target/applied cap
  - utilization
  - effective frequency scale
  - throttle target/current
  - saturation flag
  - instantaneous and averaged power
  - temperature and thermal-throttle fraction
- GPU:
  - per-device cap/target
  - per-device instantaneous and averaged power
  - per-device temperature and thermal-throttle fraction
  - aggregate utilization
  - effective performance multiplier
- workload-model inputs aggregated from running jobs:
  - `memoryIntensity`
  - `ioIntensity`
  - `cpuFeedIntensity`

Supported control actions:

- `rapl.set_power_cap_watts`
- `dvfs.set_throttle_pct`
- `gpu.set_power_cap_watts`

## Telemetry contract actually exposed by the simulator

The simulator serves each node's power in two formats, computed from one snapshot of the node:

- `GET /otlp/telemetry/{node}`: one OTLP JSON `ExportMetricsServiceRequest`, the [HTTP input format]({{< relref "/docs/architecture/telemetry.md#http-input-format" >}}) that the agent's and the controller manager's `http` sources read. Its resource carries `k8s.node.name`. It holds `hw.power` gauges for the CPU (`hw.id=cpu`, `hw.type=cpu`) and for each GPU (`hw.id=gpu<index>`, `hw.type=gpu`, `hw.name` the product), and `hw.host.power` (`hw.id=host`) for their sum. The values are the averaged powers, or the instantaneous ones while the average is still 0, unrounded. An [emulated node]({{< relref "/docs/simulator/simulator.md#emulate-mode" >}}) answers instead with its part of the emulate-mode export: energy counters and power gauges per CPU package, DRAM zone and GPU.
- `GET /telemetry/{node}`: the same OTLP JSON by default. With `?format=legacy` it serves the legacy JSON, a compact node-level view and richer subsystem views, deprecated and removed in v0.4.0; the first such read logs one warning per process. Any other `format` than `otlp` or `legacy` answers 400.

Important top-level fields of the legacy JSON:

- `packagePowerWatts`
  - exported averaged node power
- `instantPackagePowerWatts`
  - internal instantaneous node power used by the model

Important CPU fields:

- `cpu.packagePowerWatts`
- `cpu.instantPowerWatts`
- `cpu.utilization`
- `cpu.memoryIntensity`
- `cpu.ioIntensity`
- `cpu.freqScale`
- `cpu.temperatureC`
- `cpu.thermalThrottlePct`

Important GPU fields:

- `gpu.powerWattsTotal`
- `gpu.avgPowerWattsTotal`
- `gpu.utilization`
- `gpu.memoryIntensity`
- `gpu.cpuFeedIntensity`
- `gpu.capWattsPerGpuApplied`
- `gpu.capWattsPerGpuTarget`
- `gpu.devices[]`

This distinction matters because the simulator is now intentionally modeling the difference between:

- internal fast-changing device power
- exported averaged telemetry seen by the controller

## Hardware identity and overrides

The simulator runtime no longer depends primarily on pre-enumerated node classes.

Today the intended precedence is:

1. node labels define simulated hardware identity
2. shared inventory resolves that identity into a CPU/GPU model
3. optional `SIM_NODE_CLASS_CONFIG` overrides refine or override profile parameters

So `SIM_NODE_CLASS_CONFIG` is still useful, but it is now an override layer rather than the main source of hardware truth.

## Model source of truth

This page intentionally avoids duplicating formulas and hardware assumptions.
Use [Hardware Modeling]({{< relref "/docs/hardware/hardware-modeling.md" >}}) as source of truth for:

- measured vs proxy curves,
- CPU/GPU workload-class behavior,
- heterogeneous-node normalization semantics,
- vendor/API-specific constraints and references.

## Operational note

When formulas and this page diverge, simulator behavior and `hardware-modeling.md` are authoritative; update this page only for runtime flow and interfaces.
