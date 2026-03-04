# KWOK Benchmark Investigation Report (2026-03-04)

## Scope

Investigate why benchmark plots show no meaningful difference across baselines/policies (A/B/C) in throughput or energy.

Analyzed run set:

- `20260304T203834Z_bA_s1`
- `20260304T204255Z_bB_s1`
- `20260304T204715Z_bC_s1`

## Executive Summary

No visible policy separation is expected from these runs because:

1. Baseline B and C converge to the same effective node profile split (3 performance, 2 eco) for the full run.
2. Queue-aware policy is not sufficiently stressed by the current workload and parameters to diverge from static behavior.
3. Runs hit timeout (~240s) before workload completion, so throughput comparisons are not representative.
4. Baseline A is not a valid comparator in this run set because workload pods remain Pending (no matching power-profile labels).
5. Throttling is actually being applied; lack of difference is not caused by missing control actuation.
6. Energy estimation is based on a capped debug-event ring (200 events), so it represents tail-window integration, not full-run integration.

## Detailed Findings

### 1) B and C produce the same plan over time

Operator logs show repeated assignment of:

- `kwok-node-0/1/2 = performance`
- `kwok-node-3/4 = eco`

for both static and queue-aware runs.

Evidence:

- `results/20260304T204255Z_bB_s1/operator.log`
- `results/20260304T204715Z_bC_s1/operator.log`
- `results/20260304T204255Z_bB_s1/nodepowerprofiles.yaml`
- `results/20260304T204715Z_bC_s1/nodepowerprofiles.yaml`

### 2) Queue-aware has no reason to diverge with current load

The trace contains 20 jobs with affinity-derived class split:

- performance-only: 6
- eco-only: 14

With `QUEUE_PERF_PER_HP_NODE=10`, queue pressure from performance demand does not exceed the base HP target in this setup, so C stays aligned with B.

Evidence:

- `results/20260304T204255Z_bB_s1/trace.jsonl`
- install/config path: `scripts/03_install_components.sh`

### 3) Runs timeout before completion

`summary.csv` shows all runs at about 242s wall-time, matching timeout configuration.
Simulator logs show almost no completions (0-1), so throughput is not measuring completed workload behavior.

Evidence:

- `results/summary.csv`
- `results/20260304T204255Z_bB_s1/simulator.log`
- `results/20260304T204715Z_bC_s1/simulator.log`

### 4) Baseline A is invalid in this specific setup

Baseline A runs simulator-only (no operator/agent), so no `joulie.io/power-profile` node labels are maintained.
Workload pods in this trace require `performance` or `eco` affinity, so they remain Pending.

Evidence:

- `results/20260304T203834Z_bA_s1/pods.json` (workload pods pending)
- `results/20260304T203834Z_bA_s1/nodepowerprofiles.yaml` (empty list)

### 5) Throttling is applied in practice

Agent and simulator logs show repeated DVFS control actions and non-zero throttle percentages, especially on eco nodes.

Evidence:

- `results/20260304T204255Z_bB_s1/agent.log`
- `results/20260304T204255Z_bB_s1/simulator.log`
- `results/20260304T204715Z_bC_s1/agent.log`
- `results/20260304T204715Z_bC_s1/simulator.log`

### 6) Energy estimate limitations in current data path

`sim_debug_events.json` captures a bounded ring buffer (`count=200`). Energy integration from this source is therefore based on the most recent event window, not guaranteed full-run coverage.

Evidence:

- `results/20260304T203834Z_bA_s1/sim_debug_events.json`
- `results/20260304T204255Z_bB_s1/sim_debug_events.json`
- `results/20260304T204715Z_bC_s1/sim_debug_events.json`

## Root Cause Statement

The primary issue is experiment setup and measurement path (policy regimes not sufficiently separated, incomplete runs, and baseline A mismatch), not simply the simulator mathematical model.

## Implications

- Current A/B/C plots from this run set should not be interpreted as evidence that policies are equivalent.
- A valid comparison requires:
  - a runnable baseline A with compatible scheduling constraints,
  - completion-based workload metrics (not timeout-truncated),
  - and policy parameters/workload pressure that force B/C to diverge.
