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

Workload class is determined from the `joulie.io/workload-class` pod annotation or from a matching `WorkloadProfile`:

- `performance` demand: pod carries `joulie.io/workload-class: performance` or a matching profile with `criticality: performance`.
- `best-effort` demand: pod carries `joulie.io/workload-class: best-effort` or a matching profile with `criticality: best-effort`.
- `standard` demand (default): no annotation, or `joulie.io/workload-class: standard`. No class-specific adjustments.

## Supply model (nodes)

Node supply is represented by:

- `joulie.io/power-profile=performance|eco` (node label, set by operator)
- `NodeTwin.status.schedulableClass` (internal, set by operator twin controller)

Semantics:

- `performance`: full-performance supply
- `eco`: low-power supply
- `draining` (schedulableClass only): transition safeguard active while node is moving toward eco; the scheduler extender applies a score penalty

The `schedulableClass` field is internal to the operator and scheduler extender. Users interact only with the `joulie.io/workload-class` pod annotation for placement intent.

## Desired-state + twin output: `NodeTwin`

`NodeTwin` is the operator-to-agent contract for one node. The `spec` carries desired state; the `status` carries twin output (including control feedback and schedulable class).

Main spec fields:

- `spec.nodeName` (required)
- `spec.profile` (required, `performance|eco`)
- `spec.cpu.packagePowerCapWatts` (optional, absolute package cap)
- `spec.cpu.packagePowerCapPctOfMax` (optional, normalized policy intent)
- `spec.gpu.powerCap` (optional):
  - `scope` (`perGpu`)
  - `capWattsPerGpu` (absolute per-GPU cap)
  - `capPctOfMax` (percentage of node GPU max power)
- `spec.policy.name` (optional, provenance/debug)

Resolution/precedence in agent runtime:

1. CPU: `packagePowerCapWatts` if present, otherwise `packagePowerCapPctOfMax`
2. GPU: `capWattsPerGpu` if present, otherwise `capPctOfMax`

What the operator typically writes today:

- CPU intent is commonly emitted as `packagePowerCapPctOfMax`.
- GPU intent may be emitted as:
  - `capPctOfMax` only, when the agent is expected to resolve percentage to watts from device limits,
  - `capWattsPerGpu` plus `capPctOfMax`, when the operator has deterministic model-based mapping available and wants the absolute target to be explicit.

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
    packagePowerCapPctOfMax: 60
  gpu:
    powerCap:
      scope: perGpu
      capPctOfMax: 60
      capWattsPerGpu: 210
  policy:
    name: static-partition-v1
```

In this example:

- CPU is expressed as a normalized percentage of max package power.
- GPU is expressed as both percentage and resolved absolute watts per GPU.
- The agent applies the absolute GPU cap when present and falls back to percentage only when absolute watts are not provided.

## Hardware-discovery object: `NodeHardware`

`NodeHardware` is the agent-to-operator contract for discovered node capabilities.

It is status-oriented and agent-owned.
Users should not normally create it by hand.

Main fields:

- `spec.nodeName`
- `status.cpu`:
  - raw model string
  - normalized inventory model when recognized
  - sockets / total cores / cores per socket
  - runtime CPU cap range when available
  - control and telemetry availability
- `status.gpu`:
  - raw model string
  - normalized inventory model when recognized
  - GPU count
  - runtime GPU cap range when available
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

The operator uses `NodeHardware` as the source of truth for:

- hardware recognition against the inventory,
- compute-density-aware planning,
- per-device fallback when only part of the node is recognized.

In simulator-first setups, the operator can fall back to node hardware labels when `NodeHardware` has not been published yet.
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
3. Operator observes demand/supply and computes new node targets.
4. Agent publishes `NodeHardware`.
5. Operator resolves discovered hardware against the inventory.
6. Operator writes `NodeTwin` and updates node supply labels.
7. Agent enforces controls and reports status/metrics.

## Next step

Read [Joulie Operator]({{< relref "/docs/architecture/operator.md" >}}) for reconcile behavior and transition guards.
