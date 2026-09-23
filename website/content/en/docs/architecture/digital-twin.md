---
title: "Digital Twin"
weight: 35
---

The digital twin is Joulie's core predictive engine. It is a lightweight O(1) parametric model that predicts the impact of scheduling and power-cap decisions on node thermal and power state, without running a full simulation for each scheduling decision.

## What the digital twin computes

For each managed node, the twin produces three scores stored in `NodeTwin.status`:

| Signal | Range | Meaning |
|--------|-------|---------|
| **Power headroom** | up to 100, can go negative | Fraction of the node's capped power budget still unused. Higher is better for new workload placement. Negative means the node already draws more than its budget. |
| **CoolingStress** | 0-100 | Measured node power as a fraction of node TDP, scaled by an ambient temperature multiplier. High values indicate the node is near its thermal limit. |
| **PSUStress** | 0-100 | Rack power draw (or cluster power draw when no rack is known) as a fraction of a reference rack capacity. Computed and published, but not read by the scheduler today. |

The twin also computes:

- **SchedulableClass**: `performance`, `eco`, or `draining` (transition state). The scheduler extender uses this to filter and score nodes.
- **HardwareDensityScore**: normalized compute density proxy used for heterogeneous planning.
- **EstimatedPUE**: derived from cooling stress, between 1.05 and 1.40. Published for observability; not read by the scheduler today.
- **PowerMeasurement**: a block of measured and derived power values consumed directly by the scheduler for projected headroom scoring.

Every power-derived score starts from one number: `measuredNodePowerW`. The twin is not a cap-based estimator. It reads what the node is drawing and compares it against budgets it derives from hardware.

### PowerMeasurement output

The `powerMeasurement` block in `NodeTwin.status` provides the scheduler with the real-time power data it needs for projected headroom scoring:

| Field | Unit | Description |
|-------|------|-------------|
| `source` | string | Where the power reading came from (see below) |
| `measuredNodePowerW` | watts | Current total node power draw |
| `cpuCappedPowerW` | watts | CPU power budget (CPU TDP x cap percentage) |
| `gpuCappedPowerW` | watts | GPU power budget (GPU TDP x cap percentage) |
| `nodeCappedPowerW` | watts | Total node power budget (CPU + GPU capped power) |
| `cpuTdpW` | watts | CPU thermal design power (max watts per socket x sockets) |
| `gpuTdpW` | watts | GPU thermal design power (max watts per GPU x GPU count, 0 when no GPU is present) |
| `nodeTdpW` | watts | Total node TDP (CPU + GPU) |
| `powerTrendWPerMin` | watts/min | Rolling derivative of node power draw over a 5 minute window. Positive = rising, negative = falling. |

The scheduler uses `measuredNodePowerW` + pod marginal power to compute projected headroom relative to `nodeCappedPowerW`. The `powerTrendWPerMin` feeds the trend bonus, capped at 25 points in either direction. See [Scheduler Extender]({{< relref "/docs/architecture/scheduler.md" >}}) for details.

## Where the power reading comes from

The controller manager resolves `measuredNodePowerW` before calling the twin. `NODE_POWER_SOURCE` selects the source:

| `NODE_POWER_SOURCE` | Behaviour | `source` written |
|---------------------|-----------|------------------|
| `prometheus` | Runs `NODE_POWER_PROMETHEUS_QUERY` against `NODE_POWER_PROMETHEUS_ADDRESS`, substituting `{node}` with the node name. The query must return watts, so a Kepler joules counter has to be wrapped in `rate()`. | `prometheus`, or `prometheus-no-data` when the query is unset, errors, or returns 0 |
| `http` | GETs `NODE_POWER_HTTP_ENDPOINT` (with `{node}` substituted) and reads `packagePowerWatts`, or `cpu.packagePowerWatts`. This is how the simulator feeds the twin. | `http`, `http-error`, or `http-no-endpoint` |
| `static` (default) | Returns 0 watts. | `static` |

Because the default is `static`, an unconfigured cluster reports 0 watts for every node: headroom reads 100 and cooling stress reads 0 everywhere. Configure a source before treating the scores as a ranking signal.

`kepler` and `utilization` are reserved source values; no component writes them yet. When a reading exceeds twice the node's TDP the controller manager logs a one-shot warning, because that is the signature of a cumulative joules counter being read as watts.

## Node TDP and the capped budget

Both power formulas need the node's hardware limits, derived from `NodeHardware.status`:

```
cpuTdp  = cpuMaxWattsPerSocket * sockets
gpuTdp  = gpuMaxWatts * gpuCount          (0 when no GPU is present)
nodeTdp = cpuTdp + gpuTdp

cpuCappedPower  = cpuTdp * cpuCapPct/100
gpuCappedPower  = gpuTdp * gpuCapPct/100
nodeCappedPower = cpuCappedPower + gpuCappedPower
```

