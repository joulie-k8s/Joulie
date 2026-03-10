---
title: "Power Simulator"
weight: 30
---

This page documents the power/control-side simulation model.

## Scope

The power simulator handles:

- node power-state variables,
- RAPL/DVFS control ingestion,
- frequency ramp dynamics,
- power computation and capping,
- energy integration.

Workload execution model is documented separately in:

- [Workload Simulator]({{< relref "/docs/simulator/workload-simulator.md" >}})

## Per-node state

Each simulated node tracks:

- `CapWatts`
- `TargetThrottlePct`
- `ThrottlePct`
- `FreqScale` in `[f_min/f_max, 1]`
- `CPUUtil` in `[0,1]`
- `CapSaturated`

## Power model

At update time:

`P = P_idle + (P_max - P_idle) * util^alpha * freqScale^beta`

Where:

- `P_idle = BaseIdleW`
- `P_max = PMaxW`
- `util = clamp(CPUUtil, 0, 1)`
- `alpha = AlphaUtil`
- `beta = BetaFreq`

Then bounds/cap handling:

1. Floor `P` to minimum `20W`.
2. Clamp requested cap to `[RaplCapMinW, RaplCapMaxW]`.
3. If `P > cap`, solve cap-feasible frequency scale:
   - `targetFreq = ((cap - P_idle) / ((P_max - P_idle) * util^alpha))^(1/beta)`
   - lower bound `minFreqScale = FMinMHz/FMaxMHz`.
4. Recompute `P` with updated `FreqScale`.
5. Final clip: `P <= cap + RaplHeadW`.

`CapSaturated=true` when even minimum feasible dynamic power remains above cap.

## DVFS dynamics

Throttle changes are ramped, not instantaneous.

Given:

- `targetScale = 1 - TargetThrottlePct/100`
- `rampSec = max(0.05, DvfsRampMS/1000)`
- `dt = now - lastUpdate`

Update:

`FreqScale = FreqScale + (targetScale - FreqScale) * min(1, dt/rampSec)`

Then:

- clamp `FreqScale` to `[minFreqScale, 1]`,
- set `ThrottlePct = round((1 - FreqScale) * 100)`.

## Control ingestion

Supported actions:

- `rapl.set_power_cap_watts` -> update `CapWatts` (with guardrails)
- `dvfs.set_throttle_pct` -> update `TargetThrottlePct` in `[0,100]`

Unsupported GPU actions are currently returned as `blocked`.

## Energy integration

Simulator integrates per-node and total energy over time:

- per tick: `E += P * dt`
- totals are exposed via `/debug/energy`

Prometheus metrics expose instantaneous state; integrated totals are debug-API JSON in current implementation.

## Why this model

- reflects effect of RAPL and DVFS controls on power and effective frequency,
- couples naturally with workload simulator slowdown behavior,
- supports policy comparison by energy/throughput tradeoffs in large virtual clusters.
