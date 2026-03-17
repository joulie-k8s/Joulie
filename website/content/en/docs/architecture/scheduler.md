---
title: "Scheduler Extender"
weight: 40
---

Joulie ships a scheduler extender that steers workloads toward appropriate nodes based on power profile, thermal stress, and hardware capabilities.

## Why a scheduler component is needed

Kubernetes scheduling decisions are made before a pod is running.
That is the right place to enforce Joulie's power-profile intent, because:

- placing a performance workload on an eco (capped) node defeats energy savings and violates workload SLOs,
- placing any workload on a node under heavy thermal or PSU stress increases the risk of throttling and supply headroom violations,
- the `joulie.io/workload-class` annotation is the single source of truth for placement intent; the extender enforces it without requiring users to write complex scheduling rules.

## What Joulie implements: scheduler extender

Joulie implements an **HTTP-based scheduler extender**, not an in-tree plugin.

The extender registers with `kube-scheduler` through a `KubeSchedulerConfiguration` extender block.
The scheduler calls the extender's HTTP endpoints as part of the normal scheduling cycle:

- **filter** endpoint: rejects nodes that are incompatible with the pod,
- **prioritize** endpoint: ranks remaining nodes by suitability.

The extender does not replace the Kubernetes scheduler.
It runs as a lightweight HTTP service and participates in the existing scheduling cycle.

The scheduler extender is always deployed as part of Joulie. Without it, pods run anywhere and get standard Kubernetes scheduling.

## Filter logic

The extender applies one hard rule: **performance pods are rejected from eco and draining nodes**.

A pod is treated as performance if it carries `joulie.io/workload-class: performance`.

For such pods, the extender rejects any node whose `NodeTwin.status` has `schedulableClass` set to `"eco"` or `"draining"`. A label-based fallback also rejects nodes with `joulie.io/power-profile: eco` when no NodeTwin status is present.

Standard pods (the default, or `joulie.io/workload-class: standard`) pass the filter unconditionally. Unknown nodes (no NodeTwin state) are allowed for all pod classes.

## Score logic

After filtering, the extender scores remaining nodes.

### Base formula

```
score = headroom * 0.4 + (100 - coolingStress) * 0.3 + (100 - psuStress) * 0.3
```

Where:

- `headroom`: available power headroom on the node (0-100), from `NodeTwin.status.predictedPowerHeadroomScore`.
- `coolingStress`: predicted cooling stress (0-100), from `NodeTwin.status.predictedCoolingStressScore`.
- `psuStress`: predicted PSU stress (0-100), from `NodeTwin.status.predictedPsuStressScore`.

Higher scores are better.
A node with high headroom and low facility stress receives the highest score.

### Stale twin fallback

If the NodeTwin's `lastUpdated` timestamp is older than 5 minutes (configurable via `TWIN_STALENESS_THRESHOLD`), the node receives a neutral score of 50. This prevents stale data from an operator that may have stopped updating from influencing placement. Nodes with no `lastUpdated` timestamp at all are also treated as stale.

### Adaptive performance pressure relief

For standard pods on performance nodes, a pressure penalty is applied:

```
if workloadClass == "standard" AND schedulableClass == "performance":
    score -= perfPressure * 0.3
```

Where `perfPressure` is computed once per scoring batch as the average congestion across all performance nodes:

```
perfPressure = average(100 - headroom) across all non-stale performance nodes
```

At full saturation (`perfPressure = 100`), this subtracts up to 30 points from the score on performance nodes. The effect steers standard pods toward eco nodes when performance nodes are congested, preserving performance capacity for performance-class workloads.

When performance nodes are idle (`perfPressure = 0`), there is no penalty and standard pods spread normally.

### CPU-only pod GPU penalty

CPU-only pods (those not requesting `nvidia.com/gpu`, `amd.com/gpu`, or `gpu.intel.com/i915`) receive a -30 score penalty on GPU nodes. GPU presence is detected from cached NodeHardware CRs. This discourages CPU-only workloads from occupying GPU nodes where they waste GPU idle power.

Pods that request GPU resources do not receive this penalty.

### PUE-weighted marginal power estimation

When facility metrics are enabled (`ENABLE_FACILITY_METRICS=true`), the operator computes PUE from real data-center metrics and writes `NodeTwin.status.estimatedPUE`. The scheduler extender uses this to weight marginal power estimates:

```
if estimatedPUE > 1.0:
    deltaCPUWatts  *= estimatedPUE
    deltaGPUWatts  *= estimatedPUE
    deltaTotalWatts *= estimatedPUE
```

This means a pod placed on a node with PUE 1.6 is treated as costing 60% more energy than one with PUE 1.0. The effect is that the scheduler prefers nodes in more efficiently cooled parts of the facility, reducing total energy consumption including cooling overhead.

Without facility metrics, PUE defaults to 1.0 and the multiplier has no effect.

### Eviction history awareness

When the active rescheduler evicts a pod, it annotates the pod's owner (ReplicaSet or StatefulSet) with eviction context:

- `joulie.io/last-eviction-from-class`: the schedulableClass of the node the pod was evicted from (e.g., `eco`)
- `joulie.io/last-eviction-reason`: the eviction reason (e.g., `cooling_stress`)
- `joulie.io/last-eviction-time`: RFC3339 timestamp

The scheduler reads these annotations when placing the replacement pod:

- **Filter**: if a pod's owner was evicted from an eco node, eco and draining nodes are rejected (same as performance pod filtering). This prevents a standard pod from being re-placed on an eco node where it was previously throttled.
- **Score**: nodes matching the evicted-from class receive a -25 score penalty.

Eviction context expires after `EVICTION_HISTORY_TTL` (default 30 minutes). After expiry, the scheduler schedules normally again.

### Score clamping

All scores are clamped to `[0, 100]`. Nodes with no NodeTwin state receive a neutral score of 50.

## Data sources

The extender reads two types of Joulie CRs, both cached with a 30-second TTL to avoid hitting the API server on every scheduling decision:

- **NodeTwin CRs** - provide `schedulableClass`, `predictedPowerHeadroomScore`, `predictedCoolingStressScore`, `predictedPsuStressScore`, and `lastUpdated` for filter and score decisions.
- **NodeHardware CRs** - provide GPU presence information for the CPU-only GPU penalty.

`NodeTwin.status` is populated by the operator's twin controller, which runs the digital twin model using telemetry from Prometheus and `NodeHardware`.

## Summary

| Condition | Effect |
|-----------|--------|
| Performance pod + eco/draining node | Hard reject (filter) |
| Standard pod + any node | Allowed (no filter) |
| Unknown node (no NodeTwin) + any pod | Allowed, neutral score (50) |
| High headroom, low stress | High score |
| Standard pod + performance node under pressure | Score penalty (up to -30) |
| CPU-only pod + GPU node | Score penalty (-30) |
| Stale or missing NodeTwin | Neutral score (50) |
| Pod owner evicted from eco class | Hard reject eco/draining (filter) + score penalty (-25) |

## What the extender does not do

- It does not execute a full digital twin simulation per scheduling decision.
- It does not perform live pod migration or eviction.
- It does not override Kubernetes resource fits; it only participates in the extender filter/prioritize hooks.
- It does not make admission decisions for already-running pods.

Decisions are lightweight: one cache lookup per node per scheduling attempt.

## How to deploy

The scheduler extender is deployed as part of the Joulie Helm chart.

### KubeSchedulerConfiguration extender block

```yaml
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
profiles:
- schedulerName: default-scheduler
extenders:
- urlPrefix: "http://joulie-scheduler-extender.joulie-system.svc.cluster.local:9876"
  filterVerb: "filter"
  prioritizeVerb: "prioritize"
  weight: 1
  enableHTTPS: false
  nodeCacheCapable: false
  ignorable: true
```

Setting `ignorable: true` means the scheduler proceeds normally if the extender is temporarily unreachable.

### Testing

The extender exposes a `/healthz` endpoint.

To verify filter decisions without a running scheduler:

```bash
curl -s -X POST \
  http://localhost:9876/filter \
  -H 'Content-Type: application/json' \
  -d '{"pod": {...}, "nodes": {"items": [...]}}'
```

## What to read next

1. [Digital Twin]({{< relref "/docs/architecture/digital-twin.md" >}})
2. [WorkloadProfile Guide]({{< relref "/docs/getting-started/04-workload-profiles.md" >}})
4. [Pod Compatibility]({{< relref "/docs/getting-started/03-pod-compatibility.md" >}})
5. [Joulie Operator]({{< relref "/docs/architecture/operator.md" >}})
