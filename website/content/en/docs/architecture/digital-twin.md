---
title: "Digital Twin"
weight: 35
---

The digital twin is Joulie's core predictive engine. It is a lightweight O(1) parametric model that predicts the impact of scheduling and power-cap decisions on node thermal and power state, without running a full simulation for each scheduling decision.

## What the digital twin computes

For each managed node, the twin produces three scores stored in `NodeTwin.status`:

| Signal | Range | Meaning |
|--------|-------|---------|
| **Power headroom** | 0-100 | Remaining power budget before hitting thermal or PSU limits. Higher is better for new workload placement. |
| **CoolingStress** | 0-100 | Predicted percentage of cooling capacity in use. High values indicate the node is near its thermal limit. |
| **PSUStress** | 0-100 | Predicted percentage of PDU/rack power capacity in use. High values indicate the rack is near its power supply limit. |

The twin also computes:

- **SchedulableClass**: `performance`, `eco`, or `draining` (transition state). The scheduler extender uses this to filter and score nodes.
- **HardwareDensityScore**: normalized compute density proxy used for heterogeneous planning.
- **PowerMeasurement**: a block of measured and derived power values consumed directly by the scheduler for projected headroom scoring.

### PowerMeasurement output

The `powerMeasurement` block in `NodeTwin.status` provides the scheduler with the real-time power data it needs for projected headroom scoring:

| Field | Unit | Description |
|-------|------|-------------|
| `source` | string | Measurement source: `kepler` (direct), `utilization` (model-based), or `static` (estimate from caps) |
| `measuredNodePowerW` | watts | Current total node power draw |
| `cpuCappedPowerW` | watts | CPU power budget (cap percentage × max CPU watts) |
| `gpuCappedPowerW` | watts | GPU power budget (cap percentage × max GPU watts) |
| `nodeCappedPowerW` | watts | Total node power budget (CPU + GPU capped power) |
| `cpuTdpW` | watts | CPU thermal design power (max possible) |
| `gpuTdpW` | watts | GPU thermal design power (max possible) |
| `nodeTdpW` | watts | Total node TDP (CPU + GPU) |
| `powerTrendWPerMin` | watts/min | Rolling derivative of node power draw. Positive = rising, negative = falling. |

The scheduler uses `measuredNodePowerW` + pod marginal power to compute projected headroom relative to `nodeCappedPowerW`. The `powerTrendWPerMin` feeds the ±10 point trend bonus. See [Scheduler Extender]({{< relref "/docs/architecture/scheduler.md" >}}) for details.

## CoolingStress formula

CoolingStress is a **per-node** metric. It answers: "how close is this node to its cooling limit?"

### Step 1: estimate node power draw

The twin does not read live telemetry. It estimates node power from hardware discovery (`NodeHardware`) and the current cap percentages (`NodeTwin.spec`):

```
nodePower = (cpuMaxWattsPerSocket * sockets * cpuCapPct/100)
          + (gpuMaxWatts * gpuCount * gpuCapPct/100)
```

- `cpuMaxWattsPerSocket` and `gpuMaxWatts` come from `NodeHardware.status.cpu.capRange` and `NodeHardware.status.gpu.capRange`.
- `cpuCapPct` and `gpuCapPct` come from the resolved `NodeTwin.spec` intent (defaulting to 100 if unset).

This means the twin predicts power based on what the node *could* draw at its current cap setting, not what it is actually drawing right now. This is intentional: the twin is a planning model, not a monitoring dashboard.

### Step 2: compute cooling stress

The default `LinearCoolingModel` applies:

```
coolingStress = (nodePower / referenceNodePower) * 80 + max(0, temp - 20) * 0.5
```

| Term | Default | Rationale |
|------|---------|-----------|
| `referenceNodePower` | 4000 W | A fully loaded 2-socket EPYC 9654 + 8x H100 NVL reference node. A node drawing the reference power scores 80 at 20C, leaving 20 points of headroom for temperature. |
| `* 80` | | Scales power into 0-80 range so that temperature can push the score above 80 toward 100. A node at 100% of reference power is stressed but not yet at capacity. |
| `max(0, temp - 20) * 0.5` | baseline 20C | Each degree above 20C adds 0.5 points. At 40C ambient, temperature alone contributes 10 points. This models the reduced effectiveness of air-side cooling in warmer climates or seasons. |

