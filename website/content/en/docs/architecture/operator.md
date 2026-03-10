---
title: "Joulie Operator"
weight: 20
---

The operator is Joulie's cluster-level decision engine.

It does not write host power interfaces directly.
Instead, it decides desired node states and publishes them through Kubernetes objects and labels.

## Responsibilities

At each reconcile tick, the operator:

1. selects eligible managed nodes,
2. classifies workload demand from pod scheduling constraints,
3. runs a policy algorithm to compute a plan,
4. applies transition guards for safe downgrades,
5. writes desired node targets (`NodePowerProfile`) and node supply labels.

The agent then enforces those targets node-by-node.

## Control boundary with the agent

- operator decides **what** each node should be (`performance` or `eco`)
- agent decides **how** to apply controls on node interfaces

This separation keeps policy logic portable while actuator details stay node-local.

## Reconcile flow

1. Read nodes matching `NODE_SELECTOR` (chart default: `joulie.io/managed=true`).
2. Ignore reserved/unschedulable nodes.
3. Build demand view from active pods:
   - performance-constrained
   - eco-constrained
   - unconstrained
4. Run policy (`static_partition`, `queue_aware_v1`, or debug `rule_swap_v1`).
5. For planned `performance -> eco` transitions, run downgrade guard:
   - if performance-only pods are still present on node, defer downgrade
   - move node label/state to `draining-performance` until safe
6. Persist desired state through `NodePowerProfile` and update node label `joulie.io/power-profile`.

## Node state model

Joulie models three scheduler-facing supply states:

- `performance`
- `draining-performance`
- `eco`

`draining-performance` means "target eco, but downgrade currently deferred by guard conditions."

## Why this model

- scheduler gets clear supply signal from node labels,
- policy can evolve independently of host control implementation,
- transitions are auditable and safer than instant downgrade.

## What to read next

1. [Joulie Agent]({{< relref "/docs/architecture/agent.md" >}})
2. [Policy Algorithms]({{< relref "/docs/architecture/policies.md" >}})
3. [Input Telemetry and Actuation Interfaces]({{< relref "/docs/architecture/telemetry.md" >}})
