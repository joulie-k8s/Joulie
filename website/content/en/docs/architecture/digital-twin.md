---
title: "Digital Twin"
weight: 35
---

The digital twin is Joulie's core predictive engine. It is a lightweight O(1) parametric model that predicts the impact of scheduling and power-cap decisions on node thermal and power state, without running a full simulation for each scheduling decision.

## What the digital twin computes

For each managed node, the twin produces three scores stored in `NodeTwinState`:

| Signal | Range | Meaning |
|--------|-------|---------|
| **Power headroom** | 0--100 | Remaining power budget before hitting thermal or PSU limits. Higher is better for new workload placement. |
| **CoolingStress** | 0--100 | Predicted percentage of cooling capacity in use. High values indicate the node is near its thermal limit. |
| **PSUStress** | 0--100 | Predicted percentage of PDU/rack power capacity in use. High values indicate the rack is near its power supply limit. |

The twin also computes:

- **SchedulableClass**: `performance`, `eco`, or `draining` (transition state). The scheduler extender uses this to filter and score nodes.
- **HardwareDensityScore**: normalized compute density proxy used for heterogeneous planning.
- **RescheduleRecommendations**: list of workloads recommended for migration when stress thresholds are exceeded.

## CoolingStress formula

The default `LinearCoolingModel` computes cooling stress as:

```
coolingStress = (nodePower / referenceNodePower) * 80 + max(0, temp - 20) * 0.5
```

Where:

- `nodePower` is the estimated total power draw of the node (CPU + GPU), derived from hardware cap ranges and current cap percentages.
- `referenceNodePower` defaults to 4000 W (a 2-socket EPYC + 8x H100 reference node).
- `temp` is the outside air temperature in Celsius.

The result is clamped to [0, 100].

## PSUStress formula

PSU stress is computed as:

```
psuStress = clusterPower / referenceRackCapacity * 100
```

Where:

- `clusterPower` is the total cluster power draw in watts.
- `referenceRackCapacity` defaults to 50 kW.

The result is clamped to [0, 100].

## CoolingModel interface

The `CoolingModel` interface is pluggable:

```go
type CoolingModel interface {
    CoolingStress(nodePowerW, ambientTempC float64) float64
}
```

The default implementation is `LinearCoolingModel`, an algebraic proxy suitable for initial deployments and simulation. A future implementation will use openModelica reduced-order thermal simulation via the same interface for higher-fidelity predictions.

## How it feeds the scheduler

The twin controller runs in the operator on each reconcile tick (~1 minute) and writes one `NodeTwinState` CR per managed node. The scheduler extender caches these CRs with a 30-second TTL and uses them in its filter and score logic:

```
twin controller (operator)
  → writes NodeTwinState CRs
    → scheduler extender cache (30s TTL)
      → filter: rejects eco nodes for performance pods
      → score: headroom*0.4 + (100-coolingStress)*0.3 + (100-psuStress)*0.3
```

This keeps scheduling decisions lightweight (one cache lookup per node per scheduling attempt) while reflecting the latest thermal and power state of the cluster.

## How it feeds the operator

The twin also drives operator decisions:

- **Migration triggers**: when CoolingStress or PSUStress exceeds 70 on a node, the twin generates reschedule recommendations for reschedulable best-effort workloads. The operator can then trigger migration away from stressed nodes.
- **Transition guard**: when a node is transitioning from performance to eco, the twin sets `schedulableClass` to `draining` until all performance pods have completed or been rescheduled.

## Implementation

The twin is implemented in `pkg/operator/twin/twin.go`. Key types:

- `Input`: all inputs needed to compute twin state for one node (hardware, profile, cap percentages, workloads, facility signals).
- `Output`: the computed `NodeTwinState` fields.
- `CoolingModel`: pluggable interface for cooling stress computation.
- `LinearCoolingModel`: default algebraic proxy implementation.
- `Compute(Input) Output`: the main computation function.

## What to read next

1. [Scheduler Extender]({{< relref "/docs/architecture/scheduler.md" >}})
2. [Joulie Operator]({{< relref "/docs/architecture/operator.md" >}})
3. [CRD and Policy Model]({{< relref "/docs/architecture/policy.md" >}})
