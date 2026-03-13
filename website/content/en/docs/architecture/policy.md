---
title: "CRD and Policy Model"
weight: 10
---

This page defines Joulie's core contract:

- **demand** comes from pod scheduling constraints,
- **supply** is exposed by node power-profile labels,
- **discovered hardware** is published through `NodeHardware`,
- **desired state** is published through `NodePowerProfile`.

## APIs

Group/version:

- `joulie.io/v1alpha1`

CRDs:

- `NodeHardware` (`nodehardwares`, cluster-scoped)
- `NodePowerProfile` (`nodepowerprofiles`, cluster-scoped)
- `TelemetryProfile` (`telemetryprofiles`, cluster-scoped)

CRD definitions live in:

- `config/crd/bases/joulie.io_nodehardwares.yaml`
- `config/crd/bases/joulie.io_nodepowerprofiles.yaml`
- `config/crd/bases/joulie.io_telemetryprofiles.yaml`

## Demand model (workloads)

Workload class is inferred from Kubernetes scheduling constraints on key:

- `joulie.io/power-profile`

Classification:

- `performance` demand:
  - pod excludes eco in required scheduling constraints (recommended pattern: `NotIn ["eco"]`)
  - compatibility path: explicit `nodeSelector` `joulie.io/power-profile=performance`
- `eco` demand:
  - pod requires `joulie.io/power-profile=eco`
  - advanced pattern: also exclude `joulie.io/draining=true` with `NotIn ["true"]`
- `general` demand:
  - no explicit power-profile requirement (unconstrained)

Classification source is affinity/selector, not a custom intent label.

## Supply model (nodes)

Node supply is represented by label:

- `joulie.io/power-profile=performance|eco`
- `joulie.io/draining=true|false`

Semantics:

- `performance`: full-performance supply
- `eco`: low-power supply
- `draining=true`: transition safeguard active while node is moving toward eco

For eco-only placement, prefer excluding `joulie.io/draining=true` rather than requiring `draining=false`.
That pattern is more robust because unlabeled nodes remain eligible while actively draining nodes are still excluded.

## Desired-state object: `NodePowerProfile`

`NodePowerProfile` is the operator-to-agent contract for one node.

Main fields:

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
kind: NodePowerProfile
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

## Telemetry/control routing: `TelemetryProfile`

`TelemetryProfile` is the routing contract that tells the agent where telemetry comes from and where control intents should be sent.

At the policy-model level, the important distinction is:

- `NodePowerProfile` says what state a node should reach
- `NodeHardware` says what the node is and what it can do
- `TelemetryProfile` says how the agent should observe and actuate that node

High-level shape:

- `spec.target`: which node or scope the profile applies to
- `spec.sources`: telemetry backends (`cpu`, `gpu`, `thermal`, `context`)
- `spec.controls`: control backends (`cpu`, `gpu`)
- `status.control`: per-control outcome written back by the agent

Example:

```yaml
apiVersion: joulie.io/v1alpha1
kind: TelemetryProfile
metadata:
  name: sim-http-kwok-gpu-nvidia-0
spec:
  target:
    scope: node
    nodeName: kwok-gpu-nvidia-0
  sources:
    cpu:
      type: http
      http:
        endpoint: http://joulie-telemetry-sim.joulie-sim-demo.svc.cluster.local/telemetry/{node}
        timeoutSeconds: 2
  controls:
    cpu:
      type: http
      http:
        endpoint: http://joulie-telemetry-sim.joulie-sim-demo.svc.cluster.local/control/{node}
        timeoutSeconds: 2
        mode: dvfs
    gpu:
      type: http
      http:
        endpoint: http://joulie-telemetry-sim.joulie-sim-demo.svc.cluster.local/control/{node}
        timeoutSeconds: 2
        mode: powercap
```

The full runtime contract, backend types, HTTP payloads, and status semantics are documented in [Input Telemetry and Actuation Interfaces]({{< relref "/docs/architecture/telemetry.md" >}}).

## End-to-end contract flow

1. User submits workload with Kubernetes scheduling constraints.
2. Scheduler places pods according to available node labels.
3. Operator observes demand/supply and computes new node targets.
4. Agent publishes `NodeHardware`.
5. Operator resolves discovered hardware against the inventory.
6. Operator writes `NodePowerProfile` and updates node supply labels.
7. Agent enforces controls and reports status/metrics.

## Next step

Read [Joulie Operator]({{< relref "/docs/architecture/operator.md" >}}) for reconcile behavior and transition guards.
