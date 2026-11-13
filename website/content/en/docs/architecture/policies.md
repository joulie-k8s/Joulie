---
title: "Policy Algorithms"
weight: 40
---

A policy decides how many managed nodes run the `performance` profile; every other node runs `eco`. Pick one with `POLICY_TYPE` on the controller manager. The policies are in `pkg/controller/policy/`.

Demand is the number of pods annotated `joulie.io/workload-class: performance`; pods without the annotation are `standard`.

## Which performance nodes

`static_partition` and `queue_aware_v1` compute a count; the controller manager then splits it across hardware families (`gpu:<model>`, `cpu:<model>`) in proportion to family size, at least one node per family, keeping nodes already in `performance` first. A change of one in the count moves exactly one node. The split is exported as `joulie_policy_family_nodes{family}` and `joulie_policy_family_performance_nodes{family}`.

## `static_partition`

The default. Use it for a fixed, predictable share of performance nodes: about `STATIC_HP_FRAC` of every family.

| Knob | Default | Meaning |
|---|---|---|
| `STATIC_HP_FRAC` | `0.50` | fraction of nodes in `performance`: `round(N * STATIC_HP_FRAC)` |

Exercised in the [CPU-Only Benchmark]({{< relref "/docs/experiments/cpu-only-benchmark.md" >}}) and the [Heterogeneous GPU Cluster Benchmark]({{< relref "/docs/experiments/heterogeneous-benchmark.md" >}}).

## `queue_aware_v1`

Use it when performance demand varies: the count grows with the performance pods that are running or pending.

| Knob | Default | Meaning |
|---|---|---|
| `QUEUE_HP_BASE_FRAC` | `0.60` | base fraction of nodes in `performance` |
| `QUEUE_PERF_PER_HP_NODE` | `10` | performance pods one performance node absorbs |
| `QUEUE_HP_MIN` | `1` | lower bound of the count |
| `QUEUE_HP_MAX` | `1000000` | upper bound of the count |

The count is `max(round(N * QUEUE_HP_BASE_FRAC), ceil(P / QUEUE_PERF_PER_HP_NODE))`, clamped to `[QUEUE_HP_MIN, QUEUE_HP_MAX]`, with `P` the running and pending performance pods. Exercised in the same two benchmarks.

## `rule_swap_v1` (debug policy)

For debugging only: it alternates which of the first two nodes, by name, is `eco` every `RECONCILE_INTERVAL`, so transitions are visible. It has no knobs.

## Downgrade Guard

A node planned for `eco` that still runs performance pods becomes `draining`: it keeps its performance caps, and the scheduler extender sends no new performance pods to it. When those pods finish it gets the eco caps. Nothing is evicted.

Step-by-step algorithms, properties and a worked example: [Policy algorithms](https://github.com/joulie-k8s/Joulie/wiki/Policy-Algorithms) on the wiki.
