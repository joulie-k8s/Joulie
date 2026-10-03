---
title: "Policy Algorithms"
weight: 40
---


This page documents the controller policy algorithms implemented in `pkg/controller/policy/`.

Use this page after:

1. [CRD and Policy Model]({{< relref "/docs/architecture/policy.md" >}})
2. [Joulie Controller Manager]({{< relref "/docs/architecture/controller-manager.md" >}})

## Classification Input

Policy demand classification is derived from the `joulie.io/workload-class` pod annotation:

- `performance`: pod carries `joulie.io/workload-class: performance`.
- `standard` (default): no annotation or `joulie.io/workload-class: standard`.

## Shared Reconcile Flow

Each reconcile tick:

1. Select eligible nodes from `NODE_SELECTOR`, excluding reserved and unschedulable nodes.
2. Build a hardware view from `NodeHardware` when available, otherwise from node labels/inventory fallback.
3. Build a desired plan with the selected policy.
4. Apply downgrade guard (sets `NodeTwin.status.schedulableClass` to `draining` while blocking pods still run).
5. Write `NodeTwin.spec` and update the `joulie.io/power-profile` node label.

In other words, `static_partition` and `queue_aware_v1` decide *how many* nodes stay in `performance`, and the per-family split below decides *which* nodes those are.

## Performance Node Selection

`static_partition` and `queue_aware_v1` compute a performance slot count `hp_count` and pass it to `policy.PerformanceSet`:

1. Group the eligible nodes into hardware families (`policy.NodeFamily`): `gpu:<model>` when the node has GPUs, otherwise `cpu:<model>`.
2. Raise `hp_count` to the number of families and cap it at the number of nodes.
3. Give every family one slot. Each further slot goes to the family with the largest `size / (2 * slots + 1)`, ties to the smaller family key (the Sainte-Lague divisor method). A family never gets more slots than it has nodes.
4. Within a family, fill the slots with nodes whose current `joulie.io/power-profile` label is `performance` first, then by node name.

Properties:

- every family keeps performance capacity in proportion to its size, and at least one node,
- house monotone: when `hp_count` changes by one, exactly one node changes profile,
- a reconcile at constant demand moves no node, because nodes already in `performance` are kept first,
- the result does not depend on the order of the input node list.

Example: the exp02 5k inventory (`experiments/02-heterogeneous-benchmark/configs/cluster-nodes-5k.yaml`, 5000 nodes in 8 families) with 1000 performance slots (`STATIC_HP_FRAC=0.2`) keeps a fifth of every family in `performance`:

| Family | Nodes | Performance |
|---|---|---|
| h100-nvl | 1450 | 290 |
| h100-sxm | 730 | 146 |
| l40s | 850 | 170 |
| mi300x | 240 | 48 |
| w7900 | 730 | 146 |
| cpu-highcore | 250 | 50 |
| cpu-highfreq | 250 | 50 |
| cpu-intensive | 500 | 100 |

`TestPerformanceSetMatchesDocumentedExp02Split` in `pkg/controller/policy/policy_test.go` pins this split. The standalone simulator calls the same `policy.PerformanceSet`.

The controller manager exports the split after the downgrade guard as `joulie_policy_family_nodes{family}` and `joulie_policy_family_performance_nodes{family}`, so a draining node counts as not performance.

## `static_partition`

Goal: deterministic fixed HP/LP split.

Inputs:

- `N`: number of eligible nodes.
- `STATIC_HP_FRAC`: target fraction of high-performance nodes.

Algorithm:

1. `hp_count = round(N * STATIC_HP_FRAC)`.
2. Pick the performance nodes with the [per-family split](#performance-node-selection), which raises `hp_count` to the number of families and caps it at `N`.
3. Remaining nodes -> `eco`.

Properties:

- deterministic,
- stable over time unless node set changes.
- keeps about `STATIC_HP_FRAC` of every hardware family in `performance`, and at least one node per family.

This policy is exercised in the [CPU-Only Benchmark]({{< relref "/docs/experiments/cpu-only-benchmark.md" >}}) and [Heterogeneous GPU Cluster Benchmark]({{< relref "/docs/experiments/heterogeneous-benchmark.md" >}}).

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
5. Pick the performance nodes with the [per-family split](#performance-node-selection), which raises `hp_count` to the number of families and caps it at `N`.
6. Remaining nodes -> `eco`.

Properties:

- deterministic for a fixed `(N, P)` and the current profile labels,
- monotonic in pressure `P`,
- bounded by min/max limits, except that every family keeps at least one performance node,
- heterogeneous-aware because slots follow family size, and a change of `hp_count` by one moves exactly one node.

This policy is exercised in the [CPU-Only Benchmark]({{< relref "/docs/experiments/cpu-only-benchmark.md" >}}) and [Heterogeneous GPU Cluster Benchmark]({{< relref "/docs/experiments/heterogeneous-benchmark.md" >}}).

## `rule_swap_v1` (debug policy)

Goal: force visible state transitions for debugging.

Algorithm:

1. Compute phase from wall-clock and `RECONCILE_INTERVAL`.
2. Alternate which of the first two eligible nodes, in node-name order, is assigned `eco`.
3. Others remain `performance`.

This policy is intended for debugging only, not as default production behavior.

## Downgrade Guard

When planned profile is `eco` on a node currently `performance`:

1. Count active performance pods on that node.
2. If count > 0:
   - keep desired profile as `eco`,
   - set `NodeTwin.status.schedulableClass` to `draining`,
   - record transition as deferred in controller manager FSM/metrics.
3. If count == 0:
   - keep desired profile `eco`,
   - set `NodeTwin.status.schedulableClass` to `eco`.

The scheduler extender reads `schedulableClass` and filters `draining` nodes out for performance pods, as it does `eco` nodes, so no new performance work lands on a node during the transition. Standard pods are not filtered.
