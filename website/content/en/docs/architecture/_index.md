---
title: "Architecture"
linkTitle: "Architecture"
weight: 20
---

How Joulie's components turn node telemetry into power caps and placement decisions. New to Joulie? Read [Core Concepts]({{< relref "/docs/getting-started/00-core-concepts.md" >}}) and the [Quickstart]({{< relref "/docs/getting-started/01-quickstart.md" >}}) first.

## Core story

1. The **agent** discovers each node's hardware and publishes it as a `NodeHardware` object.
2. The **controller manager** reads `NodeHardware` and measured power from Prometheus ([Input metrics (OpenTelemetry)]({{< relref "/docs/architecture/telemetry.md#input-metrics-opentelemetry" >}})), runs the digital twin, and writes `NodeTwin.status`: headroom, cooling stress, PSU stress.
3. Its policy plans which nodes run `performance` or `eco`, and writes `NodeTwin.spec` and the `joulie.io/power-profile` node label.
4. The agent enforces `NodeTwin.spec` through RAPL (CPU) and NVML or sysfs (GPU), and reports the result in `NodeTwin.status.controlStatus`.
5. The **scheduler extender** reads `NodeTwin.status` to filter and score nodes for each new pod. The next reconcile reads the new telemetry, closing the loop.

<img src='{{< relURL "images/joulie-arch.png" >}}' alt="Joulie architecture overview">

## Components

| Component | Role | Reads | Writes |
|---|---|---|---|
| Agent | node-side discovery and enforcement | `NodeTwin.spec`, host RAPL, cpufreq and GPU interfaces (or the simulator over HTTP) | `NodeHardware`, `NodeTwin.status.controlStatus`, caps and governors on the node |
| Controller manager | twin and policy: what each node should be | nodes, pods, `NodeHardware`, Prometheus | `NodeTwin.spec`, `NodeTwin.status`, `joulie.io/power-profile` label |
| Scheduler extender | filter and score nodes for a pod | `NodeTwin`, `NodeHardware`, the pod | nothing (read-only) |
| `kubectl joulie` plugin | the cluster's energy state in the terminal ([commands]({{< relref "/docs/getting-started/05-configuration-reference.md#kubectl-plugin" >}})) | `NodeTwin`, `NodeHardware` | nothing |

## Key CRDs

| CRD | Owner | Purpose |
|-----|-------|---------|
| `NodeHardware` | Agent | Hardware facts: CPU/GPU model, cap ranges, frequency landmarks, governors |
| `NodeTwin` | Controller manager and agent | Desired state (`spec`: profile, caps, governor presets) and twin output (`status`: headroom, stress scores, control feedback) |

### Who writes what

| Object and field | Writer |
|---|---|
| `NodeHardware` (all of it) | Agent |
| `NodeTwin.spec` | Controller manager (policy) |
| `NodeTwin.status.controlStatus` | Agent |
| every other `NodeTwin.status` field, `status.conditions` included | Controller manager (twin) |

Each write carries a field manager (`joulie-controller-manager` or `joulie-agent`), so `kubectl get nodetwin <node> -o yaml --show-managed-fields` shows who owns each field. The per-field table and the rules writers follow are on the wiki page [CRD contracts](https://github.com/joulie-k8s/Joulie/wiki/CRD-Contracts).

## Read in this order

1. [CRD and Policy Model]({{< relref "/docs/architecture/policy.md" >}}): the `NodeHardware` and `NodeTwin` fields
2. [Joulie Controller Manager]({{< relref "/docs/architecture/controller-manager.md" >}}): reconcile, policy, facility poller
3. [Joulie Agent]({{< relref "/docs/architecture/agent.md" >}}): discovery and cap enforcement
4. [Digital Twin]({{< relref "/docs/architecture/digital-twin.md" >}}): headroom, cooling stress, PSU stress
5. [Scheduler Extender]({{< relref "/docs/architecture/scheduler.md" >}}): filter, score, deployment
6. [Energy-Aware Scheduling]({{< relref "/docs/architecture/energy-aware-scheduling.md" >}}): the pipeline end to end
7. [Policy Algorithms]({{< relref "/docs/architecture/policies.md" >}}): the policies and their knobs
8. [Input Telemetry and Actuation Interfaces]({{< relref "/docs/architecture/telemetry.md" >}}): Prometheus inputs and control backends
9. [Hardware Modeling and Physical Power Model]({{< relref "/docs/hardware/hardware-modeling.md" >}}): CPU and GPU power curves for simulation
10. [Metrics Reference]({{< relref "/docs/architecture/metrics.md" >}}): every exported metric

Detail on the contracts: [CRD contracts](https://github.com/joulie-k8s/Joulie/wiki/CRD-Contracts) on the wiki.
