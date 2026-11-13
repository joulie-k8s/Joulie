+++
title = "Core Concepts"
linkTitle = "Core Concepts"
slug = "core-concepts"
weight = 1
+++

The parts of Joulie and the control loop that ties them together. Read it before the [Quickstart]({{< relref "/docs/getting-started/01-quickstart.md" >}}).

## What Joulie is

Joulie is a Kubernetes-native energy management system that keeps a **digital twin** of every managed node. The twin is fed by telemetry: CPU and GPU power read from Prometheus as the OpenTelemetry hardware metrics `hw.energy` and `hw.power` or as [Kepler](https://github.com/sustainable-computing-io/kepler) energy counters, and per-pod utilization from cAdvisor. It drives two outcomes: power caps and governor presets that the node agent enforces (RAPL, NVML), and scheduling, where the scheduler extender steers new pods toward the nodes with the best energy and performance trade-off.

## Two energy knobs on a node

- **Power caps (throttling).** A hard ceiling on CPU or GPU watts. The biggest savings, but work slows whenever it reaches the ceiling.
- **Governors.** How eagerly a CPU or GPU raises its clocks (the Linux CPU governor and energy-performance preference, the GPU performance level or clock limit). Smaller savings, smaller slowdown.

Performance nodes stay fast; eco nodes get an efficient governor next to their cap. Governor control is off until you enable it ([Governor policy]({{< relref "/docs/architecture/policy.md#governor-policy" >}}), [experiment]({{< relref "/docs/experiments/governor-exploration.md" >}})).

## Why it matters

Joulie makes the scheduler and the controller manager aware of the cluster's power and cooling state, against cooling bottlenecks, rack power overcommit and the carbon cost of flat power profiles.

## Vocabulary

- **Joulie** (the Joulie operator): the whole system, CRDs, controller manager, agent, scheduler extender and kubectl plugin.
- **Controller manager** (`joulie-controller-manager`): the single cluster-scoped binary that hosts Joulie's controllers (twin, policy, facility metrics), like `kube-controller-manager` hosts the built-in ones.
- **Agent** (`joulie-agent`): the per-node DaemonSet, Joulie's counterpart of the kubelet.
- **Scheduler extender** (`joulie-scheduler-extender`): the HTTP service configured under `extenders` in the kube-scheduler configuration.

Releases before v0.2.0 called the controller manager "the operator" ([Upgrading]({{< relref "/docs/getting-started/06-upgrading.md" >}})).

## Main components

| Component | Binary or path | Role |
|-----------|----------------|------|
| Controller manager | `cmd/controller-manager` | Runs the twin, computes `NodeTwin.status`, decides each node's profile and caps, writes them to `NodeTwin.spec` |
| Agent | `cmd/agent` | Discovers the node's hardware into `NodeHardware`, enforces `NodeTwin.spec` (RAPL, NVML), reports back in `NodeTwin.status.controlStatus` |
| Scheduler extender | `cmd/scheduler` | Reads `NodeTwin.status` (30 s cache), rejects eco nodes for performance pods, scores nodes by power headroom and facility stress |
| kubectl plugin | `cmd/kubectl-joulie` | `kubectl joulie status` and the live dashboard `kubectl joulie top` |
| Simulator | `simulator/` | Repeatable experiments without real hardware |

## Key CRDs

| CRD | Owner | Purpose |
|-----|-------|---------|
| `NodeHardware` | Agent | Hardware facts: CPU/GPU model, cap ranges, frequency landmarks |
| `NodeTwin` | Controller manager | Desired state (spec: power cap %) + twin output (status: headroom, cooling stress, PSU stress, schedulable class, control feedback) |

Pods state their placement intent with the `joulie.io/workload-class` annotation, and the controller manager labels each node `joulie.io/power-profile`: see [Pod Compatibility]({{< relref "/docs/getting-started/03-pod-compatibility.md" >}}).

## Digital twin

A lightweight O(1) parametric model that predicts the effect of placement and cap decisions. For each managed node it writes three scores to `NodeTwin.status`, which the scheduler extender caches for 30 seconds:

| Score | Range | Meaning |
|-------|-------|---------|
| Power headroom | 0-100 | Power budget left before thermal or PSU limits; higher is better for new pods |
| CoolingStress | 0-100 | Predicted share of cooling capacity in use |
| PSUStress | 0-100 | Predicted share of PDU or rack power capacity in use |

Inputs and how each score is used: [Digital Twin]({{< relref "/docs/architecture/digital-twin.md" >}}).

## Digital twin feedback loop

```
telemetry (RAPL/NVML as OTel hw.* metrics, or Kepler; cAdvisor)
  → twin update (NodeTwin.status: headroom, coolingStress, psuStress)
    → cap decisions (NodeTwin.spec)
    → pod placement steering (scheduler extender)
      → updated telemetry
```

## Next step

Install Joulie with the [Quickstart]({{< relref "/docs/getting-started/01-quickstart.md" >}}); the [Architecture]({{< relref "/docs/architecture/_index.md" >}}) pages say how each component works.
