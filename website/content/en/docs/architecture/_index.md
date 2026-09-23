---
title: "Architecture"
linkTitle: "Architecture"
weight: 20
---

This section explains how Joulie's per-node digital twins turn telemetry into enforcement decisions, covering every component from the node agent through the scheduler extender.

If you are new, first read:

1. [Core Concepts]({{< relref "/docs/getting-started/00-core-concepts.md" >}})
2. [Quickstart]({{< relref "/docs/getting-started/01-quickstart.md" >}})

## Core story

1. **Agent** discovers node hardware (CPU/GPU models, cap ranges, frequency landmarks) and publishes a single `NodeHardware` CR per node.
2. **Twin controller** (in the controller manager) ingests `NodeHardware` + Prometheus telemetry, runs the digital twin model, and writes `NodeTwin.status` per node (headroom, cooling stress, PSU stress).
3. **Policy controller** (in the controller manager) reads `NodeTwin.status` + demand signals, runs a policy algorithm, writes `NodeTwin.spec` and node supply labels (`joulie.io/power-profile`). Transition state is tracked internally via `NodeTwin.status.schedulableClass`.
4. **Agent** reads `NodeTwin.spec` and enforces power caps via RAPL (CPU) and NVML (GPU). Writes control feedback to `NodeTwin.status.controlStatus`.
5. **Scheduler extender** reads `NodeTwin.status` and filters/scores nodes at pod scheduling time based on power profile, facility stress, and workload class.
6. Telemetry and status feed the next reconcile step, closing the loop.

<img src='{{< relURL "images/joulie-arch.png" >}}' alt="Joulie architecture overview">

## Key CRDs

| CRD | Owner | Purpose |
|-----|-------|---------|
| `NodeHardware` | Agent | Hardware facts: CPU/GPU model, cap ranges, frequency landmarks |
| `NodeTwin` | Controller manager | Desired state (spec: power cap %) + twin output (status: headroom, cooling stress, PSU stress, control feedback) |

### Who writes what

`NodeTwin` is written by two components, so ownership is defined per field. Every write carries a field manager name (`joulie-controller-manager`, `joulie-agent`), so `kubectl get nodetwin <node> -o yaml --show-managed-fields` shows the owner of each field, and contract tests assert that no writer touches another owner's fields.

| Object | Field | Owner | Meaning |
|--------|-------|-------|---------|
| `NodeHardware` | `spec.nodeName` | Agent | which node this describes |
| `NodeHardware` | `status.*` | Agent | discovered hardware facts |
| `NodeTwin` | `spec.*` | Controller manager (policy) | desired profile, caps, policy name, draining |
| `NodeTwin` | `status.controlStatus.*` | Agent | what was applied on the node, per component |
| `NodeTwin` | every other `status` field | Controller manager (twin) | the twin's computed state: measured power, headroom, stress scores, PUE |

Rules that follow from the table:

- The controller manager never writes `status.controlStatus`, and the agent never writes anything outside it. Both are patch-shaped so a violation is visible in the payload, not only in the result.
- Both components derive the object name from the node name with the same function (`pkg/api.ObjectNameForNode`), so a node whose name is not a valid object name still maps to one object.
- Writers skip a write when nothing changed. A write bumps `resourceVersion` and wakes every watcher, so idle nodes must not generate traffic. Unchanged state is rewritten at most every five minutes, which bounds how long a lost object stays unrepaired.
- `status` holds the twin's current computed state, forecasts included. That is what a twin is; the CRD field descriptions say so.

## Component roles

### Controller manager

The controller manager contains two reconcile-loop controllers and one background controller:

**Reconcile-loop controllers** (run each tick):

- **Twin controller**: ingests per-node telemetry into `NodeTwin.status`. Runs the cooling stress and PSU stress computations. Incorporates facility metrics (ambient temperature, PUE) when available. When nodes carry `joulie.io/rack` or `joulie.io/cooling-zone` labels, the twin computes PSU stress per-rack and cooling stress with per-zone ambient temperature.
- **Policy controller**: reads `NodeTwin.status` + pod demand signals, runs the policy algorithm (`pkg/controller/policy/`), writes `NodeTwin.spec` and the `joulie.io/power-profile` node label. The state machine (`pkg/controller/fsm/`) enforces downgrade guards: nodes cannot transition from performance to eco while performance-sensitive pods are still running. Transition state is tracked via `NodeTwin.status.schedulableClass`.

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

The `pkg/controller/twin` package implements an O(1) parametric model computing:
- **Power headroom**: remaining capacity before hitting the configured cap
- **Cooling stress** (0-100): predicted % of cooling capacity in use. High means risk of thermal throttling.
- **PSU stress** (0-100): predicted % of PDU/rack power capacity in use. High means risk of power brownout.

All three start from a measured node power reading supplied by the controller manager. Cooling stress is a single closed-form expression in `computeCoolingStress`: measured power over node TDP, scaled by an ambient temperature multiplier. There is no cooling model interface and no alternative implementation to select.

## Read in this order

1. [CRD and Policy Model]({{< relref "/docs/architecture/policy.md" >}}) -- NodeHardware and NodeTwin CRDs, policy state machine
2. [Joulie Controller Manager]({{< relref "/docs/architecture/controller-manager.md" >}}) -- twin controller, policy controller, facility metrics poller
3. [Joulie Agent]({{< relref "/docs/architecture/agent.md" >}}) -- hardware discovery, cap enforcement via RAPL/NVML
4. [Digital Twin]({{< relref "/docs/architecture/digital-twin.md" >}}) -- O(1) parametric model: headroom, cooling stress, PSU stress
5. [Scheduler Extender]({{< relref "/docs/architecture/scheduler.md" >}}) -- filter and prioritize endpoints, scoring formula
6. [Energy-Aware Scheduling]({{< relref "/docs/architecture/energy-aware-scheduling.md" >}}) -- workload-class-aware pod placement
7. [Policy Algorithms]({{< relref "/docs/architecture/policies.md" >}}) -- available policy implementations and tuning
8. [Input Telemetry and Actuation Interfaces]({{< relref "/docs/architecture/telemetry.md" >}}) -- Prometheus queries, RAPL/NVML control paths
9. [Hardware Modeling and Physical Power Model]({{< relref "/docs/hardware/hardware-modeling.md" >}}) -- CPU/GPU power curves for simulation
10. [Metrics Reference]({{< relref "/docs/architecture/metrics.md" >}}) -- all exported Prometheus metrics
