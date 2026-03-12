---
title: "Power Simulator"
weight: 30
---

This page describes the simulator runtime mechanics (control/state/energy paths).

The canonical physical model, provenance, and hardware assumptions are documented in:

- [Hardware Modeling]({{< relref "/docs/simulator/hardware-modeling.md" >}})

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

- CPU: cap, utilization, frequency scale, throttle target/current, saturation flag
- GPU: per-device cap/target/power, aggregate utilization, effective performance multiplier
- workload-class hints used by the model (`cpu.*`, `gpu.*`)

Supported control actions:

- `rapl.set_power_cap_watts`
- `dvfs.set_throttle_pct`
- `gpu.set_power_cap_watts`

## Model source of truth

This page intentionally avoids duplicating formulas and hardware assumptions.
Use [Hardware Modeling]({{< relref "/docs/simulator/hardware-modeling.md" >}}) as source of truth for:

- measured vs proxy curves,
- CPU/GPU workload-class behavior,
- heterogeneous-node normalization semantics,
- vendor/API-specific constraints and references.

## Operational note

When formulas and this page diverge, simulator behavior and `hardware-modeling.md` are authoritative; update this page only for runtime flow and interfaces.