`cpuCapPct` and `gpuCapPct` come from the resolved `NodeTwin.spec` intent, defaulting to 100 when unset or non-positive.

When the socket count is unknown but a per-socket maximum is known, the twin assumes a single socket. A zero socket count would multiply the per-socket maximum down to zero, and a zero budget reads as unlimited headroom further down, so a busy node would look like the emptiest one in the cluster. Assuming one socket underestimates the budget on a multi-socket machine, which is the safe direction: the node looks fuller than it is rather than infinitely free.

### Reference node used in the examples below

A node with two AMD EPYC 9654 sockets (360 W per socket) and four NVIDIA H100 NVL GPUs (400 W each), running eco at a 60% cap on both CPU and GPU:

- `cpuTdpW` = 360 * 2 = 720 W
- `gpuTdpW` = 400 * 4 = 1600 W
- `nodeTdpW` = 2320 W
- `cpuCappedPowerW` = 720 * 0.6 = 432 W
- `gpuCappedPowerW` = 1600 * 0.6 = 960 W
- `nodeCappedPowerW` = 1392 W

## CoolingStress formula

CoolingStress is a **per-node** metric. It answers: "how close is this node to its cooling limit?"

```
tempMultiplier = 1 / max(0.5, 1 - (ambientTempC - 20) * 0.02)
coolingStress  = clamp((measuredPower / nodeTdp) * tempMultiplier * 100, 0, 100)
```

Stress is normalized against **uncapped TDP**, not against the capped budget, because the cooling system is sized for the hardware that is installed, not for the cap currently applied to it. Capping a node lowers its measured power and so lowers its cooling stress, which is the intended behaviour.

| Term | Rationale |
|------|-----------|
| `measuredPower / nodeTdp` | Fraction of the node's installed thermal capacity actually in use. A node drawing its full TDP scores 100 at 20C ambient. |
| `(ambientTempC - 20) * 0.02` | Each degree above a 20C baseline shrinks the denominator by 0.02, so warm air makes the same watts cost more cooling effort. Below 20C the denominator grows and the multiplier drops below 1. |
| `max(0.5, ...)` | Floors the denominator, capping the multiplier at 2.0. The cap is reached at 45C ambient and holds for anything hotter. |

If `nodeTdp` is 0 (no hardware information) the score is 0: the twin emits no stress signal rather than a fabricated one. If the ambient temperature is 0 or below it is treated as **unknown** and the multiplier is 1.0. Facility metrics are off by default (`ENABLE_FACILITY_METRICS=false`), so an unknown ambient is the common case, and a genuine sub-zero ambient reads the same as no reading at all.

The multiplier at a few ambient temperatures:

| Ambient | Multiplier |
|---------|------------|
| unknown (0C or below) | 1.000 |
| 15C | 0.909 |
| 20C | 1.000 |
| 25C | 1.111 |
| 30C | 1.250 |
| 40C | 1.667 |
| 45C and above | 2.000 |

**Example**: the reference node drawing a measured 1160 W at 25C ambient:

- `measuredPower / nodeTdp` = 1160 / 2320 = 0.5
- `tempMultiplier` = 1 / max(0.5, 1 - (25 - 20) * 0.02) = 1 / 0.9 = 1.111
- `coolingStress` = 0.5 * 1.111 * 100 = **55.6**

The same node with an unknown ambient temperature scores 0.5 * 1.0 * 100 = **50.0**.

### Why this model

It is an algebraic proxy. It avoids CFD or thermal RC simulation and runs in O(1) per node. It is deliberately conservative (it overestimates stress relative to real cooling capacity) because its job is to provide a ranking signal for the scheduler, not an exact thermal prediction.

## Power headroom

Power headroom is the fraction of the node's capped power budget that is still unused:

```
headroom = min(100, (nodeCappedPower - measuredPower) / nodeCappedPower * 100)
```

It is clamped at 100 at the top but **not** at the bottom: a node drawing more than its budget scores negative, which ranks it below an idle node instead of tying with one. If `nodeCappedPower` is 0 (unknown hardware) the score is 100, a neutral value that avoids penalizing nodes the twin knows nothing about.

**Example**: the reference node (1392 W capped budget) drawing a measured 1160 W:

- `headroom` = (1392 - 1160) / 1392 * 100 = **16.7**

**Example**: the same node drawing 1600 W, over its eco budget:

- `headroom` = (1392 - 1600) / 1392 * 100 = **-14.9**

