---
title: "Policy Algorithms"
weight: 40
---


This page documents the controller policy algorithms implemented in `cmd/operator/main.go`.

Use this page after:

1. [CRD and Policy Model]({{< relref "/docs/architecture/policy.md" >}})
2. [Joulie Operator]({{< relref "/docs/architecture/operator.md" >}})

## Classification Input

Policy demand classification is derived from the `joulie.io/workload-class` pod annotation or a matching `WorkloadProfile`:

- `performance`: pod carries `joulie.io/workload-class: performance`.
- `best-effort`: pod carries `joulie.io/workload-class: best-effort`.
- `standard` (default): no annotation or `joulie.io/workload-class: standard`.

## Shared Reconcile Flow

Each reconcile tick:

1. Select eligible nodes from `NODE_SELECTOR`, excluding reserved and unschedulable nodes.
2. Build a hardware view from `NodeHardware` when available, otherwise from node labels/inventory fallback.
3. Sort eligible nodes by normalized compute density (highest first).
4. Preserve at least one performance-capable node per discovered hardware family whenever the requested HP count allows it.
5. Build a desired plan with the selected policy.
6. Apply downgrade guard (sets `NodeTwinState.schedulableClass` to `draining` while blocking pods still run).
7. Write `NodePowerProfile` and update the `joulie.io/power-profile` node label.

In other words, policies still decide *how many* high-performance nodes are needed, but the density-aware ordering influences *which* nodes get those assignments.

## `static_partition`

Goal: deterministic fixed HP/LP split.

Inputs:

- `N`: number of eligible nodes.
- `STATIC_HP_FRAC`: target fraction of high-performance nodes.

Algorithm:

1. `hp_count = round(N * STATIC_HP_FRAC)`.
2. Clamp `hp_count` to `[0, N]`.
3. Sort eligible nodes by compute density descending.
4. Reserve at least one performance node per hardware family (GPU model for GPU nodes, CPU model for CPU-only nodes).
5. Fill the remaining performance slots by density order.
6. Remaining nodes -> `eco`.

Properties:

- deterministic,
- stable over time unless node set changes.
- keeps at least some performance supply across heterogeneous hardware families.

## `queue_aware_v1`

Goal: adapt HP count to current performance-only pressure.

Inputs:

- `N`: number of eligible nodes.
- `P`: count of active performance-sensitive pods cluster-wide.
- `QUEUE_HP_BASE_FRAC`
- `QUEUE_HP_MIN`
- `QUEUE_HP_MAX`
- `QUEUE_PERF_PER_HP_NODE`

Algorithm:

1. `base = round(N * QUEUE_HP_BASE_FRAC)`.
2. `need = ceil(P / QUEUE_PERF_PER_HP_NODE)`.
3. `hp_count = max(base, need)`.
4. Clamp `hp_count` to `[QUEUE_HP_MIN, QUEUE_HP_MAX]`.
5. Clamp again to `[0, N]`.
6. Reserve at least one performance node per hardware family.
7. Fill the remaining performance slots by density order.
8. Remaining nodes -> `eco`.

Properties:

- deterministic for a fixed `(N, P)`,
- monotonic in pressure `P`,
- bounded by min/max limits,
- heterogeneous-aware because denser nodes are preferred first while each family keeps some performance capacity.

## `rule_swap_v1` (debug policy)

Goal: force visible state transitions for debugging.

Algorithm:

1. Compute phase from wall-clock and `RECONCILE_INTERVAL`.
2. Alternate which of the first nodes is assigned `eco`.
3. Others remain `performance`.

This policy is intended for debugging only, not as default production behavior.

## Downgrade Guard

When planned profile is `eco` on a node currently `performance`:

1. Count active performance pods on that node.
2. If count > 0:
   - keep desired profile as `eco`,
   - set `NodeTwinState.schedulableClass` to `draining`,
   - record transition as deferred in operator FSM/metrics.
3. If count == 0:
   - keep desired profile `eco`,
   - set `NodeTwinState.schedulableClass` to `eco`.

The scheduler extender reads `schedulableClass` and applies a -20 score penalty for draining nodes, discouraging new workload placement during transitions.
