---
title: "Power Simulator"
weight: 24
---

This page says how the simulator advances a modelled node (any node outside the emulate-mode cluster spec): its control state, its physics step, and the telemetry it serves. The physical model itself, its parameters and their sources, is in [Hardware Modeling]({{< relref "/docs/hardware/hardware-modeling.md" >}}); how jobs progress on a node is in [Workload Simulator]({{< relref "/docs/simulator/workload-simulator.md" >}}).

The power simulator:

- keeps per-node control state (CPU cap, DVFS throttle, GPU cap),
- applies control actions from `/control/{node}`,
- advances caps, frequency, power and temperature with their settling lags,
- serves power telemetry on `/otlp/telemetry/{node}` and `/telemetry/{node}`,
- integrates energy over time (`/debug/energy`).

## The physics step

A modelled node advances only in a physics step, in simulated time, never when it is read:

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

Node state:

- CPU: target and applied cap, utilization, effective frequency scale, throttle target and current, saturation flag, instantaneous and averaged power, temperature and thermal-throttle fraction.
- GPU: per-device cap and target, per-device instantaneous and averaged power, per-device temperature and thermal-throttle fraction, aggregate utilization, effective performance multiplier.
- Workload inputs aggregated from the running jobs: `memoryIntensity`, `ioIntensity`, `cpuFeedIntensity`.

Control actions: `rapl.set_power_cap_watts`, `dvfs.set_throttle_pct`, `gpu.set_power_cap_watts`.

## Telemetry

The simulator serves each node's power in two formats, computed from one snapshot of the node:

- `GET /otlp/telemetry/{node}`: one OTLP JSON `ExportMetricsServiceRequest`, the [HTTP input format]({{< relref "/docs/architecture/telemetry.md#http-input-format" >}}) that the agent's and the controller manager's `http` sources read. Its resource carries `k8s.node.name`. It holds `hw.power` gauges for the CPU (`hw.id=cpu`, `hw.type=cpu`) and for each GPU (`hw.id=gpu<index>`, `hw.type=gpu`, `hw.name` the product), and `hw.host.power` (`hw.id=host`) for their sum. The values are the averaged powers, or the instantaneous ones while the average is still 0, unrounded. An [emulated node]({{< relref "/docs/simulator/emulate-mode.md" >}}) answers instead with its part of the emulate-mode export: energy counters and power gauges per CPU package, DRAM zone and GPU.
- `GET /telemetry/{node}`: the same OTLP JSON. `?format=legacy` serves the deprecated legacy JSON until v0.4.0, with one warning per process; any other `format` than `otlp` or `legacy` answers 400.

The legacy JSON exposes the averaged and the instantaneous power side by side, which the OTLP document does not:

- top level: `packagePowerWatts` (exported averaged node power), `instantPackagePowerWatts` (instantaneous node power used by the model);
- CPU: `cpu.packagePowerWatts`, `cpu.instantPowerWatts`, `cpu.utilization`, `cpu.memoryIntensity`, `cpu.ioIntensity`, `cpu.freqScale`, `cpu.temperatureC`, `cpu.thermalThrottlePct`;
- GPU: `gpu.powerWattsTotal`, `gpu.avgPowerWattsTotal`, `gpu.utilization`, `gpu.memoryIntensity`, `gpu.cpuFeedIntensity`, `gpu.capWattsPerGpuApplied`, `gpu.capWattsPerGpuTarget`, `gpu.devices[]`.

`/debug/nodes` shows the same state for every node.