The scheduler prefers its own projected headroom, computed from `powerMeasurement` plus the candidate pod's marginal watts, and falls back to this `predictedPowerHeadroomScore` only for a node with no `powerMeasurement` block.

## PSUStress formula

PSUStress answers: "how close is this rack to its power supply limit?"

```
rackPower = rackTotalPowerW, or clusterTotalPowerW when no rack total is known
psuStress = clamp(rackPower / 50000 * 100, 0, 100)
```

| Term | Rationale |
|------|-----------|
| `rackTotalPowerW` | Sum of estimated power for every node carrying the same `joulie.io/rack` label. Set only when rack topology is available. |
| `clusterTotalPowerW` | Fallback: total cluster IT power from the facility metrics poller. Used when no rack total is known. |
| `50000 W` (50 kW) | Reference rack capacity, a shared constant. In a real deployment this would come from per-rack facility data. |

When neither power figure is available the score is 0. Both come from the facility metrics poller, which is disabled by default, so PSU stress is 0 on a default install.

PSUStress is per-rack when rack topology is available and cluster-wide otherwise. In the cluster-wide case every node reports the same score, which is intentional: a PDU brownout affects every node behind it, not just the one drawing the most power.

The scheduler does not read this score today. It is published for observability and reserved for rack-topology-aware placement.

**Example**: a rack drawing 30 kW:

- `psuStress` = 30000 / 50000 * 100 = **60**

## HardwareDensityScore

A normalized compute density proxy used for heterogeneous planning:

```
cpuScore = totalCores / 192 * 100
gpuScore = gpuCount / 8 * 100
density  = clamp(gpuPresent ? (cpuScore + gpuScore) / 2 : cpuScore, 0, 100)
```

The references are a 192-core CPU and an 8-GPU node. **Example**: the reference node (192 cores, 4 GPUs) scores (100 + 50) / 2 = **75**.

## Estimated PUE

```
estimatedPUE = 1.05 + (coolingStress / 100) * 0.35
```

A node at zero cooling stress reports 1.05 (best-case datacenter overhead) and one at full stress reports 1.40 (stressed cooling, high overhead). It is a restatement of cooling stress on a PUE scale, not an independent measurement, and the scheduler does not read it.

**Example**: the reference node at cooling stress 55.6 reports 1.05 + 0.556 * 0.35 = **1.24**.

## How it feeds the scheduler

The twin controller runs in the controller manager on each reconcile tick (~1 minute) and writes `NodeTwin.status` per managed node. The scheduler extender caches these `NodeTwin` CRs with a 30-second TTL and uses them in its filter and score logic:

```
twin controller (controller manager)
  → writes NodeTwin.status
    → scheduler extender cache (30s TTL)
      → filter: rejects eco nodes for performance pods
      → score: headroomScore*0.7 + (100-coolingStress)*0.15 + trendBonus + profileBonus + pressureRelief
```

Of the twin's outputs, the scheduler reads `schedulableClass`, `predictedCoolingStressScore`, the whole `powerMeasurement` block, and `predictedPowerHeadroomScore` as a fallback. `predictedPsuStressScore` and `estimatedPUE` are not part of the score.

This keeps scheduling decisions lightweight (one cache lookup per node per scheduling attempt) while reflecting the latest thermal and power state of the cluster.

## How it feeds the controller manager

The twin also drives controller manager decisions:

- **Transition guard**: when a node is transitioning from performance to eco, the twin sets `schedulableClass` to `draining` until all performance pods have completed or been drained.

## Implementation

The twin is implemented in `pkg/controller/twin/twin.go`. Key types and functions:

- `Input`: everything needed to compute twin state for one node (hardware, profile, cap percentages, measured power, power trend, facility and topology signals).
- `Output`: the computed `NodeTwin.status` fields.
- `Compute(Input) Output`: the main computation function. Derives TDP and capped budgets, then calls the helpers below.
- `computePowerHeadroom`, `computeCoolingStress`, `computePSUStress`: the three score formulas.
- `ComputeHardwareDensityScore`: the density proxy, exported for the controller manager and tests.

There is no cooling model interface and no alternative cooling implementation. Cooling stress is the single closed-form expression above; replacing it with a higher-fidelity thermal model means changing `computeCoolingStress`.

## What to read next

1. [Scheduler Extender]({{< relref "/docs/architecture/scheduler.md" >}})
2. [Joulie Controller Manager]({{< relref "/docs/architecture/controller-manager.md" >}})
3. [CRD and Policy Model]({{< relref "/docs/architecture/policy.md" >}})
4. [Hardware Modeling]({{< relref "/docs/hardware/hardware-modeling.md" >}}): reference power profiles used by `NodeHardware` and the twin's TDP derivation
