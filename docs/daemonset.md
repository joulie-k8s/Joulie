# DaemonSet Configuration

The agent runs as a privileged DaemonSet and applies/simulates node-level settings.

## Manifest

Main manifest: `deploy/joulie.yaml`.

## Required runtime settings

- `securityContext.privileged: true`
- Host mount:
  - host path `/sys` -> container path `/host-sys`
- Env:
  - `NODE_NAME` from `spec.nodeName`
  - optional `RECONCILE_INTERVAL` (default `20s`)
  - optional `SIMULATE_ONLY=true` (skip host writes, log requested actions)
  - optional `METRICS_ADDR` (default `:8080`)

## Scheduling scope

Current manifest is broad by default (no nodeSelector). To scope it, add node selector/affinity if desired:

```yaml
nodeSelector:
  joulie.io/managed: "true"
```

## RBAC

Agent needs read-only access to:

- `nodes`
- `powerpolicies.joulie.io`
- `nodepowerprofiles.joulie.io`

No write permissions are required in current implementation.

## NFD labels used by agent

CPU vendor:

- `feature.node.kubernetes.io/cpu-vendor` (if present)
- `feature.node.kubernetes.io/cpu-model.vendor_id` (fallback, common with NFD)

GPU vendor discovery hints:

- `feature.node.kubernetes.io/pci-10de.present` (NVIDIA)
- `feature.node.kubernetes.io/pci-1002.present` (AMD)
- `feature.node.kubernetes.io/pci-8086.present` (Intel)
- and class-specific forms like `pci-0300_<vendor>.present` / `pci-0302_<vendor>.present`

GPU limits are not enforced yet; labels are currently used for capability detection and logs.

## Desired-state source order

On each node, agent resolves desired state in this order:

1. `NodePowerProfile` matching `spec.nodeName=<this-node>` (operator-driven path).
2. Otherwise: highest-priority matching `PowerPolicy` selector.
