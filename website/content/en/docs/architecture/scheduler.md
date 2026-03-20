---
title: "Scheduler Extender"
weight: 40
---

Joulie ships a scheduler extender that steers workloads toward appropriate nodes based on power profile, thermal stress, and hardware capabilities.

## How a pod gets scheduled (end-to-end)

When a new pod is created in the cluster, the following sequence occurs:

```
1. Pod created (e.g., kubectl apply, Job controller, Deployment rollout)
       |
2. kube-scheduler picks up the unscheduled pod
       |
3. kube-scheduler runs its default filters (resource fits, taints, affinity)
       |
4. kube-scheduler calls Joulie's /filter endpoint
       |  - Sends: pod spec + candidate node list
       |  - Joulie reads pod annotation joulie.io/workload-class
       |  - Performance pods: reject nodes with schedulableClass = eco or draining
       |  - Standard pods: pass all nodes
       |  - Returns: filtered node list + rejection reasons
       |
5. kube-scheduler calls Joulie's /prioritize endpoint
       |  - Sends: pod spec + surviving node list
       |  - Joulie reads NodeTwin CRs (cached, 30s TTL) for power state
       |  - Joulie reads NodeHardware CRs (cached, 30s TTL) for hardware specs
       |  - Joulie extracts pod CPU/GPU requests for marginal power estimation
       |  - Joulie scores each node 0-100 using the scoring formula
       |  - Returns: list of (node, score) pairs
       |
6. kube-scheduler combines Joulie scores with its own plugin scores
       |
7. Pod is bound to the highest-scoring node
```

The extender participates in steps 4 and 5 only. It does not replace the Kubernetes scheduler — it extends it with energy-aware filter and scoring logic.

### What the extender reads

| Source | CRD | What it provides | Cache TTL |
|--------|-----|------------------|-----------|
| Digital twin output | `NodeTwin.status` | `schedulableClass`, headroom, cooling stress, power measurement (measured/capped/TDP/trend) | 30s |
| Hardware discovery | `NodeHardware.status` | CPU cores, CPU max watts, GPU count, GPU max watts, memory | 30s |
| Pod spec | (from kube-scheduler request) | `joulie.io/workload-class` annotation, container CPU/GPU resource requests | per-request |

### What the extender does NOT do

- It does not execute a full digital twin simulation per scheduling decision.
- It does not perform live pod migration or eviction.
- It does not override Kubernetes resource fits; it only participates in the extender filter/prioritize hooks.
- It does not make admission decisions for already-running pods.

Decisions are lightweight: one cache lookup per node per scheduling attempt.

## Why a scheduler component is needed

Kubernetes scheduling decisions are made before a pod is running.
That is the right place to enforce Joulie's power-profile intent, because:

- placing a performance workload on an eco (capped) node defeats energy savings and violates workload SLOs,
- placing any workload on a node under heavy thermal stress increases the risk of throttling and headroom violations,
- the `joulie.io/workload-class` annotation is the single source of truth for placement intent; the extender enforces it without requiring users to write complex scheduling rules.

## What Joulie implements: scheduler extender

Joulie implements an **HTTP-based scheduler extender**, not an in-tree plugin.

The extender registers with `kube-scheduler` through a `KubeSchedulerConfiguration` extender block.
The scheduler calls the extender's HTTP endpoints as part of the normal scheduling cycle:

- **filter** (`POST /filter`): rejects nodes that are incompatible with the pod,
- **prioritize** (`POST /prioritize`): ranks remaining nodes by suitability.

The extender does not replace the Kubernetes scheduler.
It runs as a lightweight HTTP service and participates in the existing scheduling cycle.

The scheduler extender is always deployed as part of Joulie. Without it, pods run anywhere and get standard Kubernetes scheduling.

## Filter logic (step 4)

The extender applies one hard rule: **performance pods are rejected from eco and draining nodes**.

A pod is treated as performance if it carries `joulie.io/workload-class: performance`.

For such pods, the extender rejects any node whose `NodeTwin.status` has `schedulableClass` set to `"eco"` or `"draining"`. A label-based fallback also rejects nodes with `joulie.io/power-profile: eco` when no NodeTwin status is present.

