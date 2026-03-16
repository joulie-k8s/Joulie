+++
title = "Core Concepts"
linkTitle = "Core Concepts"
slug = "core-concepts"
weight = 1
+++

Before installing Joulie, understand the control model.

## What Joulie is

Joulie is a Kubernetes-native **digital twin** for energy-efficient data centers.

It continuously ingests telemetry from every node (CPU/GPU power draw via RAPL and NVML/DCGM, per-pod resource utilization via cAdvisor, and optional energy counters from [Kepler](https://github.com/sustainable-computing-io/kepler)) to maintain an up-to-date model of each node's thermal and power state.

That digital twin model drives two outcomes:

1. **Energy control**: the operator writes desired power state into `NodeTwin` CRs (CPU and GPU power caps). The node agent reads `NodeTwin.spec` and enforces them.
2. **Scheduling decisions**: the scheduler extender reads computed `NodeTwin.status` (power headroom, predicted cooling stress, PSU load) to steer new pods toward nodes with the best energy-efficiency / performance trade-off.

The feedback loop: telemetry → twin update → cap decisions → new pod placement → updated telemetry. This keeps the cluster's power envelope stable and prevents cooling or PSU spikes without sacrificing critical workload performance.

## Why it matters

As AI and scientific workloads scale, clusters face:
- **Cooling bottlenecks**: GPU-dense racks exceed cooling capacity during training bursts
- **PSU/PDU overcommit**: peak power draw exceeds rack power budgets
- **Carbon cost**: flat power profiles waste energy during low-demand periods

Joulie addresses these by making the scheduler and operator aware of the physical energy state of the cluster in real time.

## Main components

- **Operator** (`cmd/operator`): cluster-level decision engine and twin controller
  - runs the digital twin model, computes `NodeTwin.status`
  - decides desired node power profile/cap assignments
  - writes desired state into `NodeTwin.spec`
  - triggers pod migration under thermal/PSU pressure
- **Agent** (`cmd/agent`): node-level actuator
  - discovers local CPU/GPU hardware and capability
  - publishes discovered hardware as `NodeHardware`
  - reads `NodeTwin.spec` for desired state and enforces power controls (RAPL/NVML)
  - writes control feedback to `NodeTwin.status.controlStatus`
- **Scheduler extender** (`cmd/scheduler`): placement steering
  - reads `NodeTwin.status` (30s TTL cache)
  - rejects eco nodes for performance pods
  - scores nodes by power headroom and facility stress
- **kubectl plugin** (`cmd/kubectl-joulie`): cluster observability
  - `kubectl joulie status` shows per-node energy state, power profiles, and cap settings
  - `kubectl joulie recommend` surfaces GPU slicing and reschedule recommendations
- **Simulator** (`simulator/`): digital-twin execution environment
  - enables repeatable experiments without requiring real hardware

## Key CRDs

| CRD | Owner | Purpose |
|-----|-------|---------|
| `NodeHardware` | Agent | Hardware facts: CPU/GPU model, cap ranges, frequency landmarks, GPU slicing modes |
| `NodeTwin` | Operator | Desired state (spec: power cap %) + twin output (status: headroom, cooling stress, PSU stress, schedulable class, migration recommendations, GPU slicing recommendations, control feedback) |

The operator also creates `WorkloadProfile` CRs internally to classify workloads. These are managed automatically. You only interact with them indirectly via the `joulie.io/workload-class` pod annotation.

## Node supply labels

The operator sets one label on each managed node:

- `joulie.io/power-profile`: `performance` or `eco`

This label reflects the current power state of the node. Users do not need to interact with it directly for placement. The `joulie.io/workload-class` annotation on pods is the single source of truth for placement intent.

## Workload classes

Joulie uses a single `joulie.io/workload-class` pod annotation to drive placement:

| Class | Scheduler behavior |
|-------|-------------------|
| `performance` | Hard-rejected from eco nodes. Must run on full-power nodes. |
| `standard` | Default. Can run on any node. Adaptive scoring steers toward eco when performance nodes are congested. |

## Digital twin

The digital twin is the core predictive engine of Joulie. It is an O(1) parametric model that predicts the impact of scheduling and power-cap decisions without running a full simulation for each scheduling decision.

### What it computes

For each managed node, the twin produces three scores:

- **Power headroom** (0-100): how much power budget remains before hitting thermal or PSU limits. Higher is better for new workload placement.
- **CoolingStress** (0-100): predicted percentage of cooling capacity in use. High values mean the node is near its thermal limit and risks throttling.
- **PSUStress** (0-100): predicted percentage of PDU/rack power capacity in use. High values mean the rack is near its power supply limit.

### CoolingStress formula

The default `LinearCoolingModel` computes:

```
coolingStress = (nodePower / referenceNodePower) * 80 + max(0, temp - 20) * 0.5
```

Where `referenceNodePower` defaults to 4000 W (a 2-socket EPYC + 8x H100 reference node). The result is clamped to [0, 100].

### PSUStress formula

```
psuStress = clusterPower / referenceRackCapacity * 100
```

Where `referenceRackCapacity` defaults to 50 kW. The result is clamped to [0, 100].

### CoolingModel interface

The `CoolingModel` interface is pluggable. The default implementation is `LinearCoolingModel`, an algebraic proxy suitable for initial deployments. A future implementation will use openModelica reduced-order thermal simulation via the same interface for higher-fidelity predictions.

### How it feeds the scheduler

The twin outputs are written to `NodeTwin.status` (one per managed node). The scheduler extender caches these with a 30-second TTL and uses them in its filter and score logic to steer pods toward nodes with the best energy/performance trade-off.

### How it feeds the operator

When CoolingStress or PSUStress exceeds 70 on a node, the twin generates reschedule recommendations for reschedulable standard workloads, enabling the operator to trigger migration away from stressed nodes.

For GPU nodes with slicing support, the twin also produces GPU slicing recommendations (MIG or time-slicing) based on observed workload intensity patterns. These are advisory: cluster admins review and apply them during maintenance windows.

### Implementation

The twin is implemented in `pkg/operator/twin/twin.go`.

## Digital twin feedback loop

```
telemetry (RAPL/NVML/cAdvisor/Kepler)
  → twin update (NodeTwin.status: headroom, coolingStress, psuStress)
    → cap decisions (NodeTwin.spec)
    → pod placement steering (scheduler extender)
      → updated telemetry
```

## Next step

Proceed to [Quickstart]({{< relref "/docs/getting-started/01-quickstart.md" >}}), then use Architecture pages for deeper details.
