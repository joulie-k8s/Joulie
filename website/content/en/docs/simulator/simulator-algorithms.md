---
title: "Simulator Algorithms"
weight: 20
---


This page documents the core simulator math and control/workload update loop implemented in `simulator/cmd/simulator/main.go`.

## Per-Node State

Each simulated node tracks:

- `CapWatts`
- `TargetThrottlePct`
- `ThrottlePct`
- `FreqScale` in `[f_min/f_max, 1]`
- `CPUUtil` in `[0,1]`
- `CapSaturated` flag

## Power Model

At telemetry/control update time, simulator computes power:

`P = P_idle + (P_max - P_idle) * util^alpha * freqScale^beta`

Where:

- `P_idle = BaseIdleW`
- `P_max = PMaxW`
- `util = clamp(CPUUtil, 0, 1)`
- `alpha = AlphaUtil` (defaults to 1 if invalid)
- `beta = BetaFreq` (defaults to 1 if invalid)

Then applies bounds/cap handling:

1. Floor `P` to at least `20W`.
2. Clamp requested cap to `[RaplCapMinW, RaplCapMaxW]`.
3. If `P > cap`, solve a cap-feasible frequency scale:
   - `targetFreq = ((cap - P_idle) / ((P_max - P_idle) * util^alpha))^(1/beta)`
   - lower-bounded by `minFreqScale = FMinMHz/FMaxMHz`.
4. Recompute `P` using updated `FreqScale`.
5. Final clip: `P <= cap + RaplHeadW`.

`CapSaturated=true` when even minimum feasible dynamic power remains above cap.

## DVFS Dynamics

DVFS throttle is not applied as an instantaneous jump.

Given:

- `targetScale = 1 - TargetThrottlePct/100`
- `rampSec = max(0.05, DvfsRampMS/1000)`
- `dt = now - lastUpdate`

Update:

`FreqScale = FreqScale + (targetScale - FreqScale) * min(1, dt/rampSec)`

Then clamp to `[minFreqScale, 1]` and set:

`ThrottlePct = round((1 - FreqScale) * 100)`

## Control Ingestion

Control endpoint supports:

- `rapl.set_power_cap_watts`: updates `CapWatts` (with cap guardrails).
- `dvfs.set_throttle_pct`: updates `TargetThrottlePct` in `[0,100]`.

The resulting node dynamics/power are reflected immediately via the same model above.

## Workload Execution Model

Each job has:

- requested CPU cores `C_req`,
- total work `CPUUnitsTotal`,
- remaining work `CPUUnitsRemaining`,
- sensitivity `S_cpu` in `[0,1]`.

Node effective speed factor is `FreqScale`.

Per-job instantaneous speed on node:

`speed = C_req * BaseSpeedPerCore * (1 - (1 - FreqScale) * S_cpu)`

If `k` active jobs run on same node, fair-share is approximated by:

`progress = speed * dt / max(1, k)`

Update:

`CPUUnitsRemaining -= progress`

Job completes when `CPUUnitsRemaining <= 0`, then pod is deleted.

## Lifetime / Completion Estimate

At any instant, approximate remaining lifetime of one job can be estimated as:

`T_remaining ~= CPUUnitsRemaining / (speed / max(1, k))`

So tighter caps / higher throttle / higher sensitivity increase completion time by reducing effective speed.

## Scheduling-Class Inference

Simulator also derives class from pod scheduling constraints:

- `performance`
- `eco`
- `general` (implicit unconstrained)

This is used for debug counters and completion metrics labeling.
