# Operator Notes

## Target concept

Joulie should evolve into a centralized operator that owns the global optimization loop.

At each control step (for example every minute), the operator:

1. Reads cluster-wide context.
2. Decides node-to-power-profile assignments.
3. Writes desired per-node state.
4. Monitors outcomes and re-plans.

Profiles start simple:

- `performance`: unconstrained / HPC-oriented.
- `eco`: constrained / energy-saving.

## Global inputs

The operator policy has a cluster-wide view and should support:

- static metadata: node location, rack/zone, reserved nodes to exclude from optimization.
- time-based rules: business-hour peak windows and fixed schedules.
- telemetry-driven rules: temperatures, PUE, hotspot indicators, power trends.
- future data-driven policies: Prometheus-fed models, external inference (for example KServe).

## Minimal first policy

Start with a deterministic rule-based policy for validation:

- small set of target nodes (for example 2 nodes).
- every `X` minutes, alternate assignments between `performance` and `eco`.

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
