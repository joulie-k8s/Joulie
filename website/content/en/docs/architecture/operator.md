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
2. reads `NodeHardware` when available and falls back to node labels when it is not,
3. resolves hardware identity against the shared inventory,
4. classifies workload demand from pod scheduling constraints and `WorkloadProfile` data,
5. runs a policy algorithm (`pkg/operator/policy/`) to compute a plan,
6. applies transition guards for safe downgrades,
7. evaluates node stress for migration recommendations (`pkg/operator/migration/`),
8. writes desired node targets (`NodeTwin.spec`) and the `joulie.io/power-profile` node label.

The agent then enforces those targets node-by-node.

## Migration recommendations

When node stress (CoolingStress or PSUStress) exceeds configured thresholds, the migration controller (`pkg/operator/migration/`) evaluates which workloads can be safely rescheduled. It generates reschedule recommendations written to `NodeTwin.status.rescheduleRecommendations` for reschedulable standard workloads. These recommendations are surfaced via `kubectl joulie recommend`.

## Control boundary with the agent

- operator decides **what** each node should be
- agent decides **how** to apply the corresponding controls on that node

This separation keeps policy logic portable while actuator details stay node-local.

## Reconcile flow

1. Read nodes matching `NODE_SELECTOR` (chart default: `joulie.io/managed=true`).
2. Ignore reserved/unschedulable nodes.
3. Build a normalized hardware view:
   - prefer `NodeHardware`
   - otherwise derive hardware identity from node labels / allocatable resources
   - resolve CPU/GPU models against the inventory
   - compute per-node CPU/GPU density signals
4. Build demand view from active pods:
   - performance-constrained
   - eco-constrained
   - unconstrained
5. Sort eligible nodes by normalized compute density (CPU + GPU), highest first.
6. Run policy (`static_partition`, `queue_aware_v1`, or debug `rule_swap_v1`).
7. For planned `performance -> eco` transitions, run downgrade guard:
   - publish `profile=eco` as desired state
   - set `NodeTwin.status.schedulableClass` to `draining` while performance pods are still present
8. Persist desired state through `NodeTwin.spec` and update the `joulie.io/power-profile` node label.

The important distinction is:

- `NodeTwin.spec` expresses desired target state for enforcement,
- `joulie.io/power-profile` node label expresses the current power profile,
- `NodeTwin.status` holds twin output including `schedulableClass`, which expresses transition state (including `draining`) for the scheduler extender.

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

This is why GPU `NodeTwin.spec` objects may contain both normalized intent and resolved absolute caps at the same time.

## Heterogeneous planning

The operator is now inventory-aware.

Its first heterogeneous-planning input is a normalized compute-density score built from:

- recognized CPU model + socket/core shape
- recognized GPU model + GPU count

This score is used to order eligible nodes before policy assignment.
So, for the same policy parameters, denser nodes are preferred first for `performance` supply.

If `NodeHardware` is not available yet:

- the operator derives a best-effort hardware view from labels such as `joulie.io/hw.cpu-model`, `joulie.io/hw.gpu-model`, `joulie.io/hw.gpu-count`,
- and from allocatable extended resources (`nvidia.com/gpu`, `amd.com/gpu`).

That keeps simulator-first and bootstrap scenarios working without making `NodeHardware` a hand-authored prerequisite.

## Node state model

Joulie models two scheduler-facing supply states:

- `performance`
- `eco`

`DrainingPerformance` is an internal operator FSM state tracked via `NodeTwin.status.schedulableClass = "draining"`.

That state means:

- the operator wants the node to end up in eco,
- the transition is still guarded because performance pods are still present,
- the scheduler extender sees `schedulableClass: draining` and applies a score penalty to avoid placing new workloads on the node.

## Why this model

- scheduler gets clear supply signal from node labels,
- policy can evolve independently of host control implementation,
- transitions are auditable and safer than instant downgrade.

## What to read next

1. [Joulie Agent]({{< relref "/docs/architecture/agent.md" >}})
2. [Policy Algorithms]({{< relref "/docs/architecture/policies.md" >}})
3. [Input Telemetry and Actuation Interfaces]({{< relref "/docs/architecture/telemetry.md" >}})
