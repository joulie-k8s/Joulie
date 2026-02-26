# CRD and Policy Model

## CRD

The implemented APIs are:

- Group: `joulie.io`
- Version: `v1alpha1`
- `PowerPolicy` (`powerpolicies`, cluster-scoped) for selector-based intent
- `NodePowerProfile` (`nodepowerprofiles`, cluster-scoped) for operator-assigned per-node desired state

CRD files:

- `config/crd/bases/joulie.io_powerpolicies.yaml`
- `config/crd/bases/joulie.io_nodepowerprofiles.yaml`

## Conceptual model (next step)

Policy should be modeled as a cluster-wide mapping:

- input: cluster context at time `t`
- output: `node -> power profile`

Minimal profiles:

- `performance` (HPC, unconstrained)
- `eco` (energy-aware, throttling allowed)

Initial implementation should remain rule-based and deterministic.
Future implementations can be telemetry-driven or model-driven.

## Spec fields

- `spec.priority` (int, default `0`)
- `spec.selector.matchLabels` (required)
- `spec.cpu.packagePowerCapWatts` (optional, number)
- `spec.gpu.enabled` (optional, bool; reserved)
- `spec.gpu.powerLimitWatts` (optional, number; reserved)

## Selection behavior

On each node, agent:

1. Lists all `PowerPolicy` objects.
2. Matches policy selector against node labels.
3. Picks highest `spec.priority`.
4. Uses name as tiebreaker (lexicographically).
5. Applies CPU cap if `spec.cpu.packagePowerCapWatts` is set.

This is the current agent-driven behavior. The target architecture is operator-driven assignment, where agents consume only their node-specific desired profile/state.

## Simple starter policy (recommended)

Bootstrap test policy:

1. Select two non-reserved nodes.
2. Assign `performance` to node A and `eco` to node B.
3. Every minute, swap assignments.
4. Observe frequency/power metrics and verify profile transitions.

This validates control-loop correctness before adding advanced policy logic.

## Future extension hooks

Policy modules should be able to consume:

- node metadata (geo/zone/rack, reserved flag)
- schedules/time windows
- telemetry (PUE, temperatures, hotspot signals)
- external inference outputs (for example KServe model predictions)

## Example

```yaml
apiVersion: joulie.io/v1alpha1
kind: PowerPolicy
metadata:
  name: amd-worker-balanced
spec:
  priority: 100
  selector:
    matchLabels:
      feature.node.kubernetes.io/cpu-vendor: AuthenticAMD
      node-role.kubernetes.io/worker: ""
  cpu:
    packagePowerCapWatts: 180
```