Standard pods (the default, or `joulie.io/workload-class: standard`) pass the filter unconditionally. Unknown nodes (no NodeTwin state) are allowed for all pod classes.

## Score logic (step 5)

After filtering, the extender scores remaining nodes. Each node receives a score from 0 to 100. Higher scores are better. kube-scheduler uses these scores (combined with its own plugin scores) to pick the winning node.

### Scoring inputs per node

For each candidate node, the extender gathers:

1. **Power measurement** from `NodeTwin.status.powerMeasurement`:
   - `measuredNodePowerW` — actual node power draw (from Kepler, utilization model, or static estimate)
   - `nodeCappedPowerW` — the node's current power budget (sum of CPU + GPU caps)
   - `nodeTdpW` — thermal design power (maximum possible draw)
   - `powerTrendWPerMin` — rolling derivative of power draw (watts/minute, positive = rising)

2. **Twin-computed scores** from `NodeTwin.status`:
   - `predictedPowerHeadroomScore` — fallback headroom if no power measurement is available
   - `predictedCoolingStressScore` — thermal stress (0 = cool, 100 = at cooling limit)
   - `schedulableClass` — `performance`, `eco`, or `draining`

3. **Pod resource demand** extracted from the pod's container specs:
   - CPU cores requested (summed across all containers)
   - GPU count requested (from `nvidia.com/gpu` or `amd.com/gpu` limits)

4. **Node hardware** from `NodeHardware.status`:
   - CPU model, total cores, max watts per socket
   - GPU model, count, max watts per GPU

### Marginal power estimation

Before scoring, the extender estimates how many additional watts this specific pod will add to the node. This makes the score **pod-specific** — the same node scores differently for a lightweight 2-core pod vs. an 8-GPU training pod.

The estimation uses `powerest.EstimateMarginalImpact`:

```
podMarginalW = CPUUtilCoeff * (podCPUCores / nodeTotalCores) * nodeMaxCPUWatts
             + GPUUtilCoeff * (podGPUCount / nodeGPUCount)   * nodeMaxGPUWatts
```

Coefficients are tunable via environment variables (`MARGINAL_CPU_UTIL_COEFF`, `MARGINAL_GPU_UTIL_COEFF_STANDARD`, `MARGINAL_GPU_UTIL_COEFF_PERFORMANCE`). GPU coefficients differ by workload class because performance workloads typically drive GPUs harder.

### Scoring formula

```
projectedPower = measuredPower + podMarginalPower
headroomScore  = (cappedPower - projectedPower) / cappedPower * 100
coolingStress  = from NodeTwin.status.predictedCoolingStressScore (0-100)
clusterTrend   = sum of all per-node powerTrend (W/min)
trendScale     = 2.0 if |clusterTrend| > 500 else 6.0
trendBonus     = -clamp(powerTrend / trendScale, -25, 25)

score = headroomScore * 0.7
      + (100 - coolingStress) * 0.15
      + trendBonus
      + profileBonus
      + pressureRelief
```

Each component explained:

| Component | Weight | Range | What it does |
|-----------|--------|-------|--------------|
| `headroomScore * 0.7` | 70% | can go negative to +70 | Favors nodes with room in their power budget after this pod is placed. Can go negative if the pod would exceed the budget — the node is penalized, not just zeroed. |
| `(100 - coolingStress) * 0.15` | 15% | 0 to +15 | Penalizes thermally stressed nodes. At coolingStress=0 (cool), contributes 15 points. At coolingStress=100, contributes 0. |
| `trendBonus` | ±25 pts | -25 to +25 | Rewards nodes whose power draw is falling (trend < 0), penalizes nodes whose power is rising (trend > 0). Uses adaptive scaling: trendScale=6.0 at steady state (amplifies signal), trendScale=2.0 during cluster-wide bursts (\|clusterTrend\| > 500 W/min). |
| `profileBonus` | +10 pts | 0 or +10 | Standard pods on eco nodes get +10, steering non-critical work toward energy-saving nodes. |
| `pressureRelief` | up to -30 pts | -30 to 0 | Standard pods on performance nodes are penalized when performance nodes are congested (see below). |

