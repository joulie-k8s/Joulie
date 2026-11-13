+++
title = "Agent Runtime Modes"
linkTitle = "Agent Runtime Modes"
slug = "daemonset"
weight = 4
+++

How the chart deploys the node agent. Pick the mode with `agent.mode`:

| `agent.mode` | Pods | Use |
|---|---|---|
| `daemonset` (default) | one pod per managed node (`charts/joulie/templates/agent-daemonset.yaml`) | real hardware |
| `pool` | a StatefulSet whose pods each run many logical per-node agents (`agent-statefulset.yaml`) | KWOK experiments over the simulator's HTTP API |
| `emulated` | one Deployment per node of the simulated cluster (`agent-emulated.yaml`), reading that node's emulated `/sys` and GPU tools | the [simulated cluster]({{< relref "/docs/simulator/simulated-cluster.md" >}}) |
| `none` | no agent | |

## DaemonSet mode (real hardware)

Use it on real nodes, where the agent writes the power limits under the host's `/sys`. The chart sets:

| Setting | Value |
|---|---|
| `securityContext.privileged` | `true` |
| Host mount | host `/sys` at container `/host-sys` |
| `NODE_NAME` | from `spec.nodeName` |
| `AGENT_MODE` | `daemonset` (default) |
| `RECONCILE_INTERVAL` | optional, default `20s` |
| `SIMULATE_ONLY` | optional, `true` skips host writes and logs the requested actions |
| `METRICS_ADDR` | optional, default `:8080` |

## Pool mode (KWOK / simulation)

Use it for KWOK-scale simulation: a real controller manager and scheduler, no physical hardware, and agents that control nodes over the simulator's HTTP API ([KWOK and Standalone Modes]({{< relref "/docs/simulator/simulator.md" >}})).

| Env var | Value |
|---|---|
| `AGENT_MODE` | `pool` |
| `POOL_NODE_SELECTOR` | for example `joulie.io/managed=true` |
| `POOL_SHARDS` | total shards |
| `POOL_SHARD_ID` | this pod's shard, or `POD_NAME` from the StatefulSet ordinal |

A pod owns a node when `fnv32(nodeName) % POOL_SHARDS == POOL_SHARD_ID`. In chart values:

```yaml
agent:
  mode: pool
  pool:
    replicas: 2
    shards: 2
    nodeSelector: "joulie.io/managed=true"
```

## Emulated mode (simulated cluster)

Use it for the simulated cluster: one Deployment `joulie-agent-emu-<i>` per entry of `agent.emulated.nodes`, each running the unmodified agent against one node's emulated tree on the host that runs the simulator in [emulate mode]({{< relref "/docs/simulator/emulate-mode.md" >}}). The simulated cluster scripts write `agent.emulated.nodes` for you; the values are under [Agent chart values]({{< relref "/docs/getting-started/05-configuration-reference.md#agent-chart-values" >}}).

## Managed node selector

- The controller manager and the agent manage only nodes labelled `joulie.io/managed=true` by default (`agent.daemonset.nodeSelector`, `POOL_NODE_SELECTOR`); unlabelled nodes get no agent pod.
- The agent tolerates every taint (`agent.tolerations`) and runs on `amd64` and `arm64`, so every labelled node gets one.
- A managed node the agent still does not reach reports `AgentReporting` `False` on its `NodeTwin` ([Managed nodes without an agent]({{< relref "/docs/getting-started/05-configuration-reference.md#managed-nodes-without-an-agent" >}})).

To use another scope:

```yaml
agent:
  daemonset:
    nodeSelector:
      custom.scope/energy-managed: "true"
```

## RBAC

The agent needs `nodes`, `nodetwins.joulie.io` and `nodehardwares.joulie.io`, with the status subresources of both CRDs (`charts/joulie/templates/rbac.yaml`). Pool mode uses the same permissions.

## NFD labels used by agent

| Label | Tells the agent |
|---|---|
| `feature.node.kubernetes.io/cpu-vendor` | CPU vendor, if present |
| `feature.node.kubernetes.io/cpu-model.vendor_id` | CPU vendor, fallback (common with NFD) |
| `feature.node.kubernetes.io/pci-10de.present` | NVIDIA GPU |
| `feature.node.kubernetes.io/pci-1002.present` | AMD GPU |
| `feature.node.kubernetes.io/pci-8086.present` | Intel GPU |
| `feature.node.kubernetes.io/pci-<class>_<vendor>.present` | the same, in NFD's default class-specific form, for `0300` (VGA), `0302` (3D controller), `0380` (other display controller) and `1200` (processing accelerator, such as an AMD Instinct MI300X) |

GPU labels are used for capability detection; GPU caps are enforced when a supported backend is available.

## Desired-state source

On each node, the agent reads its desired state only from the `NodeTwin` whose `spec.nodeName` is that node.

Details: [Configuration notes, Agent runtime modes](https://github.com/joulie-k8s/Joulie/wiki/Configuration-Notes#agent-runtime-modes).
