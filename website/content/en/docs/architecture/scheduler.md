---
title: "Scheduler Extender"
weight: 40
---

Joulie ships a scheduler extender that steers workloads away from eco nodes and thermally or electrically stressed nodes.

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

The extender applies one hard rule: **performance pods are rejected from eco nodes**.

### Eco node rejection for performance pods

A pod is treated as performance if:

- it carries `joulie.io/workload-class: performance`, or
- a matching `WorkloadProfile` has `criticality: performance`.

For such pods, the extender rejects any node whose `NodeTwin.status` has `schedulableClass: eco`.

All other pods (`standard`, `best-effort`, and unannotated pods) pass the filter unconditionally.

## Score logic

After filtering, the extender scores remaining nodes.

### Base formula

```
score = headroom * 0.4 + (100 - coolingStress) * 0.3 + (100 - psuStress) * 0.3
```

Where:

- `headroom`: available CPU/GPU compute headroom on the node (0-100), from `NodeTwin.status.PredictedPowerHeadroomScore`.
- `coolingStress`: predicted % of cooling capacity in use (0-100), from `NodeTwin.status.PredictedCoolingStressScore`.
- `psuStress`: predicted % of PDU/rack PSU capacity in use (0-100), from `NodeTwin.status.PredictedPsuStressScore`.

Higher scores are better.
A node with high headroom and low facility stress receives the highest score.
If a node has no `NodeTwin.status`, a neutral score of 50 is returned.

### Workload class adjustments

| Condition | Adjustment |
|-----------|-----------|
| `schedulableClass == "draining"` | -20 (avoid adding load to transitioning nodes) |
| `workload-class == "best-effort"` AND `schedulableClass == "eco"` | +5 (concentrate batch work on eco nodes) |

### Cap sensitivity scoring

For workloads that annotate sensitivity:

```
joulie.io/cpu-sensitivity: high  → score = score*0.7 + cpuCapHeadroom*0.3
joulie.io/gpu-sensitivity: high  → score = score*0.7 + gpuCapHeadroom*0.3
```

This blends the base score with remaining cap headroom for sensitive workloads, penalizing nodes that are near their cap ceiling.

All scores are clamped to `[0, 100]`.

## Data sources: NodeTwin CR

The extender reads `NodeTwin` CRs (one per managed node).
Values are cached with a 30-second TTL to avoid hitting the API server on every scheduling decision.

`NodeTwin.status` is populated by the operator's twin controller, which runs the digital twin model using telemetry from Prometheus and `NodeHardware`.

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
