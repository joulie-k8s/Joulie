# DaemonSet Configuration

The enforcer runs as a privileged DaemonSet and writes host powercap files.

## Manifest

Main manifest: `deploy/joulie.yaml`.

## Required runtime settings

- `securityContext.privileged: true`
- Host mount:
  - host path `/sys` -> container path `/host-sys`
- Env:
  - `NODE_NAME` from `spec.nodeName`
  - optional `RECONCILE_INTERVAL` (default `20s`)

## Scheduling scope

Current manifest schedules on all nodes (`tolerations: operator: Exists`).
To scope to workers only, add a nodeSelector, for example:

```yaml
nodeSelector:
  node-role.kubernetes.io/worker: ""
```

## RBAC

Agent needs read-only access to:

- `nodes`
- `powerpolicies.joulie.io`

No write permissions are required in current implementation.

## NFD labels used by agent

CPU vendor:

- `feature.node.kubernetes.io/cpu-vendor`

GPU vendor discovery hints:

- `feature.node.kubernetes.io/pci-10de.present` (NVIDIA)
- `feature.node.kubernetes.io/pci-1002.present` (AMD)
- `feature.node.kubernetes.io/pci-8086.present` (Intel)
- and class-specific forms like `pci-0300_<vendor>.present` / `pci-0302_<vendor>.present`

GPU limits are not enforced yet; labels are currently used for capability detection and logs.
