---
title: "Joulie Agent"
weight: 30
---

The agent is Joulie's node-side component: it discovers the node's hardware and applies the caps and governor presets the controller manager plans. It does not plan policy.

## Responsibilities

Every reconcile (`RECONCILE_INTERVAL`, 20 s), for each node it owns, the agent:

| Step | What |
|---|---|
| 1 | discovers CPU and GPU hardware, cap ranges and governors |
| 2 | publishes them as `NodeHardware` |
| 3 | reads the desired state in `NodeTwin.spec` |
| 4 | applies the governor presets and the CPU and GPU caps, through host interfaces or HTTP |
| 5 | reports the outcome in `NodeTwin.status.controlStatus` and on `/metrics` |

## Inputs and outputs

| Inputs | Outputs |
|---|---|
| `NodeTwin.spec`: profile, caps, governor presets | `NodeHardware`: discovered hardware and capabilities |
| backend variables (`TELEMETRY_CPU_SOURCE`, `TELEMETRY_CPU_CONTROL`, `TELEMETRY_GPU_CONTROL`; default `host`) | writes to RAPL, cpufreq and GPU interfaces, or the simulator over HTTP |
| node labels (hardware identity, NFD and GPU feature discovery) | `NodeTwin.status.controlStatus`: `applied`, `blocked`, `error` or `none` per component |
| | Prometheus metrics on `/metrics` ([Metrics Reference]({{< relref "/docs/architecture/metrics.md" >}})) |

`NodeHardware` is the agent's output; you do not write it.

## Runtime modes

- `daemonset`: one agent pod per managed real node, for real hardware.
- `pool`: a few agent pods share many logical nodes (sharded), for KWOK and simulator runs.

Both select nodes with `joulie.io/managed=true`. Setup is in [Agent Runtime Modes]({{< relref "/docs/getting-started/02-agent-runtime-modes.md" >}}).

## Governor discovery

Every reconcile the agent reads the CPU and GPU frequency governors under the host `/sys` (mounted at `/host-sys`), and for NVIDIA through `nvidia-smi`, without writing anything. It publishes them in `NodeHardware.status.cpu.governor`, `status.cpu.landmarks` and `status.gpu.governor`: the driver, the current state, the presets the node accepts, whether it is writable, and the baseline a restore puts back. Discovery is always on.

```bash
kubectl get nodehardware <node> -o jsonpath='{.status.cpu.governor.presets}'
```

## Governor control

The agent writes governor presets only with `GOVERNOR_WRITES=true` ([Agent governor control]({{< relref "/docs/getting-started/05-configuration-reference.md#agent-governor-control" >}})). It maps each preset to the node's native settings, writes them, reads them back where it can, and reports the outcome in `status.controlStatus.cpuGovernor` and `gpuGovernor`, with the native setting in `applied`.

| Result | Meaning |
|---|---|
| `applied`, `equivalent` | in force; checked again every reconcile |
| `applied-unverified` | NVIDIA clock locks issued, not yet seen in use |
| `unsupported`, `not-applied` | the node cannot take the preset, or the write did not hold; retried after 30 min or when the intent changes |
| `refused` | permission denied; not retried until the agent restarts |
| `drift`, `contended` | another writer changed the setting; re-applied, then left alone after the third drift in 30 min |
| `disabled` | `GOVERNOR_WRITES=false` or `SIMULATE_ONLY` |
| `restored` | the preset left the spec and the baseline went back |
| `error` | the mechanism could not run (missing tool); retried after 5 min |

When a preset leaves the spec, or the agent stops with `GOVERNOR_RESTORE_ON_EXIT=true` (default), the agent puts back the baseline it recorded before its first write. What each preset becomes per driver is in [CPU support]({{< relref "/docs/hardware/cpus.md#governors" >}}) and [GPU support]({{< relref "/docs/hardware/gpus.md#governors" >}}).

## CPU enforcement algorithm

1. Take `packagePowerCapWatts`, or resolve `packagePowerCapPctOfMax` to watts against the node's RAPL range (for an HTTP backend, the hardware catalog's range for the node's CPU).
2. Write the watts to RAPL (host files, or `rapl.set_power_cap_watts` over HTTP).
3. Without RAPL, fall back to DVFS: a feedback loop on `scaling_max_freq` that holds the measured power under the cap. A percentage with no range to resolve it becomes a throttle percentage directly (`dvfs.set_throttle_pct` over HTTP).

The tuning variables are under [Agent DVFS control]({{< relref "/docs/getting-started/05-configuration-reference.md#agent-dvfs-control" >}}). The backend in use is `joulie_backend_mode{mode=none|rapl|dvfs}`, and the log line `resolved cpu cap from pct` names the range used.

## GPU power cap

The agent resolves `spec.gpu.powerCap` to watts per GPU and writes the same cap to every GPU of the node, skipping GPUs already at it, and reads the result back where the driver allows.

| Vendor | Mechanism | Needs |
|---|---|---|
| NVIDIA | `nvidia-smi -pl`, per GPU | `nvidia-smi` in the agent ([Agent GPU tools]({{< relref "/docs/getting-started/05-configuration-reference.md#agent-gpu-tools" >}})) |
| AMD | `power1_cap` of the amdgpu hwmon device, no tool | root agent, writable `/host-sys` (the chart's default) |
| Intel | `power1_max` or `power2_max` of the i915 or xe hwmon device, discrete GPUs only | as AMD |

A node whose GPUs have no settable limit reports `blocked`; a failed write is `error` naming the GPU, while the other GPUs are still capped. Check a node with:

```bash
kubectl get nodetwin <node> -o jsonpath='{.status.controlStatus.gpu}'
```

## Performance -> eco transition and safeguards

The controller manager decides when a node may drop to eco; the agent only enforces the published target. A node planned for eco that still runs performance pods stays `draining` with its performance caps until they finish, so they are never throttled mid-run, and nothing is evicted. See the [node state model]({{< relref "/docs/architecture/controller-manager.md" >}}) and [Policy Algorithms]({{< relref "/docs/architecture/policies.md" >}}).

## Next steps

1. [Policy Algorithms]({{< relref "/docs/architecture/policies.md" >}})
2. [Input Telemetry and Actuation Interfaces]({{< relref "/docs/architecture/telemetry.md" >}})

Discovery rules, every result and restore case, the DVFS loop, the per-vendor cap paths and the transition state machine: [Agent control](https://github.com/joulie-k8s/Joulie/wiki/Agent-Control) on the wiki.
