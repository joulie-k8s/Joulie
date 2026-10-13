---
title: "CRD and Policy Model"
weight: 10
---

This page defines Joulie's core contract:

- **demand** comes from pod scheduling constraints,
- **supply** is exposed by node power-profile labels,
- **discovered hardware** is published through `NodeHardware`,
- **desired state** is published through `NodeTwin`.

## APIs

Group/version:

- `joulie.io/v1alpha1`

CRDs:

- `NodeHardware` (`nodehardwares`, cluster-scoped)
- `NodeTwin` (`nodetwins`, cluster-scoped)

CRD definitions live in:

- `config/crd/bases/joulie.io_nodehardwares.yaml`
- `config/crd/bases/joulie.io_nodetwins.yaml`

## Demand model (workloads)

Workload class is determined from the `joulie.io/workload-class` pod annotation:

- `performance` demand: pod carries `joulie.io/workload-class: performance`.
- `standard` demand (default): no annotation, or `joulie.io/workload-class: standard`. Can run on any node; adaptive scoring steers toward eco when performance nodes are congested.

## Supply model (nodes)

Node supply is represented by:

- `joulie.io/power-profile=performance|eco` (node label, set by the controller manager)
- `NodeTwin.status.schedulableClass` (internal, set by controller manager's twin controller)

Semantics:

- `performance`: full-performance supply
- `eco`: low-power supply
- `draining` (schedulableClass only): transition safeguard active while node is moving toward eco; the scheduler extender filters it out for performance pods

The `schedulableClass` field is internal to the controller manager and scheduler extender. Users interact only with the `joulie.io/workload-class` pod annotation for placement intent.

## Desired-state + twin output: `NodeTwin`

`NodeTwin` is the controller-manager-to-agent contract for one node. The `spec` carries desired state; the `status` carries twin output (including control feedback and schedulable class).

Main spec fields:

- `spec.nodeName` (required)
- `spec.profile` (required, `performance|eco`)
- `spec.cpu.packagePowerCapWatts` (optional, absolute package cap)
- `spec.cpu.packagePowerCapPctOfMax` (optional, normalized policy intent)
- `spec.cpu.governor` (optional, CPU governor preset; see [Governor policy](#governor-policy)):
  - `preset` (`performance|balanced|efficiency`)
  - `boost` (optional, `on|off`)
- `spec.gpu.powerCap` (optional):
  - `scope` (`perGpu`)
  - `capWattsPerGpu` (absolute per-GPU cap)
  - `capPctOfMax` (percentage of node GPU max power)
- `spec.gpu.governor` (optional, GPU governor preset):
  - `preset` (`auto|sustained|efficient|low|peak`)
  - `computeClockCeilingPctOfMax` (only with `efficient`)
- `spec.policy.name` (optional, provenance/debug)

Resolution/precedence in agent runtime:

1. CPU: `packagePowerCapWatts` if present, otherwise `packagePowerCapPctOfMax`
2. GPU: `capWattsPerGpu` if present, otherwise `capPctOfMax`

What the controller manager typically writes today:

- CPU intent is commonly emitted as `packagePowerCapPctOfMax` on CPU-only nodes.
- A GPU node's CPU gets `packagePowerCapWatts` at `PERFORMANCE_CAP_WATTS` whatever the node's profile: capping the CPU of a GPU node slows the GPU data feed by more than it saves, so a GPU node gets only GPU caps.
- GPU intent may be emitted as:
  - `capPctOfMax` only, when the agent is expected to resolve percentage to watts from device limits,
  - `capWattsPerGpu` plus `capPctOfMax`, when the controller manager has deterministic model-based mapping available and wants the absolute target to be explicit.

Example:

```yaml
apiVersion: joulie.io/v1alpha1
kind: NodeTwin
metadata:
  name: node-kwok-gpu-nvidia-0
spec:
  nodeName: kwok-gpu-nvidia-0
  profile: eco
  cpu:
    packagePowerCapWatts: 5000
  gpu:
    powerCap:
      scope: perGpu
      capPctOfMax: 60
      capWattsPerGpu: 210
  policy:
    name: static-partition-v1
```

In this example:

- The CPU of this GPU node is left at the performance cap; a CPU-only eco node would get `packagePowerCapPctOfMax: 60` instead.
- GPU is expressed as both percentage and resolved absolute watts per GPU.
- The agent applies the absolute GPU cap when present and falls back to percentage only when absolute watts are not provided.

## Governor policy

A governor decides how eagerly a CPU or GPU raises its clocks. It saves less than a cap and slows work less. Joulie sets one vendor-neutral preset per component, which the agent maps to the node's native settings. The preset follows the profile the node runs at, so a draining node keeps the performance presets, as it keeps the performance caps. Pod classes (`joulie.io/workload-class`) only steer placement. Off by default (`GOVERNOR_CONTROL=off`); see [Governor control]({{< relref "/docs/getting-started/05-configuration-reference.md#governor-control" >}}) for the variables.

| Node state | CPU-only node: `spec.cpu.governor` | GPU node: `spec.cpu.governor` | `spec.gpu.governor` | Why |
|---|---|---|---|---|
| ActivePerformance | `CPU_GOVERNOR_PERFORMANCE_PRESET` (`performance`) | not written (`CPU_GOVERNOR_ON_GPU_NODES=false`) | the model's entry in `GPU_GOVERNOR_MODEL_PRESETS_JSON`, else `GPU_GOVERNOR_PERFORMANCE_PRESET` (empty: not written) | race to idle; a data path is held only on models the operator opted in |
| DrainingPerformance | as ActivePerformance | not written | as ActivePerformance | performance pods still run |
| ActiveEco | `CPU_GOVERNOR_ECO_PRESET` (`efficiency`), with `CPU_GOVERNOR_ECO_BOOST` when set | not written | the model's entry, else `GPU_GOVERNOR_ECO_PRESET` (`efficient`, ceiling `GPU_GOVERNOR_ECO_CEILING_PCT_OF_MAX`, 80) | a compute-bound job's energy optimum sits below the maximum clock; a memory-bound one loses almost nothing |
| preset not in the node's `NodeHardware` presets | not written, one warning | | not written, one warning (no fallback to `auto`, which would override an operator's own setting) | never ask a node for what it cannot do |
| `GOVERNOR_CONTROL=off` | nothing written | nothing | nothing | the default |

`GOVERNOR_CONTROL=cpu` writes only the CPU column, `gpu` only the GPU one. When a field leaves the spec, because control was turned off or the node stopped listing the preset, the agent puts back the state it recorded before its first write. The twin's power budget and the scheduler extender do not read the governor: a governor moves the measured power, which the twin already reads.

## Hardware-discovery object: `NodeHardware`

`NodeHardware` is the agent-to-controller-manager contract for discovered node capabilities.

It is status-oriented and agent-owned.
Users should not normally create it by hand.

Main fields:

- `spec.nodeName`
- `status.cpu`:
  - raw model string
  - normalized inventory model when recognized
  - sockets / total cores / cores per socket
  - runtime CPU cap range when available
  - frequency landmarks from cpufreq (`landmarks`: minimum, nominal, maximum boost)
  - the cpufreq governor state, the presets the node accepts and the baseline the agent restores (`governor`)
  - control and telemetry availability
- `status.gpu`:
  - raw model string
  - normalized inventory model when recognized
  - GPU count
  - runtime GPU cap range when available
  - the GPU clock governor state, clock ranges, the presets the GPUs accept and the baseline per GPU (`governor`)
  - control and telemetry availability
- `status.quality`:
  - overall discovery quality
  - warnings

Discovery quality values are:

- `exact`: recognized against the inventory
- `heuristic`: some raw hardware signal is present, but normalization is incomplete
- `unavailable`: no useful signal was found for that subsystem

Example:

```yaml
apiVersion: joulie.io/v1alpha1
kind: NodeHardware
metadata:
  name: node-kwok-gpu-nvidia-0
spec:
  nodeName: kwok-gpu-nvidia-0
status:
  cpu:
    rawModel: AMD EPYC 9654 96-Core Processor
    model: AMD_EPYC_9654
    vendor: amd
    sockets: 2
    totalCores: 192
    coresPerSocket: 96
    driverFamily: amd-pstate
    controlAvailable: true
    telemetryAvailable: true
    quality: exact
  gpu:
    rawModel: NVIDIA H100 NVL
    model: NVIDIA_H100_NVL
    vendor: nvidia
    count: 4
    capMinWatts: 200
    capMaxWatts: 400
    controlAvailable: true
    telemetryAvailable: true
    quality: exact
  capabilities:
    cpuControl: true
    gpuControl: true
    cpuTelemetry: true
    gpuTelemetry: true
  quality:
    overall: exact
    warnings: []
```

The controller manager uses `NodeHardware` as the source of truth for:

- hardware recognition against the inventory,
- the hardware family of each node, used by the per-family performance split ([Policy Algorithms]({{< relref "/docs/architecture/policies.md" >}})),
- per-device fallback when only part of the node is recognized.

In simulator-first setups, the controller manager can fall back to node hardware labels when `NodeHardware` has not been published yet.
This keeps simulator examples lightweight while preserving the same architecture once the agent starts publishing discovered hardware.

## Telemetry/control backend selection

Telemetry and control backend selection is configured via environment variables on the agent, not through a CRD.

At the policy-model level, the important distinction is:

- `NodeTwin.spec` says what state a node should reach
- `NodeHardware` says what the node is and what it can do
- Agent environment variables say how the agent should observe and actuate that node

Key environment variables:

- `TELEMETRY_CPU_SOURCE`: telemetry backend for CPU (`host`, `http`)
- `TELEMETRY_CPU_CONTROL`: control backend for CPU (`host`, `http`)
- `TELEMETRY_GPU_SOURCE`: telemetry backend for GPU (`host`, `http`)
- `TELEMETRY_GPU_CONTROL`: control backend for GPU (`host`, `http`)

Default is `host` backends (real node interfaces). For simulator/KWOK setups, set these to `http` with the appropriate endpoint environment variables pointing at the simulator service (e.g., `http://joulie-telemetry-sim.joulie-sim-demo.svc.cluster.local`).

Control feedback is written to `NodeTwin.status.controlStatus` by the agent.

The full runtime contract, backend types, HTTP payloads, and status semantics are documented in [Input Telemetry and Actuation Interfaces]({{< relref "/docs/architecture/telemetry.md" >}}).

## End-to-end contract flow

1. User submits workload with Kubernetes scheduling constraints.
2. Scheduler places pods according to available node labels.
3. The controller manager observes demand/supply and computes new node targets.
4. Agent publishes `NodeHardware`.
5. The controller manager resolves discovered hardware against the inventory.
6. The controller manager writes `NodeTwin` and updates node supply labels.
7. Agent enforces controls and reports status/metrics.

## Next step

Read [Joulie Controller Manager]({{< relref "/docs/architecture/controller-manager.md" >}}) for reconcile behavior and transition guards.
