---
title: "Policy Algorithms"
weight: 40
---


This page documents the controller policy algorithms implemented in `cmd/operator/main.go`.

Use this page after:

1. [CRD and Policy Model]({{< relref "/docs/architecture/policy.md" >}})
2. [Joulie Operator]({{< relref "/docs/architecture/operator.md" >}})

## Classification Input

Policy demand classification is derived from pod scheduling constraints on `joulie.io/power-profile`:

- `performance-only`: pod excludes eco in required scheduling constraints.
- `eco-only`: pod can run only on `eco`.
- `general` (implicit unconstrained): no explicit power-profile constraint, or both profiles allowed.

## Shared Reconcile Flow

Each reconcile tick:

1. Select eligible nodes from `NODE_SELECTOR`, excluding reserved and unschedulable nodes.
2. Build a hardware view from `NodeHardware` when available, otherwise from node labels/inventory fallback.
3. Sort eligible nodes by normalized compute density (highest first).
4. Build a desired plan with the selected policy.
5. Apply downgrade guard (sets `draining=true` while blocking pods still run).
6. Write `NodePowerProfile` and update node labels (`joulie.io/power-profile`, `joulie.io/draining`).

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
4. Break ties lexicographically.
5. First `hp_count` nodes -> `performance`; remaining -> `eco`.

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
6. Sort nodes by compute density descending.
7. Break ties lexicographically.
8. First `hp_count` nodes -> `performance`; remaining -> `eco`.

Properties:

- deterministic for a fixed `(N, P)`,
- monotonic in pressure `P`,
- bounded by min/max limits,
- heterogeneous-aware because denser nodes are preferred first.

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
   - keep desired profile as `eco`,
   - set node label `joulie.io/draining=true`,
   - record transition as deferred in operator FSM/metrics.
3. If count == 0:
   - keep desired profile `eco`,
   - set node label `joulie.io/draining=false`.
