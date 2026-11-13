---
title: "Scheduler Extender"
weight: 40
---

Joulie's scheduler extender keeps performance pods off eco nodes and ranks the remaining nodes by power headroom and cooling stress. It is an HTTP extender that kube-scheduler calls; it does not replace kube-scheduler. The chart always deploys it; without it pods get standard Kubernetes scheduling.

## How a pod gets scheduled (end-to-end)

1. kube-scheduler picks up the new pod and runs its own filters (resources, taints, affinity).
2. It calls Joulie's `POST /filter`: performance pods lose the eco and draining nodes.
3. It calls `POST /prioritize`: Joulie scores each remaining node 0 to 100 for this pod and returns it as a 0 to 10 priority.
4. kube-scheduler adds Joulie's priority, times the extender `weight`, to its own plugin scores and binds the pod to the best node.

Placement intent comes from one pod annotation, `joulie.io/workload-class`, so users write no scheduling rules. The extender does not migrate or evict pods and never overrides resource fits.

### What the extender reads

| Source | What it provides | Cache |
|--------|------------------|-------|
| `NodeTwin.status` | `schedulableClass`, cooling stress, headroom, `powerMeasurement` (measured, capped, TDP, trend) | `CACHE_TTL` (30 s) |
| `NodeHardware.status` | CPU cores and max watts, GPU count and max watts | `CACHE_TTL` |
| the pod | `joulie.io/workload-class`, CPU and GPU requests | per request |

## Filter logic

| Pod | Node | Result |
|---|---|---|
| `joulie.io/workload-class: performance` | `schedulableClass` `eco` or `draining` (or label `joulie.io/power-profile: eco` without a twin) | rejected |
| `performance` | any other node, or no `NodeTwin` | allowed |
| standard (no annotation, or `standard`) | any node | allowed |

## Score logic

The score is pod-specific: the extender estimates the watts this pod would add to each node from its CPU and GPU requests (GPU shares for MIG devices and partitions, see [Shared and partitioned GPUs]({{< relref "/docs/hardware/gpus.md#shared-and-partitioned-gpus" >}})), adds them to the node's measured power, and compares the result with the node's capped budget. Nodes with room left after the pod score high; heavy pods on nearly full nodes and eco nodes, whose budget is lower, score low.

```
score = headroomScore*0.7 + (100-coolingStress)*0.15 + trendBonus + profileBonus + pressureRelief
```

| Term | Range | From |
|---|---|---|
| projected headroom x 0.7 | negative to 70 | `powerMeasurement.measuredNodePowerW` plus the pod's watts, against `nodeCappedPowerW` ([Digital Twin]({{< relref "/docs/architecture/digital-twin.md#where-the-power-reading-comes-from" >}})) |
| (100 - cooling stress) x 0.15 | 0 to 15 | `predictedCoolingStressScore` |
| trend bonus | -25 to 25 | `powerTrendWPerMin`: falling power scores higher |
| profile bonus | 0 or 10 | +10 for a standard pod on an eco node |
| pressure relief | -30 to 0 | a standard pod on a performance node, when performance nodes are congested |

The score is clamped to 0 to 100. A node with stale (older than `TWIN_STALENESS_THRESHOLD`) or missing twin data gets a neutral 50. PSU stress and estimated PUE are not part of the score.

## Environment variables

| Variable | Default | Purpose |
|----------|---------|---------|
| `EXTENDER_ADDR` | `:9876` | HTTP listen address |
| `CACHE_TTL` | `30s` | Minimum interval between rebuilding the scoring maps from the informer cache |
| `TWIN_STALENESS_THRESHOLD` | `5m` | Max age of NodeTwin data before treating as stale |
| `MARGINAL_CPU_UTIL_COEFF` | (from `powerest.DefaultCoefficients`) | CPU marginal power coefficient |
| `MARGINAL_GPU_UTIL_COEFF_STANDARD` | (from `powerest.DefaultCoefficients`) | GPU marginal power coefficient for standard pods |
| `MARGINAL_GPU_UTIL_COEFF_PERFORMANCE` | (from `powerest.DefaultCoefficients`) | GPU marginal power coefficient for performance pods |

## Debug endpoint

`GET /debug/scoring` returns the current scoring state of every node as JSON, without a scheduling event:

```json
{
  "coefficients": {"cpuUtilCoeff": 0.8, "gpuUtilCoeffStandard": 0.6, "gpuUtilCoeffPerformance": 0.9},
  "nodes": [
    {
      "nodeName": "node-0",
      "schedulableClass": "performance",
      "headroom": 72.5,
      "coolingStress": 31.3,
      "measuredPowerW": 450.0,
      "cappedPowerW": 1200.0,
      "nodeTdpW": 2000.0,
      "powerTrendWPerMin": -5.2,
      "baseScore": 60.0,
      "cpuTotalCores": 128,
      "cpuMaxWattsTotal": 700.0,
      "gpuCount": 8,
      "gpuMaxWattsPerGpu": 400.0,
      "hasGpu": true,
      "stale": false
    }
  ]
}
```

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

`ignorable: true` lets the scheduler proceed normally if the extender is temporarily unreachable.

### Testing

The extender exposes `/healthz`. To check filter decisions without a running scheduler:

```bash
curl -s -X POST \
  http://localhost:9876/filter \
  -H 'Content-Type: application/json' \
  -d '{"pod": {...}, "nodes": {"items": [...]}}'
```

## What to read next

1. [Digital Twin]({{< relref "/docs/architecture/digital-twin.md" >}})
2. [Pod Compatibility]({{< relref "/docs/getting-started/03-pod-compatibility.md" >}})
3. [Joulie Controller Manager]({{< relref "/docs/architecture/controller-manager.md" >}})

The full scoring formula, the marginal power model and its default coefficients, the fallbacks and worked examples: [Scheduler scoring](https://github.com/joulie-k8s/Joulie/wiki/Scheduler-Scoring) on the wiki.
