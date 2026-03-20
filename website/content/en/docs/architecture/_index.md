---
title: "Architecture"
linkTitle: "Architecture"
weight: 20
---

Architecture explains how Joulie's per-node digital twins turn telemetry into enforcement decisions.

If you are new, first read:

1. [Core Concepts]({{< relref "/docs/getting-started/00-core-concepts.md" >}})
2. [Quickstart]({{< relref "/docs/getting-started/01-quickstart.md" >}})

## Core story

1. **Agent** discovers node hardware (CPU/GPU models, cap ranges, frequency landmarks) and publishes a single `NodeHardware` CR per node.
2. **Operator twin controller** ingests `NodeHardware` + Prometheus telemetry, runs the digital twin model, and writes `NodeTwin.status` per node (headroom, cooling stress, PSU stress).
3. **Operator policy controller** reads `NodeTwin.status` + demand signals, runs a policy algorithm, writes `NodeTwin.spec` and node supply labels (`joulie.io/power-profile`). Transition state is tracked internally via `NodeTwin.status.schedulableClass`.
4. **Agent** reads `NodeTwin.spec` and enforces power caps via RAPL (CPU) and NVML (GPU). Writes control feedback to `NodeTwin.status.controlStatus`.
5. **Scheduler extender** reads `NodeTwin.status` and filters/scores nodes at pod scheduling time based on power profile, facility stress, and workload class.
6. Telemetry and status feed the next reconcile step, closing the loop.

<img src='{{< relURL "images/joulie-arch.png" >}}' alt="Joulie architecture overview">

## Key CRDs

| CRD | Owner | Purpose |
|-----|-------|---------|
| `NodeHardware` | Agent | Hardware facts: CPU/GPU model, cap ranges, frequency landmarks |
| `NodeTwin` | Operator | Desired state (spec: power cap %) + twin output (status: headroom, cooling stress, PSU stress, control feedback) |

## Component roles

### Operator

The operator contains two reconcile-loop controllers and one background controller:

**Reconcile-loop controllers** (run each tick):

- **Twin controller**: ingests per-node telemetry into `NodeTwin.status`. Runs the `CoolingModel` and PSU stress computations. Incorporates facility metrics (ambient temperature, PUE) when available. When nodes carry `joulie.io/rack` or `joulie.io/cooling-zone` labels, the twin computes PSU stress per-rack and cooling stress with per-zone ambient temperature.
- **Policy controller**: reads `NodeTwin.status` + pod demand signals, runs the policy algorithm (`pkg/operator/policy/`), writes `NodeTwin.spec` and the `joulie.io/power-profile` node label. The state machine (`pkg/operator/fsm/`) enforces downgrade guards: nodes cannot transition from performance to eco while performance-sensitive pods are still running. Transition state is tracked via `NodeTwin.status.schedulableClass`.

**Background controllers** (run on independent intervals):

- **Facility metrics poller** (`ENABLE_FACILITY_METRICS=false` by default): queries Prometheus for ambient temperature, IT power, and cooling power. Computes PUE for twin and scheduler consumption.

### Agent

The agent is the node-side enforcement component.
It discovers local hardware, publishes `NodeHardware`, reads `NodeTwin.spec`, and applies CPU and GPU controls through configured backends (RAPL for CPU, NVML for GPU). Control feedback is written to `NodeTwin.status.controlStatus`.

### Scheduler extender

The scheduler extender is a read-only HTTP service that participates in the Kubernetes scheduling cycle. When a new pod is created, kube-scheduler calls the extender at two endpoints:

- **Filter** (`POST /filter`): rejects eco and draining nodes for performance pods. Standard pods pass all nodes.
- **Prioritize** (`POST /prioritize`): scores each node 0-100 using `score = headroomScore*0.7 + (100-coolingStress)*0.15 + trendBonus + profileBonus + pressureRelief`.

The score is **pod-specific**: the extender estimates how many watts the pod will add to each node (from its CPU/GPU requests) and subtracts that from the node's capped power budget before scoring. Heavy pods reduce headroom more, naturally steering them toward nodes with more power budget remaining.

### kubectl plugin

The `kubectl joulie` plugin (`cmd/kubectl-joulie`) provides immediate visibility into the cluster's energy state:

- `kubectl joulie status`: per-node overview of power profiles, cap settings, twin stress scores.

No configuration is needed. The plugin reads your current kubeconfig context.

### Digital twin model

The `pkg/operator/twin` package implements an O(1) parametric model computing:
- **Power headroom**: remaining capacity before hitting the configured cap
- **Cooling stress** (0-100): predicted % of cooling capacity in use. High means risk of thermal throttling.
- **PSU stress** (0-100): predicted % of PDU/rack power capacity in use. High means risk of power brownout.

The `CoolingModel` interface is pluggable. Default: `LinearCoolingModel` (algebraic proxy). Future: openModelica reduced-order thermal simulation via the same interface.

## Read in this order

1. [CRD and Policy Model]({{< relref "/docs/architecture/policy.md" >}})
2. [Joulie Operator]({{< relref "/docs/architecture/operator.md" >}})
3. [Joulie Agent]({{< relref "/docs/architecture/agent.md" >}})
4. [Digital Twin]({{< relref "/docs/architecture/digital-twin.md" >}})
5. [Scheduler Extender]({{< relref "/docs/architecture/scheduler.md" >}})
6. [Energy-Aware Scheduling]({{< relref "/docs/architecture/energy-aware-scheduling.md" >}})
7. [Policy Algorithms]({{< relref "/docs/architecture/policies.md" >}})
10. [Input Telemetry and Actuation Interfaces]({{< relref "/docs/architecture/telemetry.md" >}})
11. [Hardware Modeling and Physical Power Model]({{< relref "/docs/hardware/hardware-modeling.md" >}})
12. [Metrics Reference]({{< relref "/docs/architecture/metrics.md" >}})
