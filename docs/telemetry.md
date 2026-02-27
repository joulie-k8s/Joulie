# Input Telemetry and Actuation Interfaces

This document defines Joulie's **internal input interfaces** for telemetry and control.

Important distinction:

- this is **not** about Prometheus metrics exposed by Joulie,
- this is about how Joulie components **consume input data** and **apply controls**.

## Goals

1. Run against real hardware in bare metal clusters.
2. Run in virtual/simulated clusters (kind/kwok) with the same control logic.
3. Keep APIs generic enough for CPU + GPU + future signals.
4. Avoid policy/API churn when moving from rule-based to data-driven policies.

## Core model (simple)

Joulie uses:

- one `TelemetryProvider` abstraction for **input metrics** (for both agent and controller),
- one `ControlProvider` abstraction for **actuation/state readback** (agent-side).

Why this split:

- both agent and controller need input telemetry (different subsets),
- only agent needs low-level control application (RAPL/DVFS/GPU writes).

## TelemetryProvider (shared input abstraction)

`TelemetryProvider` should be usable by:

- agent (node-local signals like RAPL/DVFS/GPU state),
- controller (cluster/global signals like Prometheus aggregates, weather, grid carbon).

Expected signal families (generic, extensible):

- CPU:
  - package power,
  - frequency/utilization state.
- GPU (future-ready):
  - power/utilization/clocks/thermal state.
- thermal/facility:
  - inlet/outlet temperatures,
  - hotspot indicators,
  - PUE / energy mix / grid carbon (controller-level inputs).
- workload context:
  - requested/allocated CPU/GPU,
  - workload intent density.

Provider backends can be:

- `host` (sysfs/vendor tooling on real nodes),
- `http` (simulator endpoint),
- `mixed` (combination per signal family).

## Normalized snapshot and quality flags

`TelemetryProvider` should return normalized values with metadata:

- value,
- timestamp,
- quality flag.

Quality flags:

- `fresh`: recent enough for control decisions,
- `stale`: older than policy TTL (may be used with caution),
- `missing`: unavailable/failed.

This allows policy logic to degrade safely instead of failing hard when some inputs are delayed.

## ControlProvider (agent-side actuation abstraction)

Expected operations:

- CPU:
  - set/read package cap,
  - set/read DVFS bounds.
- GPU (future):
  - set/read power cap,
  - set/read clock policy.

Expected result model:

- `applied`,
- `blocked` (unsupported / permissions / policy guard),
- `error` (runtime failure),
- plus observed post-state.

This is required so Joulie can compare desired vs applied behavior even in simulation.

## Real-hardware mode

Host implementation maps to current Linux/device interfaces:

- RAPL: read `energy_uj`, write `constraint_0_power_limit_uw`,
- DVFS: read/write `scaling_*_freq` / `cpuinfo_*_freq`,
- future GPU backends via vendor APIs/tools.

Current mount convention:

- host `/sys` mounted in container at `/host-sys`.

## Simulator mode (HTTP)

HTTP implementation reads/writes through a simulator service.

Minimum simulator contract:

1. expose telemetry per node (CPU now, GPU-ready schema),
2. expose global/context telemetry (weather/grid carbon/prometheus-derived features),
3. accept control intents (RAPL/DVFS/GPU),
4. return applied state + status,
5. evolve telemetry over time based on workload allocation and controls.

This enables closed-loop validation with no physical RAPL/DVFS/GPU devices.

## Simulator concept for WAO vs Joulie comparison

Target workflow:

1. Simulator receives a cluster template (node hardware profiles/capabilities).
2. Simulator receives workload allocation state from Kubernetes/scheduler.
3. Simulator computes telemetry trajectories per node + global context.
4. WAO/Joulie read those metrics via their telemetry paths.
5. Joulie writes control intents; simulator reflects their impact on future telemetry.

This provides a fair same-workload/same-telemetry benchmark between WAO and Joulie.

## Future generic telemetry CRD (concept)

Current CRD (`NodePowerProfile`) is for desired profile assignment only.

For richer data-driven policies, add a generic telemetry CRD family (future work), CPU+GPU extensible from day one. Conceptual shape:

- `NodeTelemetryProfile` (or equivalent):
  - `spec.source.type`: `host|http|mixed`
  - `spec.source.http.endpoint` (sim mode)
  - `status.snapshot.cpu.*`
  - `status.snapshot.gpu[*].*`
  - `status.snapshot.thermal.*`
  - `status.snapshot.context.*` (controller-relevant features)
  - `status.snapshot.meta.timestamp/quality`

Key requirement: schema must be extensible by device family (CPU/GPU/accelerator) without breaking readers.
