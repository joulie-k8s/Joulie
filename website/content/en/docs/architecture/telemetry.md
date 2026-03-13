---
title: "Input Telemetry and Actuation Interfaces"
weight: 50
---

This page describes runtime IO contracts:

- how Joulie reads telemetry inputs,
- how Joulie sends control intents.

If you want the CRD-level summary first, read [CRD and Policy Model]({{< relref "/docs/architecture/policy.md" >}}).
This page is the detailed runtime reference for the `TelemetryProfile` contract.

It is not the `/metrics` exposition contract.
For exported metrics, see [Metrics Reference]({{< relref "/docs/architecture/metrics.md" >}}).

## Why this abstraction exists

Joulie must run in two worlds with the same control logic:

- real hardware clusters,
- simulator/KWOK clusters.

So agent/operator logic depends on provider interfaces, not directly on sysfs or simulator HTTP shape.

## `TelemetryProfile` in one sentence

`TelemetryProfile` tells the agent which telemetry backend to read from and which control backend to write to for a given node or scope.

Conceptually:

- `NodeHardware` defines discovered node capability
- `NodePowerProfile` defines the target
- `TelemetryProfile` defines the wiring

`TelemetryProfile` should not carry hardware identity.
Its job is only backend routing.
In particular:

- do not use `TelemetryProfile` to describe CPU/GPU model or inventory identity,
- do not hand-author it as a substitute for `NodeHardware`,
- do use it to route simulator HTTP or host backends.

## Telemetry provider model

Telemetry input can come from:

- `host` (real node interfaces),
- `http` (simulator endpoint),
- `prometheus` (cluster-level inputs, future expansion),
- `none`.

Quality handling should distinguish `fresh`, `stale`, and `missing` samples so policy can degrade safely.

## Control provider model

Control outputs are routed by backend:

- `host`: write real interfaces (RAPL/cpufreq),
- `http`: send intents to simulator,
- `none`.

Result semantics:

- `applied`
- `blocked`
- `error`
- `none` (no intent selected for that control path)

This allows desired vs applied behavior auditing.

## Current CPU control intents

Supported intent actions:

- `rapl.set_power_cap_watts`
- `dvfs.set_throttle_pct`

DVFS intent is normalized as `throttlePct` (`0..100`) to stay portable across heterogeneous CPUs.
Backend-specific frequency writes remain implementation details.

## GPU control intent

Supported GPU intent action:

- `gpu.set_power_cap_watts`

Payload:

- `capWattsPerGpu`

Semantics:

- node-level intent is translated to per-device enforcement,
- same cap is applied to all GPUs on the node,
- result is reported as `applied|blocked|error` in `TelemetryProfile.status.control.gpu`.

## Current HTTP contracts

Telemetry endpoint:

- `GET /telemetry/{node}`

Accepted minimal payloads include:

```json
{ "packagePowerWatts": 245.3 }
```

or

```json
{ "cpu": { "packagePowerWatts": 245.3 } }
```

The simulator exports a richer payload than this minimal contract. Important optional fields currently include:

- top level:
  - `packagePowerWatts`
  - `instantPackagePowerWatts`
- `cpu.*`:
  - `packagePowerWatts`
  - `instantPowerWatts`
  - `utilization`
  - `memoryIntensity`
  - `ioIntensity`
  - `freqScale`
  - `temperatureC`
  - `thermalThrottlePct`
- `gpu.*`:
  - `powerWattsTotal`
  - `avgPowerWattsTotal`
  - `utilization`
  - `memoryIntensity`
  - `cpuFeedIntensity`
  - `devices`

Controllers should treat these richer fields as additive observability, not as a replacement for the minimal contract above.

Control endpoint:

- `POST /control/{node}`

Example request:

```json
{
  "node": "worker-01",
  "action": "dvfs.set_throttle_pct",
  "throttlePct": 20,
  "ts": "2026-02-27T00:00:00Z"
}
```

Expected response includes:

- `result` (`applied|blocked|error`)
- `message`
- `state` (best-effort post-state)

## Current host contracts

Agent host mode uses Linux interfaces:

- RAPL energy/power cap files
- cpufreq files for observed and enforced frequency bounds

GPU host control backends:

- NVIDIA path: NVIDIA tooling/NVML-compatible power-limit controls
- AMD path: ROCm SMI power-limit controls where supported

If a GPU backend is unavailable on a node, result is `blocked` (not silent success).

Current deployment convention mounts host `/sys` into container `/host-sys`.

## CRD integration

Current runtime responsibilities:

- agent publishes `NodeHardware` for discovered hardware/capability state,
- operator writes `NodePowerProfile` targets,
- agent reads `NodePowerProfile` for desired state,
- agent reads node-scoped `TelemetryProfile` for source/control routing,
- agent writes control status under `TelemetryProfile.status.control`.

Today, the stable documented status contract is `TelemetryProfile.status.control`.
If additional diagnostic snapshots are present in some environments, treat them as auxiliary rather than core API contract.

## Next step

To see simulator-side algorithm details using these interfaces, read:

- [Workload and Power Simulator]({{< relref "/docs/simulator/simulator.md" >}})
- [Workload Simulator]({{< relref "/docs/simulator/workload-simulator.md" >}})
- [Power Simulator]({{< relref "/docs/simulator/power-simulator.md" >}})
