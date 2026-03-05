---
title: "CRD and Policy Model"
---


## CRD

The implemented APIs are:

- Group: `joulie.io`
- Version: `v1alpha1`
- `NodePowerProfile` (`nodepowerprofiles`, cluster-scoped) for operator-assigned per-node desired state
- `TelemetryProfile` (`telemetryprofiles`, cluster-scoped) for telemetry source configuration (node + cluster scope)

CRD files:

- `config/crd/bases/joulie.io_nodepowerprofiles.yaml`
- `config/crd/bases/joulie.io_telemetryprofiles.yaml`

## Conceptual model (next step)

Policy should be modeled as a cluster-wide mapping:

- input: cluster context at time `t`
- output: `node -> power state`

Minimal states:

- `ActivePerformance` (mapped to profile `performance`)
- `ActiveEco` (mapped to profile `eco`)

Initial implementation should remain rule-based and deterministic.
Future implementations can be telemetry-driven or model-driven.

## Policy core: state machine + planner abstraction

Policy should be structured as a planner over a transition state machine, not only as a direct cap assignment.

Proposed minimal node states:

- `ActivePerformance`
- `DrainingPerformance`
- `ActiveEco`

Transition intent:

- `ActiveEco -> ActivePerformance`: always allowed.
- `ActivePerformance -> DrainingPerformance`: allowed when policy decides downgrade should start.
- `DrainingPerformance -> ActiveEco`: only when guard condition is satisfied (for example no performance-required pods remain), or when a force rule triggers.

This keeps downgrade behavior explicit and safe.

Detailed algorithm definitions for implemented policies are documented in:

- [Policy Algorithms](./policies/)

## `NodePowerProfile` fields (current)

- `spec.nodeName` (required)
- `spec.profile` (required, `performance|eco`)
- `spec.cpu.packagePowerCapWatts` (optional, number)
- `spec.policy.name` (optional, metadata string)

## `NodePowerProfile` vs `TelemetryProfile`

- `NodePowerProfile` answers: **what** power profile/cap should be applied to a node.
- `TelemetryProfile` answers: **how** the agent reads inputs and applies controls (host/http/prometheus routing by signal family).

Current runtime flow:

1. Operator sets `NodePowerProfile`.
2. Agent reads local `NodePowerProfile`.
3. Agent reads node-scoped `TelemetryProfile`.
4. Agent enforces and reports status in `TelemetryProfile.status.control`.

## Selection behavior (current)

On each node, agent resolves desired state only from:

1. `NodePowerProfile` with `spec.nodeName == <node>`.

## Scheduling-aware contract (policy-owned)

The policy layer should own workload safety checks before downgrades:

- performance-required workload on node: block or defer downgrade,
- eco workloads only: allow downgrade.

Node labels communicate supply (`joulie.io/power-profile=performance|draining-performance|eco`), while workload scheduling constraints communicate demand.
Default scheduler remains unchanged.

### Workload scheduling classes

Supported classes (derived by policy from pod scheduling constraints):

- `performance`: workload should run on nodes with performance supply.
- `eco`: workload should run on nodes with eco supply.
- unconstrained (implicit): no power-profile scheduling constraint; policy treats it as general demand.

Classification source of truth:

- `spec.nodeSelector` and `spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution`
- key: `joulie.io/power-profile`
- if no power-profile constraint exists, classification is implicit unconstrained (general).

Reference example:

- [Workload Scheduling Classes](https://github.com/matbun/joulie/tree/main/examples/03-workload-intent-classes/README.md)

## Simple starter policy (recommended)

Bootstrap test policy:

1. Select two non-reserved nodes.
2. Assign `ActivePerformance` to node A and `ActiveEco` to node B (profile mapping `performance`/`eco`).
3. Every minute, swap assignments.
4. Observe frequency/power metrics and verify profile transitions.

This validates control-loop correctness before adding advanced policy logic.

## Future extension hooks

Policy modules should be able to consume:

- node metadata (geo/zone/rack, reserved flag)
- schedules/time windows
- telemetry (PUE, temperatures, hotspot signals)
- external inference outputs (for example KServe model predictions)

Recommended abstraction boundary for future-proofing:

- `ContextProvider`: provides cluster/node/workload/time/telemetry snapshot.
- `PolicyModule`: computes desired assignments/transitions from the context.
- `ActuationAdapter`: writes assignments (`NodePowerProfile` and node labels).

When data-driven policies are added, Prometheus should be integrated behind `ContextProvider` so policy APIs remain stable.

Input telemetry/control provider design (host vs simulated HTTP) is documented in:

- [Input Telemetry and Actuation Interfaces](./telemetry/)

## Example

```yaml
apiVersion: joulie.io/v1alpha1
kind: NodePowerProfile
metadata:
  name: node-worker-01
spec:
  nodeName: worker-01
  profile: eco
  cpu:
    packagePowerCapWatts: 180
  policy:
    name: rule-swap-v1
```
