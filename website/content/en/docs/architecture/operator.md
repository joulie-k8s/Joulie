---
title: "Joulie Operator"
weight: 20
---

The operator is Joulie's cluster-level decision engine.

It does not write host power interfaces directly.
Instead, it decides desired node states and publishes them through Kubernetes objects and labels.

In practice, the operator answers one question over and over:
which nodes should currently supply `performance` capacity, and which can safely supply `eco` capacity?

## Responsibilities

At each reconcile tick, the operator:

1. selects eligible managed nodes,
2. classifies workload demand from pod scheduling constraints,
3. runs a policy algorithm to compute a plan,
4. applies transition guards for safe downgrades,
5. writes desired node targets (`NodePowerProfile`) and node supply labels.

The agent then enforces those targets node-by-node.

## Control boundary with the agent

- operator decides **what** each node should be
- agent decides **how** to apply the corresponding controls on that node

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
   - publish `profile=eco` as desired state
   - keep `joulie.io/draining=true` while performance-sensitive pods are still present
6. Persist desired state through `NodePowerProfile` and update node labels:
   - `joulie.io/power-profile`
   - `joulie.io/draining`

The important distinction is:

- `NodePowerProfile` expresses desired target state for enforcement,
- node labels express scheduler-facing supply state during transitions.

## Power intent configuration knobs

Operator intent emission is controlled by env vars:

- CPU:
  - `CPU_WRITE_ABSOLUTE_CAPS` (`true|false`)
  - `CPU_PERFORMANCE_CAP_PCT_OF_MAX`
  - `CPU_ECO_CAP_PCT_OF_MAX`
  - `PERFORMANCE_CAP_WATTS`
  - `ECO_CAP_WATTS`
- GPU:
  - `GPU_PERFORMANCE_CAP_PCT_OF_MAX`
  - `GPU_ECO_CAP_PCT_OF_MAX`
  - `GPU_WRITE_ABSOLUTE_CAPS` (`true|false`)
  - `GPU_MODEL_CAPS_JSON`
  - `GPU_PRODUCT_LABEL_KEYS`

High-level behavior:

- CPU:
  - when `CPU_WRITE_ABSOLUTE_CAPS=false`, operator writes normalized percentage intent,
  - when `CPU_WRITE_ABSOLUTE_CAPS=true`, operator writes absolute watts intent.
- GPU:
  - when `GPU_WRITE_ABSOLUTE_CAPS=false`, operator writes percentage intent,
  - when `GPU_WRITE_ABSOLUTE_CAPS=true`, operator may write resolved `capWattsPerGpu` in addition to `capPctOfMax`, when model-based mapping is available.

This is why GPU `NodePowerProfile` objects may contain both normalized intent and resolved absolute caps at the same time.

## Node state model

Joulie models two scheduler-facing supply states:

- `performance`
- `eco`

`DrainingPerformance` is an internal operator FSM state tracked while `profile=eco` and `joulie.io/draining=true`.

That state means:

- the operator wants the node to end up in eco,
- the transition is still guarded because performance-sensitive pods are present,
- advanced eco-only placement can avoid the node until draining clears.

## Why this model

- scheduler gets clear supply signal from node labels,
- policy can evolve independently of host control implementation,
- transitions are auditable and safer than instant downgrade.

## What to read next

1. [Joulie Agent]({{< relref "/docs/architecture/agent.md" >}})
2. [Policy Algorithms]({{< relref "/docs/architecture/policies.md" >}})
3. [Input Telemetry and Actuation Interfaces]({{< relref "/docs/architecture/telemetry.md" >}})