**Example**: A node with 600W capped power, 300W measured, pod adds 50W, cooling stress 20, power trend flat:

- headroomScore = (600 - 350) / 600 * 100 = 41.7
- score = 41.7 *0.7 + (100 - 20)* 0.15 + 0 = 29.2 + 12.0 = **41.2**

**Example**: An ideal node (empty, fully uncapped, cool, standard pod on eco):

- headroomScore = 100, coolingStress = 0, trendBonus = 0, profileBonus = 10
- score = 100 *0.7 + 100* 0.15 + 0 + 10 = 70 + 15 + 10 = **95**

### Projected headroom (the key innovation)

Unlike simple utilization-based scheduling, Joulie computes headroom relative to the node's **capped power budget** (not TDP or max capacity), and it subtracts the **pod's estimated marginal power** before scoring:

```
projectedPower = measuredPower + podMarginalPower
headroomScore  = (cappedPower - projectedPower) / cappedPower * 100
```

This means:

- **Eco nodes** (capped at, say, 60%) have a lower `cappedPower`, so they run out of headroom sooner and are naturally deprioritized for heavy workloads.
- **GPU-heavy pods** consume more marginal watts, so nodes near their cap are penalized more for GPU pods than CPU-only pods.
- **headroomScore can go negative** — if the pod would push the node over its cap, the score goes below zero. After the formula is applied and clamped to [0, 100], such nodes get score 0 and are effectively last-choice.

If power measurement data is unavailable (no `powerMeasurement` in NodeTwin status), the extender falls back to the twin-computed `predictedPowerHeadroomScore`.

### Stale twin fallback

If the NodeTwin's `lastUpdated` timestamp is older than 5 minutes (configurable via `TWIN_STALENESS_THRESHOLD`), the node receives a neutral score of 50. This prevents stale data from an operator that may have stopped updating from influencing placement. Nodes with no `lastUpdated` timestamp at all are also treated as stale. Nodes with no NodeTwin state at all also receive 50.

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

### Score clamping

All scores are clamped to `[0, 100]` before being returned to kube-scheduler.

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

## Summary

| Condition | Effect |
|-----------|--------|
| Performance pod + eco/draining node | Hard reject (filter) |
| Standard pod + any node | Allowed (no filter) |
| Unknown node (no NodeTwin) + any pod | Allowed, neutral score (50) |
| High headroom, low cooling stress, falling power trend | High score |
| Low headroom (pod would exceed cap) | Low score (headroomScore goes negative) |
| Standard pod + eco node | +10 bonus (profile steering) |
| Standard pod + performance node under pressure | Score penalty up to -30 via pressureRelief |
| Stale or missing NodeTwin | Neutral score (50) |
| Heavy pod (high CPU/GPU) on nearly-full node | Low score (marginal power eats headroom) |
| Light pod on same node | Higher score (less marginal power) |

## Environment variables

| Variable | Default | Purpose |
|----------|---------|---------|
| `EXTENDER_ADDR` | `:9876` | HTTP listen address |
| `CACHE_TTL` | `30s` | TTL for NodeTwin and NodeHardware caches |
| `TWIN_STALENESS_THRESHOLD` | `5m` | Max age of NodeTwin data before treating as stale |
| `MARGINAL_CPU_UTIL_COEFF` | (from `powerest.DefaultCoefficients`) | CPU marginal power coefficient |
| `MARGINAL_GPU_UTIL_COEFF_STANDARD` | (from `powerest.DefaultCoefficients`) | GPU marginal power coefficient for standard pods |
| `MARGINAL_GPU_UTIL_COEFF_PERFORMANCE` | (from `powerest.DefaultCoefficients`) | GPU marginal power coefficient for performance pods |
| `ENABLE_FACILITY_METRICS` | `false` | Enable PUE-weighted marginal scoring |

## Debug endpoint

The extender exposes `GET /debug/scoring` which returns the current scoring state for all nodes as JSON:

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

This endpoint is useful for debugging scoring decisions without needing to trigger an actual scheduling event.

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
2. [Pod Compatibility]({{< relref "/docs/getting-started/03-pod-compatibility.md" >}})
3. [Joulie Operator]({{< relref "/docs/architecture/operator.md" >}})
