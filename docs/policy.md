# CRD and Policy Model

## CRD

The implemented API is:

- Group: `joulie.io`
- Version: `v1alpha1`
- Kind: `PowerPolicy`
- Resource: `powerpolicies`
- Scope: `Cluster`

CRD file: `config/crd/bases/joulie.io_powerpolicies.yaml`

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
