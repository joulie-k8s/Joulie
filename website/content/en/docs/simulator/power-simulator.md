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
- exposing power telemetry on `/telemetry/{node}`,
- integrating energy over time (`/debug/energy`).

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

The simulator returns both a compact node-level view and richer subsystem views on:

- `GET /telemetry/{node}`

Important top-level fields:

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
