+++
title = "Agent Runtime Modes"
linkTitle = "Agent Runtime Modes"
slug = "daemonset"
weight = 2
+++


The agent supports two runtime modes:

- `daemonset`: real-hardware mode, one pod per real node.
- `pool`: simulation mode, one pod hosts many logical per-node controllers.

Chart templates:

- `charts/joulie/templates/agent-daemonset.yaml`
- `charts/joulie/templates/agent-statefulset.yaml`

## DaemonSet mode (real hardware)

## Required runtime settings

- `securityContext.privileged: true`
- Host mount:
  - host path `/sys` -> container path `/host-sys`
- Env:
  - `NODE_NAME` from `spec.nodeName`
  - `AGENT_MODE=daemonset` (default)
  - optional `RECONCILE_INTERVAL` (default `20s`)
  - optional `SIMULATE_ONLY=true` (skip host writes, log requested actions)
  - optional `METRICS_ADDR` (default `:8080`)

## Pool mode (KWOK / simulation)

Pool mode preserves per-node semantics but shards logical node controllers across replicas.

Required env vars:

- `AGENT_MODE=pool`
- `POOL_NODE_SELECTOR` (for example `joulie.io/managed=true`)
- `POOL_SHARDS` (total shards)
- `POOL_SHARD_ID` (or `POD_NAME` from StatefulSet ordinal)

Sharding function:

- `owns(node) = fnv32(nodeName) % POOL_SHARDS == POOL_SHARD_ID`

In chart values:

```yaml
agent:
  mode: pool
  pool:
    replicas: 2
    shards: 2
    nodeSelector: "joulie.io/managed=true"
```

Use `daemonset` mode for real `/host-sys` enforcement and `pool` mode for KWOK-scale simulation.

## Scheduling scope (daemonset)

Current manifest is broad by default (no nodeSelector). To scope it, add node selector/affinity if desired:

```yaml
nodeSelector:
  joulie.io/managed: "true"
```

## RBAC

Agent needs:

- `nodes`
- `nodepowerprofiles.joulie.io`
- `telemetryprofiles.joulie.io` (read + status update in current implementation)

Pool mode uses the same API permissions.

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

## Desired-state source

On each node, agent resolves desired state only from:

1. `NodePowerProfile` matching `spec.nodeName=<this-node>`.
