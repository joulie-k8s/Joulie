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

Provider backends can be selected per signal family:

- `none` (not provided),
- `host` (sysfs/vendor tooling on real nodes),
- `http` (simulator endpoint),
- `prometheus` (query-based pull for controller/global inputs).

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

## DVFS control signal shape

Current DVFS control intent is normalized as `throttlePct` (0-100), not a fixed Hz.

Reason:

- frequency domains and available frequency steps vary by CPU/vendor/platform,
- a percentage is portable across heterogeneous nodes and simulator backends,
- host-specific Hz writes remain an implementation detail of the `host` control backend.

On real nodes, the agent still exports observed/max frequency metrics in kHz.

## Real-hardware mode

Host implementation maps to current Linux/device interfaces:

- RAPL: read `energy_uj`, write `constraint_0_power_limit_uw`,
- DVFS: read/write `scaling_*_freq` / `cpuinfo_*_freq`,
- future GPU backends via vendor APIs/tools.

Current mount convention:

- host `/sys` mounted in container at `/host-sys`.

Agent backend selection is self-discovered:

- if cpufreq files are present, host DVFS writes are enabled,
- if cpufreq files are missing, host DVFS writes are disabled automatically,
- if HTTP control is configured, DVFS intents are still applied through HTTP.

## Simulator mode (HTTP)

HTTP implementation reads/writes through a simulator service.

Minimum simulator contract:

1. expose telemetry per node (CPU now, GPU-ready schema),
2. expose global/context telemetry (weather/grid carbon/prometheus-derived features),
3. accept control intents (RAPL/DVFS/GPU),
4. return applied state + status,
5. evolve telemetry over time based on workload allocation and controls.

This enables closed-loop validation with no physical RAPL/DVFS/GPU devices.

### Current HTTP input contract (implemented now)

For DVFS observed-power input, the current agent implementation reads:

- `GET <endpoint>` where `{node}` placeholder (if present) is replaced with node name.

Accepted JSON forms:

```json
{ "packagePowerWatts": 245.3 }
```

or

```json
{ "cpu": { "packagePowerWatts": 245.3 } }
```

This is a minimal first contract and will evolve as telemetry coverage expands.

### Current HTTP control contract (implemented now)

Agent sends `POST <endpoint>` (with `{node}` replacement) with JSON payload:

```json
{
  "node": "worker-01",
  "action": "rapl.set_power_cap_watts | dvfs.set_throttle_pct",
  "capWatts": 120.0,
  "throttlePct": 20,
  "ts": "2026-02-27T00:00:00Z"
}
```

Simulator/backend applies it and returns success/failure.

## Simulator concept for WAO vs Joulie comparison

Target workflow:

1. Simulator receives a cluster template (node hardware profiles/capabilities).
2. Simulator receives workload allocation state from Kubernetes/scheduler.
3. Simulator computes telemetry trajectories per node + global context.
4. WAO/Joulie read those metrics via their telemetry paths.
5. Joulie writes control intents; simulator reflects their impact on future telemetry.

This provides a fair same-workload/same-telemetry benchmark between WAO and Joulie.

Implementation notes are documented in:

- `docs/simulator.md`
- `simulator/README.md`

## Generic telemetry CRD (current + extension path)

Current CRDs:

- `NodePowerProfile`: desired power profile assignment.
- `TelemetryProfile`: telemetry source routing/configuration.

Current ownership/consumption model:

- Operator writes `NodePowerProfile` (desired node target).
- Agent reads `NodePowerProfile`.
- Agent reads node-scoped `TelemetryProfile` to choose telemetry source/control backend.
- Agent writes `TelemetryProfile.status.control.*` as applied/blocked/error feedback.

At the moment, operator does not yet consume `TelemetryProfile` for decision logic; that is reserved for future policy extensions (cluster/global telemetry inputs).

`TelemetryProfile` currently covers source routing (for example CPU from `host` or `http`) and is the basis for simulated input mode.
It should be extended over time with CPU/GPU/thermal/context status snapshots while preserving schema compatibility.

Current control routing in the same CRD:

- `spec.controls.cpu.type`: `none|host|http`
- `spec.controls.cpu.http.endpoint`: HTTP control sink for CPU intents
- `spec.controls.cpu.http.mode`: `auto|rapl|dvfs`

Key requirement: schema must remain extensible by device family (CPU/GPU/accelerator) without breaking readers.
