---
title: "Power Simulator"
weight: 24
---

This page says how the simulator advances a modelled node (any node outside the emulate-mode cluster spec): its controls, its physics step and its telemetry. The models and their sources are in [Hardware Modeling]({{< relref "/docs/hardware/hardware-modeling.md" >}}); how jobs progress is in [Workload Simulator]({{< relref "/docs/simulator/workload-simulator.md" >}}).

## The physics step

A modelled node advances only in a physics step, in simulated time, never when it is read. One step moves caps, frequency, power and temperature toward their targets with their settling lags, in substeps of at most 100 ms, and integrates energy from the instantaneous power.

| Mode | What steps the nodes | Step |
|---|---|---|
| Kubernetes (KWOK) | the physics tick, every `SIM_PHYSICS_TICK` (default `1s`) | the wall time since the last tick |
| Emulate mode | the emulator's tick (`SIM_EMULATE_TICK`), for the nodes outside the spec | the time between two snapshots |
| Standalone | each standalone tick | `SIM_SA_DT` seconds of virtual time, in substeps of `SIM_SA_SUBSTEP` |

A control only sets targets, and every read shows the node as the last step left it, so reading a node more or less often never changes its behaviour. The order of the work inside a substep and the lag equations are on the wiki page [CPU power model, Cap enforcement](https://github.com/joulie-k8s/Joulie/wiki/CPU-Power-Model#cap-enforcement).

## Runtime controls

The agent sends these actions to `POST /control/{node}`:

| Action | Effect |
|---|---|
| `rapl.set_power_cap_watts` | caps the node's CPU power as a whole, split evenly over its sockets; a cap below `SIM_RAPL_CAP_MIN_W` (default 0) is refused |
| `dvfs.set_throttle_pct` | throttles the CPU frequency |
| `gpu.set_power_cap_watts` | caps each GPU; a cap below the board's minimum power is refused |

## Telemetry

`GET /otlp/telemetry/{node}` serves the node's CPU, GPU and host power as an OTLP JSON document, and `/debug/energy` its integrated energy. The names are in [Simulator Metrics]({{< relref "/docs/simulator/metrics.md" >}}).

Step timing, node state fields, control edge cases and the legacy JSON format: [Simulator time and work](https://github.com/joulie-k8s/Joulie/wiki/Simulator-Time-and-Work) and [Simulator modes](https://github.com/joulie-k8s/Joulie/wiki/Simulator-Modes#http-api) on the wiki.
