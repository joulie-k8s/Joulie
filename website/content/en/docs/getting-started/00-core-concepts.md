+++
title = "Core Concepts"
linkTitle = "Core Concepts"
slug = "core-concepts"
weight = 1
+++

Before installing Joulie, understand the control model.

## What Joulie is

Joulie is a Kubernetes-native energy management system that uses **per-node digital twins** to optimize data center power consumption.

It continuously ingests telemetry from every node (CPU/GPU power draw via RAPL and NVML/DCGM, per-pod resource utilization via cAdvisor, and optional energy counters from [Kepler](https://github.com/sustainable-computing-io/kepler)) to maintain an up-to-date model of each node's thermal and power state.

These per-node digital twins drive two outcomes:

1. **Energy control**: the controller manager writes desired power state into `NodeTwin` CRs (CPU and GPU power caps). The node agent reads `NodeTwin.spec` and enforces them.
2. **Scheduling decisions**: the scheduler extender reads computed `NodeTwin.status` (power headroom, predicted cooling stress, PSU load) to steer new pods toward nodes with the best energy-efficiency / performance trade-off.

The feedback loop: telemetry → twin update → cap decisions → new pod placement → updated telemetry. This keeps the cluster's power envelope stable and prevents cooling or PSU spikes without sacrificing critical workload performance.

## Why it matters

As AI and scientific workloads scale, clusters face:
- **Cooling bottlenecks**: GPU-dense racks exceed cooling capacity during training bursts
- **PSU/PDU overcommit**: peak power draw exceeds rack power budgets
- **Carbon cost**: flat power profiles waste energy during low-demand periods

Joulie addresses these by making the scheduler and controller manager aware of the physical energy state of the cluster in real time.

## Vocabulary

In Kubernetes an *operator* is a whole extension: custom resources plus the controllers that act on them. Joulie uses the word that way:

- **Joulie operator**, or just **Joulie**: the whole system (CRDs, controller manager, agent, scheduler extender, kubectl plugin).
- **Controller manager** (`joulie-controller-manager`): the single cluster-scoped binary that hosts Joulie's controllers (twin, policy, facility metrics), like `kube-controller-manager` hosts the built-in ones.
- **Agent** (`joulie-agent`): the per-node DaemonSet, Joulie's counterpart of the kubelet as the primary node agent.
- **Scheduler extender** (`joulie-scheduler-extender`): the HTTP service configured under `extenders` in the kube-scheduler configuration.

Older releases and dashboards called the controller manager "the operator"; its image, Deployment, values key and metric names carried that name and are kept as deprecated aliases for one release.

## Main components

- **Controller manager** (`cmd/controller-manager`): cluster-level decision engine and twin controller
  - runs the digital twin model, computes `NodeTwin.status`
  - decides desired node power profile/cap assignments
  - writes desired state into `NodeTwin.spec`
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
- **Simulator** (`simulator/`): digital-twin execution environment
  - enables repeatable experiments without requiring real hardware

## Key CRDs

| CRD | Owner | Purpose |
|-----|-------|---------|
| `NodeHardware` | Agent | Hardware facts: CPU/GPU model, cap ranges, frequency landmarks |
| `NodeTwin` | Controller manager | Desired state (spec: power cap %) + twin output (status: headroom, cooling stress, PSU stress, schedulable class, control feedback) |

## Node supply labels

The controller manager sets one label on each managed node:

- `joulie.io/power-profile`: `performance` or `eco`

This label reflects the current power state of the node. Users do not need to interact with it directly for placement. The `joulie.io/workload-class` annotation on pods is the single source of truth for placement intent.

## Workload classes

Joulie uses a single `joulie.io/workload-class` pod annotation to drive placement:

| Class | Scheduler behavior |
|-------|-------------------|
| `performance` | Hard-rejected from eco nodes. Must run on full-power nodes. |
| `standard` | Default. Can run on any node. Adaptive scoring steers toward eco when performance nodes are congested. |

## Digital twin

The digital twin is the core predictive engine of Joulie. It is a lightweight O(1) parametric model that predicts the impact of scheduling and power-cap decisions without running a full simulation for each scheduling decision.

For each managed node, the twin produces three scores written to `NodeTwin.status`:

- **Power headroom** (0-100): remaining power budget before hitting thermal or PSU limits. Higher is better for new workload placement.
- **CoolingStress** (0-100): predicted percentage of cooling capacity in use. High values mean the node is near its thermal limit.
- **PSUStress** (0-100): predicted percentage of PDU/rack power capacity in use. High values mean the rack is near its power supply limit.

The scheduler extender caches these scores (30-second TTL) and uses them for filter and score decisions.

For formula details and the pluggable `CoolingModel` interface, see [Digital Twin]({{< relref "/docs/architecture/digital-twin.md" >}}).

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
