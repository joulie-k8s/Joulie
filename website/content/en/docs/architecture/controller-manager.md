---
title: "Joulie Controller Manager"
weight: 20
aliases: ["/docs/architecture/operator/"]
---

The controller manager is Joulie's cluster-level decision engine: it decides which nodes supply `performance` capacity and which can safely supply `eco`, and publishes that through Kubernetes objects and labels. It never writes host power interfaces; the agent does.

## Responsibilities

| Part | Runs | Does |
|---|---|---|
| Twin controller | every `RECONCILE_INTERVAL` (1 min) | reads measured power and `NodeHardware`, writes the twin's scores to `NodeTwin.status` ([Digital Twin]({{< relref "/docs/architecture/digital-twin.md" >}})) |
| Policy controller | every `RECONCILE_INTERVAL` | classifies demand, runs the policy, applies the downgrade guard, writes `NodeTwin.spec` (caps and, with governor control on, governor presets) and the `joulie.io/power-profile` label |
| Facility poller | every `FACILITY_POLL_INTERVAL` (30 s), off by default | reads ambient temperatures, IT, cooling and rack power, carbon intensity ([Measured inputs](#measured-inputs)) |

The controller manager decides **what** each node should be; the agent decides **how** to apply it on the node.

## Reads, writes and leader election

All reads come from an informer cache, so a reconcile touches the API server only when it writes. Writes carry the `joulie-controller-manager` field manager and are skipped when nothing changed. With `LEADER_ELECT=true` (Helm `controllerManager.leaderElection.enabled`) only the holder of the `joulie-controller-manager` lease reconciles; it is off by default, matching the chart's single replica.

## Measured inputs

The controller manager is the only component that queries Prometheus: node power every reconcile (`NODE_POWER_SOURCE=prometheus`, the chart default) and facility metrics when `ENABLE_FACILITY_METRICS=true`. The metric names are in [Input metrics (OpenTelemetry)]({{< relref "/docs/architecture/telemetry.md#input-metrics-opentelemetry" >}}), the variables in [Measured node power]({{< relref "/docs/getting-started/05-configuration-reference.md#measured-node-power" >}}).

## Reconcile flow

Each tick it reads the nodes matching `NODE_SELECTOR` (chart: `joulie.io/managed=true`), skips reserved and unschedulable ones, builds a hardware view from `NodeHardware` (or node labels before an agent publishes), counts performance demand from pod annotations, runs the policy (`POLICY_TYPE`), holds planned downgrades as `draining` while performance pods still run, and writes `NodeTwin.spec` and the node label.

```bash
kubectl get nodetwins
kubectl get nodes -L joulie.io/power-profile
```

## Power intent and governors

Cap intent is set by the variables under [Power cap configuration]({{< relref "/docs/getting-started/05-configuration-reference.md#power-cap-configuration" >}}): percentages by default, absolute watts with `CPU_WRITE_ABSOLUTE_CAPS` or `GPU_WRITE_ABSOLUTE_CAPS`. Governor presets are off by default; see [Governor control]({{< relref "/docs/getting-started/05-configuration-reference.md#governor-control" >}}) and [Governor policy]({{< relref "/docs/architecture/policy.md#governor-policy" >}}).

## Heterogeneous planning

The policies `static_partition` and `queue_aware_v1` decide how many nodes stay in `performance`; the controller manager then splits those slots across hardware families (`gpu:<model>` or `cpu:<model>`) in proportion to family size, at least one per family. Nodes already in `performance` are kept first, so a reconcile at constant demand moves nothing. The split is exported as `joulie_policy_family_nodes{family}` and `joulie_policy_family_performance_nodes{family}` ([Metrics]({{< relref "/docs/architecture/metrics.md" >}})).

Before an agent publishes `NodeHardware`, the hardware view comes from labels such as `joulie.io/hw.cpu-model`, `joulie.io/hw.gpu-model`, `nvidia.com/gpu.product` and from allocatable GPU resources ([Shared and partitioned GPUs]({{< relref "/docs/hardware/gpus.md#shared-and-partitioned-gpus" >}})).

## Node state model

| FSM state | Profile label | `schedulableClass` | Caps | Performance pods |
|---|---|---|---|---|
| `ActivePerformance` | `performance` | `performance` | performance | accepted |
| `DrainingPerformance` | `eco` (desired) | `draining` | performance, until the performance pods finish | rejected for new ones |
| `ActiveEco` | `eco` | `eco` | eco | rejected |

The scheduler extender filters `draining` and `eco` nodes out for performance pods and applies no score penalty to draining nodes. The guard never evicts.

## What to read next

1. [Joulie Agent]({{< relref "/docs/architecture/agent.md" >}})
2. [Policy Algorithms]({{< relref "/docs/architecture/policies.md" >}})
3. [Energy-Aware Scheduling]({{< relref "/docs/architecture/energy-aware-scheduling.md" >}})
4. [Input Telemetry and Actuation Interfaces]({{< relref "/docs/architecture/telemetry.md" >}})
5. [Configuration Reference]({{< relref "/docs/getting-started/05-configuration-reference.md" >}})

The full reconcile flow, the family split algorithm and the reasoning behind the model: [Policy algorithms](https://github.com/joulie-k8s/Joulie/wiki/Policy-Algorithms) on the wiki.
