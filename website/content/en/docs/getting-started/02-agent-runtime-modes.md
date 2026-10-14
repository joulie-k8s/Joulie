+++
title = "Agent Runtime Modes"
linkTitle = "Agent Runtime Modes"
slug = "daemonset"
weight = 4
+++


The chart runs the agent in one of three modes, or not at all (`agent.mode`):

| `agent.mode` | Pods | Use |
|---|---|---|
| `daemonset` (default) | one pod per managed node (`charts/joulie/templates/agent-daemonset.yaml`) | real hardware |
| `pool` | a StatefulSet whose pods each run many logical per-node agents (`agent-statefulset.yaml`) | KWOK experiments over the simulator's HTTP API |
| `emulated` | one Deployment per node of the simulated cluster (`agent-emulated.yaml`), reading that node's emulated `/sys` and GPU tools | the [simulated cluster]({{< relref "/docs/simulator/simulated-cluster.md" >}}) |
| `none` | no agent | |

## DaemonSet mode (real hardware)

### Required runtime settings

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

Use `daemonset` mode for real `/host-sys` enforcement and `pool` mode for KWOK-scale simulation: pool mode runs KWOK experiments over the simulator's HTTP API ([KWOK and Standalone Modes]({{< relref "/docs/simulator/simulator.md" >}})), with a real controller manager and scheduler and no physical hardware.

## Emulated mode (simulated cluster)

With `agent.mode: emulated` the chart renders one Deployment `joulie-agent-emu-<i>` per entry of `agent.emulated.nodes`, on the node that runs the simulator in [emulate mode]({{< relref "/docs/simulator/emulate-mode.md" >}}). Each runs the unmodified agent binary for one node, with that node's emulated tree mounted where the agent reads `/sys`, `/proc/cpuinfo` and the GPU tools, a literal `NODE_NAME`, and no host network or privileges. The simulated cluster scripts write `agent.emulated.nodes` for you; the values are under [Agent chart values]({{< relref "/docs/getting-started/05-configuration-reference.md#agent-chart-values" >}}).

## Managed node selector

- `joulie.io/managed=true`

This label defines managed scope for both controller manager and agent by default.

- Pool mode:
  - uses `POOL_NODE_SELECTOR` (chart default: `joulie.io/managed=true`).
- DaemonSet mode:
  - chart default sets `agent.daemonset.nodeSelector.joulie.io/managed: "true"`.

Default behavior:

- nodes without `joulie.io/managed=true` are outside controller manager policy scope;
- no DaemonSet agent pod is scheduled on unlabeled nodes.

Override example (if you need a different scope):

```yaml
agent:
  daemonset:
    nodeSelector:
      custom.scope/energy-managed: "true"
```

## RBAC

Agent needs:

- `nodes`
- `nodetwins.joulie.io`

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

GPU labels are used for capability detection, and GPU cap intents are enforced when a supported backend is available.

## Desired-state source

On each node, agent resolves desired state only from:

1. `NodeTwin.spec` matching `spec.nodeName=<this-node>`.
