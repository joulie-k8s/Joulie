# Operator Notes

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

## Global inputs

The operator policy has a cluster-wide view and should support:

- static metadata: node location, rack/zone, reserved nodes to exclude from optimization.
- time-based rules: business-hour peak windows and fixed schedules.
- telemetry-driven rules: temperatures, PUE, hotspot indicators, power trends.
- future data-driven policies: Prometheus-fed models, external inference (for example KServe).

## Minimal first policy

Start with a deterministic rule-based policy for validation:

- small set of target nodes (for example 2 nodes).
- every `X` minutes, alternate assignments between `ActivePerformance` and `ActiveEco` (profile mapping `performance`/`eco`).

This validates:

- end-to-end control loop.
- remote assignment propagation.
- observed effect in metrics.

Current implementation includes this baseline policy as `rule-swap-v1` in `cmd/operator/main.go`.

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

Future data-driven policies should use Prometheus (or other sources) through `ContextProvider`, not by changing agent APIs.

## Migration path from current PoC

Current state is agent-driven (`PowerPolicy` self-selection per node).

Migration path:

1. Keep `PowerPolicy` as user intent surface.
2. Introduce operator-owned node-scoped desired state (`NodePowerProfile`).
3. Switch agents to consume only their node-scoped assignment.
4. Add policy plugins incrementally (rule-based first, telemetry/AI later).

## Suggested deployment shape

- Operator Deployment in `joulie-system`.
- ServiceAccount + RBAC (read nodes/metrics, write desired-state CRs).
- Leader election.
- Operator metrics endpoint (decisions, reassignments, errors, loop latency).

Future operator metrics should also expose transition outcomes (`blocked`, `forced`, `completed`) to make policy behavior auditable in Grafana.
