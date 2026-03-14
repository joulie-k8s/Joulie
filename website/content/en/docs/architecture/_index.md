---
title: "Architecture"
linkTitle: "Architecture"
weight: 20
---

Architecture explains how Joulie's digital twin turns telemetry into enforcement decisions.

If you are new, first read:

1. [Core Concepts]({{< relref "/docs/getting-started/00-core-concepts.md" >}})
2. [Quickstart]({{< relref "/docs/getting-started/01-quickstart.md" >}})

## Core story

1. **Agent** discovers node hardware (CPU/GPU models, cap ranges, frequency landmarks, GPU slicing modes) and publishes a single `NodeHardware` CR per node.
2. **Operator twin controller** ingests `NodeHardware` + Prometheus telemetry, runs the digital twin model, and writes `NodeTwinState` per node (headroom, cooling stress, PSU stress).
3. **Operator policy controller** reads `NodeTwinState` + demand signals, runs a policy algorithm, writes `NodePowerProfile` and node supply labels (`joulie.io/power-profile`). Transition state is tracked internally via `NodeTwinState.schedulableClass`.
4. **Agent** reads `NodePowerProfile` and enforces power caps via RAPL (CPU) and NVML (GPU).
5. **Scheduler extender** reads `NodeTwinState` and filters/scores nodes at pod scheduling time based on power profile, facility stress, and workload class.
6. Telemetry and status feed the next reconcile step, closing the loop.

<img src='{{< relURL "images/joulie-arch.png" >}}' alt="Joulie architecture overview">

## Key CRDs

| CRD | Owner | Purpose |
|-----|-------|---------|
| `NodeHardware` | Agent | Hardware facts: CPU/GPU model, cap ranges, frequency landmarks, GPU slicing modes |
| `NodePowerProfile` | Operator/user | Desired state: power cap % |
| `NodeTwinState` | Operator | Twin output: headroom score, cooling stress, PSU stress, migration recommendations |
| `WorkloadProfile` | Operator (classifier) | Per-pod: workload class, CPU/GPU intensity, cap sensitivity |

## Component roles

### Operator

The operator contains three controllers that share the same reconcile entry point:

- **Twin controller**: ingests per-node telemetry into `NodeTwinState`. Runs the `CoolingModel` and PSU stress computations. Makes facility stress signals available to the scheduler extender.
- **Policy controller**: reads `NodeTwinState` + pod demand signals, runs the policy algorithm, writes `NodePowerProfile` and the `joulie.io/power-profile` node label. Transition state is tracked internally via `NodeTwinState.schedulableClass`.

### Agent

The agent is the node-side enforcement component.
It discovers local hardware, publishes `NodeHardware`, reads `NodePowerProfile`, and applies CPU and GPU controls through configured backends (RAPL for CPU, NVML for GPU).

### Scheduler extender

The scheduler extender is a read-only HTTP service that participates in the Kubernetes scheduling cycle.

- **Filter**: rejects eco nodes for performance pods (hard rule).
- **Score**: ranks nodes using `score = headroom*0.4 + (100-coolingStress)*0.3 + (100-psuStress)*0.3`, with workload-class adjustments (best-effort +5 on eco, draining -20).

### Digital twin model

The `pkg/operator/twin` package implements an O(1) parametric model computing:
- **Power headroom**: remaining capacity before hitting the configured cap
- **Cooling stress** (0–100): predicted % of cooling capacity in use. High → risk of thermal throttling.
- **PSU stress** (0–100): predicted % of PDU/rack power capacity in use. High → risk of power brownout.

The `CoolingModel` interface is pluggable. Default: `LinearCoolingModel` (algebraic proxy). Future: openModelica reduced-order thermal simulation via the same interface.

## Read in this order

1. [CRD and Policy Model]({{< relref "/docs/architecture/policy.md" >}})
2. [Joulie Operator]({{< relref "/docs/architecture/operator.md" >}})
3. [Joulie Agent]({{< relref "/docs/architecture/agent.md" >}})
4. [Digital Twin]({{< relref "/docs/architecture/digital-twin.md" >}})
5. [Scheduler Extender]({{< relref "/docs/architecture/scheduler.md" >}})
6. [Policy Algorithms]({{< relref "/docs/architecture/policies.md" >}})
7. [Input Telemetry and Actuation Interfaces]({{< relref "/docs/architecture/telemetry.md" >}})
8. [Hardware Modeling and Physical Power Model]({{< relref "/docs/hardware/hardware-modeling.md" >}})
9. [Metrics Reference]({{< relref "/docs/architecture/metrics.md" >}})
