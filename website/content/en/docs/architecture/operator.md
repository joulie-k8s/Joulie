---
title: "Operator Notes"
---


## Target concept

Joulie should evolve into a centralized operator that owns the global optimization loop.

At each control step (for example every minute), the operator:

1. Reads cluster-wide context.
2. Decides node-to-power-profile assignments.
3. Writes desired per-node state.
4. Monitors outcomes and re-plans.

States start simple:

- `ActivePerformance` (mapped to profile `performance`): unconstrained / HPC-oriented.
- `ActiveEco` (mapped to profile `eco`): constrained / energy-saving.

## Control responsibility boundary

Operator is the control-plane brain.
Agent is an actuator/telemetry component.

- Operator decides *what* should happen: profile assignments, transitions, safety rules, timing.
- Agent decides only *how to apply* on host interfaces and reports result (`applied`, `blocked`, `error`).

This keeps policy evolution independent from device-specific enforcement details.

## Transition state machine (design baseline)

To avoid contract violations during `ActivePerformance -> ActiveEco` moves, use two-phase downgrade:

1. `ActivePerformance`
2. `DrainingPerformance` (keep performance cap, stop admitting new performance workloads)
3. `ActiveEco` (commit eco cap when safe condition is met)

If safe condition never occurs, policy controls escalation (hold, timeout, force, or drain/evict strategy).

Current implementation includes a basic guard: when target is `ActiveEco` but the node still runs pods classified as performance-only from scheduling constraints (`nodeSelector`/required `nodeAffinity` on `joulie.io/power-profile`), downgrade is deferred and node remains in performance profile.
Pods with no power-profile scheduling constraint are classified as implicit unconstrained (general), not performance-only.

## Global inputs

The operator policy has a cluster-wide view and should support:

- static metadata: node location, rack/zone, reserved nodes to exclude from optimization.
- time-based rules: business-hour peak windows and fixed schedules.
- telemetry-driven rules: temperatures, PUE, hotspot indicators, power trends.
- future data-driven policies: Prometheus-fed models, external inference (for example KServe).

## Current policies

Current operator policy modules in `cmd/operator/main.go`:

- `static_partition`:
  - deterministic split of managed nodes into `performance` and `eco`;
  - controlled by `STATIC_HP_FRAC` (default `0.50` -> 50/50 split).
- `queue_aware_v1`:
  - starts from a base high-performance share (`QUEUE_HP_BASE_FRAC`);
  - raises high-performance node count when cluster-wide performance-only pod pressure grows (derived from scheduling constraints);
  - bounded by `QUEUE_HP_MIN`/`QUEUE_HP_MAX` and scaled by `QUEUE_PERF_PER_HP_NODE`.
- `rule_swap_v1`:
  - alternates eco/performance assignment across the first nodes on each reconcile tick;
  - kept only as a debugging policy to validate transitions and control-loop wiring.

Defaults and fallback:

- default `POLICY_TYPE` is `static_partition`;
- default `STATIC_HP_FRAC` is `0.50` (50/50 split);
- unknown `POLICY_TYPE` falls back to `static_partition` (not swap).

## Extensibility model

Keep policy logic pluggable:

- a common policy interface (`Evaluate` / `Plan`) returning node assignments.
- one baseline rule-based module.
- optional telemetry/model adapters as separate modules.

The core operator loop remains stable while policy modules evolve independently.

Suggested interfaces:

- `PolicyModule.Plan(context) -> node transitions`
- `ContextProvider.Snapshot() -> cluster context`
- `StateGuard.Check(node, transition) -> allowed/blocked(reason)`

Input source and actuation abstraction details are defined in:

- [Input Telemetry and Actuation Interfaces](./telemetry/)

Future data-driven policies should use Prometheus (or other sources) through `ContextProvider`, not by changing agent APIs.

## Current control path

Current path is operator-driven:

1. Operator computes node assignments.
2. Operator writes node-scoped desired state (`NodePowerProfile`).
3. Agent consumes only its node-scoped assignment.
4. Policy plugins can evolve independently (rule-based first, telemetry/AI later).

## Suggested deployment shape

- Operator Deployment in `joulie-system`.
- ServiceAccount + RBAC (read nodes/metrics, write desired-state CRs).
- Leader election.
- Operator metrics endpoint (decisions, reassignments, errors, loop latency).

Future operator metrics should also expose transition outcomes (`blocked`, `forced`, `completed`) to make policy behavior auditable in Grafana.