The result is clamped to [0, 100].

**Example**: a 2-socket EPYC node with 4x H100 at eco (60% cap), 25C ambient:
- CPU: 400 W/socket * 2 * 0.6 = 480 W
- GPU: 400 W * 4 * 0.6 = 960 W
- nodePower = 1440 W
- coolingStress = (1440 / 4000) * 80 + (25 - 20) * 0.5 = 28.8 + 2.5 = 31.3

### Why this model

The `LinearCoolingModel` is an algebraic proxy. It avoids CFD or thermal RC simulation and runs in O(1) per node. It is deliberately conservative (overestimates stress relative to real cooling capacity) because its main job is to provide a ranking signal for the scheduler, not an exact thermal prediction. Exact thermal models can be plugged in via the `CoolingModel` interface.

## PSUStress formula

PSUStress is a **cluster-level** metric. It answers: "how close is this rack to its power supply limit?"

```
psuStress = clusterTotalPower / referenceRackCapacity * 100
```

| Term | Default | Rationale |
|------|---------|-----------|
| `clusterTotalPower` | (sum of all node power) | Total cluster power draw in watts, passed in by the operator from aggregated telemetry. |
| `referenceRackCapacity` | 50,000 W (50 kW) | A typical single-rack PDU capacity. This is a placeholder; in production, actual PDU readings would replace it. |

The result is clamped to [0, 100].

Because this is a cluster-level signal, all nodes on the same rack see the same PSU stress score. This is intentional: a rack PDU brownout affects every node in the rack, not just the one drawing the most power.

**Example**: 8 nodes drawing a total of 30 kW:
- psuStress = 30000 / 50000 * 100 = 60

## Power headroom

Power headroom combines cap state and cooling stress into a single "room for more work" score:

```
capFactor = (cpuCapPct + gpuCapPct) / 200       // 0 = fully capped, 1 = uncapped
coolingFactor = 1 - coolingStress / 100          // 0 = cooling at capacity, 1 = cool
headroom = capFactor * coolingFactor * 100
```

The scheduler uses headroom as the primary scoring signal (70% weight). A node with high caps and low cooling stress gets the highest headroom and attracts new workloads.

## CoolingModel interface

The `CoolingModel` interface is pluggable:

```go
type CoolingModel interface {
    CoolingStress(nodePowerW, ambientTempC float64) float64
}
```

The default implementation is `LinearCoolingModel`, an algebraic proxy suitable for initial deployments and simulation. A future implementation will use openModelica reduced-order thermal simulation via the same interface for higher-fidelity predictions.

## How it feeds the scheduler

The twin controller runs in the operator on each reconcile tick (~1 minute) and writes `NodeTwin.status` per managed node. The scheduler extender caches these `NodeTwin` CRs with a 30-second TTL and uses them in its filter and score logic:

```
twin controller (operator)
  → writes NodeTwin.status
    → scheduler extender cache (30s TTL)
      → filter: rejects eco nodes for performance pods
      → score: headroomScore*0.7 + (100-coolingStress)*0.15 + trendBonus + profileBonus + pressureRelief
```

This keeps scheduling decisions lightweight (one cache lookup per node per scheduling attempt) while reflecting the latest thermal and power state of the cluster.

## How it feeds the operator

The twin also drives operator decisions:

- **Transition guard**: when a node is transitioning from performance to eco, the twin sets `schedulableClass` to `draining` until all performance pods have completed or been drained.

## Implementation

The twin is implemented in `pkg/operator/twin/twin.go`. Key types:

- `Input`: all inputs needed to compute twin state for one node (hardware, profile, cap percentages, workloads, facility signals).
- `Output`: the computed `NodeTwin.status` fields.
- `CoolingModel`: pluggable interface for cooling stress computation.
- `LinearCoolingModel`: default algebraic proxy implementation.
- `Compute(Input) Output`: the main computation function.

## What to read next

1. [Scheduler Extender]({{< relref "/docs/architecture/scheduler.md" >}})
2. [Joulie Operator]({{< relref "/docs/architecture/operator.md" >}})
3. [CRD and Policy Model]({{< relref "/docs/architecture/policy.md" >}})
