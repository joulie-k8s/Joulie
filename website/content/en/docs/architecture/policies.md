---
title: "Policy Algorithms"
---


This page documents the controller policy algorithms implemented in `cmd/operator/main.go`.

## Classification Input

Policy demand classification is derived from pod scheduling constraints on `joulie.io/power-profile`:

- `performance-only`: pod can run only on `performance`/`draining-performance`.
- `eco-only`: pod can run only on `eco`.
- `general` (implicit unconstrained): no explicit power-profile constraint, or both profiles allowed.
- `unknown`: unsupported/ambiguous constraint shape.

For safety, `unknown` is treated as performance-sensitive in downgrade guards.

## Shared Reconcile Flow

Each reconcile tick:

1. Select eligible nodes from `NODE_SELECTOR`, excluding reserved and unschedulable nodes.
2. Build a desired plan with the selected policy.
3. Apply downgrade guard (can convert planned `eco` to `draining-performance`/`performance`).
4. Write `NodePowerProfile` and update node label `joulie.io/power-profile`.

## `static_partition`

Goal: deterministic fixed HP/LP split.

Inputs:

- `N`: number of eligible nodes.
- `STATIC_HP_FRAC`: target fraction of high-performance nodes.

Algorithm:

1. `hp_count = round(N * STATIC_HP_FRAC)`.
2. Clamp `hp_count` to `[0, N]`.
3. Sort eligible nodes lexicographically.
4. First `hp_count` nodes -> `performance`; remaining -> `eco`.

Properties:

- deterministic,
- stable over time unless node set changes.

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
6. Sort nodes lexicographically.
7. First `hp_count` nodes -> `performance`; remaining -> `eco`.

Properties:

- deterministic for a fixed `(N, P)`,
- monotonic in pressure `P`,
- bounded by min/max limits.

## `rule_swap_v1` (debug policy)

Goal: force visible state transitions for debugging.

Algorithm:

1. Compute phase from wall-clock and `RECONCILE_INTERVAL`.
2. Alternate which of the first nodes is assigned `eco`.
3. Others remain `performance`.

This policy is intended for debugging only, not as default production behavior.

## Downgrade Guard

When planned profile is `eco` on a node currently `performance`:

1. Count active performance-sensitive pods on that node.
2. If count > 0:
   - keep cap/profile as `performance`,
   - set label profile to `draining-performance`.
3. If count == 0:
   - allow transition to `eco`.

If node is already `draining-performance`, the same check decides whether to complete drain or remain draining.
