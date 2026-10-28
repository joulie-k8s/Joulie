---
title: "CRD and Policy Model"
weight: 10
---

This page defines Joulie's core contract:

- **demand** comes from pod scheduling constraints,
- **supply** is exposed by node power-profile labels,
- **discovered hardware** is published through `NodeHardware`,
- **desired state** is published through `NodeTwin`.

## APIs

Group/version:

- `joulie.io/v1alpha1`

CRDs:

- `NodeHardware` (`nodehardwares`, cluster-scoped)
- `NodeTwin` (`nodetwins`, cluster-scoped)

CRD definitions live in:

- `config/crd/bases/joulie.io_nodehardwares.yaml`
- `config/crd/bases/joulie.io_nodetwins.yaml`

## Demand model (workloads)

Workload class is determined from the `joulie.io/workload-class` pod annotation:

- `performance` demand: pod carries `joulie.io/workload-class: performance`.
- `standard` demand (default): no annotation, or `joulie.io/workload-class: standard`. Can run on any node; adaptive scoring steers toward eco when performance nodes are congested.

## Supply model (nodes)

Node supply is represented by:

- `joulie.io/power-profile=performance|eco` (node label, set by the controller manager)
- `NodeTwin.status.schedulableClass` (internal, set by controller manager's twin controller)

Semantics:

- `performance`: full-performance supply
- `eco`: low-power supply
- `draining` (schedulableClass only): transition safeguard active while node is moving toward eco; the scheduler extender filters it out for performance pods

The `schedulableClass` field is internal to the controller manager and scheduler extender. Users interact only with the `joulie.io/workload-class` pod annotation for placement intent.

## Desired-state + twin output: `NodeTwin`

`NodeTwin` is the controller-manager-to-agent contract for one node. The `spec` carries desired state; the `status` carries twin output (including control feedback and schedulable class).

Main spec fields:

- `spec.nodeName` (required)
- `spec.profile` (required, `performance|eco`)
- `spec.cpu.packagePowerCapWatts` (optional, absolute package cap)
- `spec.cpu.packagePowerCapPctOfMax` (optional, normalized policy intent)
- `spec.cpu.governor` (optional, CPU governor preset; see [Governor policy](#governor-policy)):
  - `preset` (`performance|balanced|efficiency`)
  - `boost` (optional, `on|off`)
- `spec.gpu.powerCap` (optional):
  - `scope` (`perGpu`)
  - `capWattsPerGpu` (absolute per-GPU cap)
  - `capPctOfMax` (percentage of node GPU max power)
- `spec.gpu.governor` (optional, GPU governor preset):
  - `preset` (`auto|sustained|efficient|low|peak`)
  - `computeClockCeilingPctOfMax` (only with `efficient`)
- `spec.policy.name` (optional, provenance/debug)

Resolution/precedence in agent runtime:

1. CPU: `packagePowerCapWatts` if present, otherwise `packagePowerCapPctOfMax`
2. GPU: `capWattsPerGpu` if present, otherwise `capPctOfMax`

What the controller manager typically writes today:

- CPU intent is commonly emitted as `packagePowerCapPctOfMax` on CPU-only nodes.
- A GPU node's CPU gets `packagePowerCapWatts` at `PERFORMANCE_CAP_WATTS` whatever the node's profile: capping the CPU of a GPU node slows the GPU data feed by more than it saves, so a GPU node gets only GPU caps.
- With `CPU_ECO_CAP_ON_UNCAPPABLE_GPU_NODES=true`, a GPU node whose `NodeHardware` reports GPUs without control (`status.gpu.controlAvailable` false: Intel GPUs, NVIDIA boards whose power limits read `[N/A]`) gets the CPU caps of its profile instead, as a CPU-only node does, so it does not run uncapped in eco. A node with no `NodeHardware` keeps the GPU node rule.
- GPU intent may be emitted as:
  - `capPctOfMax` only, when the agent is expected to resolve percentage to watts from device limits,
  - `capWattsPerGpu` plus `capPctOfMax`, when the controller manager has deterministic model-based mapping available and wants the absolute target to be explicit.

Example:

```yaml
apiVersion: joulie.io/v1alpha1
kind: NodeTwin
metadata:
  name: kwok-gpu-nvidia-0
spec:
  nodeName: kwok-gpu-nvidia-0
  profile: eco
  cpu:
    packagePowerCapWatts: 5000
  gpu:
    powerCap:
      scope: perGpu
      capPctOfMax: 60
      capWattsPerGpu: 240
  policy:
    name: static-partition-v1
```

In this example:

- The CPU of this GPU node is left at the performance cap; a CPU-only eco node would get `packagePowerCapPctOfMax: 60` instead.
- GPU is expressed as both percentage and resolved absolute watts per GPU.
- The agent applies the absolute GPU cap when present and falls back to percentage only when absolute watts are not provided.

Besides the twin's numbers, the controller manager writes `status.conditions`: the condition `AgentReporting` is `True` (reason `NodeHardwareCurrent`) while an agent publishes the node's `NodeHardware`, and `False` when none has (`NoNodeHardware`) or its last write is more than 15 minutes old (`NodeHardwareStale`). The policy plans a managed node either way; `False` says the caps in `spec` are not enforced. `kubectl get nodetwins` shows it in the `Agent` column.

## Governor policy

A governor decides how eagerly a CPU or GPU raises its clocks. It saves less than a cap and slows work less. Joulie sets one vendor-neutral preset per component, which the agent maps to the node's native settings. The preset follows the profile the node runs at, so a draining node keeps the performance presets, as it keeps the performance caps. Pod classes (`joulie.io/workload-class`) only steer placement.

Off by default: the controller manager writes presets only with `GOVERNOR_CONTROL` set to `cpu`, `gpu` or `all`, and the agent applies them only with `GOVERNOR_WRITES=true`. The variables are under [Governor control]({{< relref "/docs/getting-started/05-configuration-reference.md#governor-control" >}}), what each preset means under [Governor presets]({{< relref "/docs/hardware/hardware-modeling.md#69-governor-presets" >}}).

| Node state | CPU-only node: `spec.cpu.governor` | GPU node: `spec.cpu.governor` | `spec.gpu.governor` | Why |
|---|---|---|---|---|
| ActivePerformance | `CPU_GOVERNOR_PERFORMANCE_PRESET` (`performance`), boost left alone | not written (`CPU_GOVERNOR_ON_GPU_NODES=false`) | the model's entry in `GPU_GOVERNOR_MODEL_PRESETS_JSON`, else `GPU_GOVERNOR_PERFORMANCE_PRESET` (empty: not written) | race to idle; a data path is held only on models the operator opted in |
| DrainingPerformance | as ActivePerformance | not written | as ActivePerformance | performance pods still run |
| ActiveEco | `CPU_GOVERNOR_ECO_PRESET` (`efficiency`), with `CPU_GOVERNOR_ECO_BOOST` when set | not written | the model's entry, else `GPU_GOVERNOR_ECO_PRESET` (`efficient`, ceiling `GPU_GOVERNOR_ECO_CEILING_PCT_OF_MAX`, 80) | a compute-bound job's energy optimum sits below the maximum clock; a memory-bound one loses almost nothing |
| preset not in the node's `NodeHardware` presets | not written, one warning | | not written, one warning (no fallback to `auto`, which would override an operator's own setting) | never ask a node for what it cannot do |
| `GOVERNOR_CONTROL=off` | nothing written | nothing | nothing | the default |

- `GOVERNOR_CONTROL=cpu` writes only the CPU presets, `gpu` only the GPU ones. Any value other than `off`, `cpu`, `gpu` or `all` logs a warning and means `off`.
- A node gets only a preset its agent lists in `NodeHardware` `status.cpu.governor.presets` or `status.gpu.governor.presets`. Nothing replaces a preset the node does not list, so a setting the node's operator chose stays. A node whose agent finds no governor lists no preset and gets none.
- A `gpu.governor` block that holds a `baseline` and no `mechanism` is a discovery gap: the agent's GPU discovery did not answer and it had no earlier block to keep. The block lists no presets, and the configured GPU preset is still written, since dropping it would make the agent restore the baseline and set the preset again a reconcile later.
- `GPU_GOVERNOR_MODEL_PRESETS_JSON` sets the GPU presets and the ceiling per GPU model. Its keys are matched against the node's GPU product label value (the first of `GPU_PRODUCT_LABEL_KEYS` the node carries), as in `GPU_MODEL_CAPS_JSON`, and against the model the agent reports. It is how a model is opted in to `sustained`, which no default selects.
- An empty GPU preset, in a variable or a model entry, writes none. A preset or ceiling the controller manager cannot use logs a warning and keeps the default.
- With `GPU_GOVERNOR_SUSTAINED_ONLY_MULTI_GPU=true`, a node whose GPU preset is `sustained` gets it only while a running pod on it requests 2 or more GPUs, and no GPU governor otherwise, so its agent restores the baseline: held data paths cost idle power and pay off in transfers between GPUs. A pod's GPUs are the units of every GPU resource (`pkg/api.GPUResourceKind`: whole GPUs, MIG devices, partitions, shared replicas), from its requests, else its limits; pending, finished and terminating pods do not count. The rule is checked at every reconcile, so the preset follows such a pod within one `RECONCILE_INTERVAL`.
- When a field leaves the spec, because control was turned off or the node stopped listing the preset, the agent puts back the state it recorded before its first write.

Both eco defaults are provisional: the [governor exploration]({{< relref "/docs/experiments/governor-exploration.md" >}}) has run in the simulator only: its sweep on emulated nodes (Tier A), the sensitivity of its conclusions to the assumed parameters (Q5) and the functional checks on the simulated cluster (Tier B); its rule also needs energy measured on the simulated cluster and the assumed parameters measured on hardware. In that sweep, judged per profile, GPU `efficient` at 80 % lowers energy by 11 to 17 % with a p95 slowdown of 17.1 to 17.6 % on every GPU model that can express it, within the eco limit of 20 % at every value of the assumed parameters the sweep varies; the models agree because they share the assumed clock parameters. CPU `efficiency` passes Tier A on 2 of the 5 CPU drivers, with a worst cell over 20 % on both, and Q5 on 1, so as a default for every driver it does not pass; at the frequency ramp the profiles assume, it also lengthens a compute-bound burst by 71 to 127 %. Both are decided again on measured data in [#116](https://github.com/joulie-k8s/Joulie/issues/116).

### Governor fields

Only `pkg/governor` turns a preset into a native setting; every other component speaks presets, listed in `pkg/api/governor.go`.

| Field | Type | Owner | Description |
|-------|------|-------|-------------|
| `spec.cpu.governor.preset` | `performance`, `balanced`, `efficiency` | Controller manager | the CPU governor of every cpufreq policy: highest frequency at any load, demand-driven with a quick ramp, or demand-driven with an energy bias |
| `spec.cpu.governor.boost` | `on`, `off`, optional | Controller manager | frequency boost (turbo); absent leaves it as it is |
| `spec.gpu.governor.preset` | `auto`, `sustained`, `efficient`, `low`, `peak` | Controller manager | the GPU clock governor of every GPU: vendor default, data-path clocks held at their maximum with idle gating kept, compute clock under a ceiling, every clock at its minimum, every clock at its maximum with idle gating off |
| `spec.gpu.governor.computeClockCeilingPctOfMax` | number in (0, 100] | Controller manager | the compute clock ceiling of `efficient`, in percent of the maximum compute clock |
| `status.controlStatus.cpuGovernor`, `status.controlStatus.gpuGovernor` | `ControlResult` | Agent | outcome of the last governor write: `backend` (`cpufreq`, `amdgpu-sysfs`, `nvidia-smi`, or `none` or `http` when nothing is written), `result`, `message`, and `applied`, the native setting in effect |
| `NodeHardware.status.cpu.governor` | object | Agent | `driver`, `driverMode`, `current`, `available`, `epp`, `boost` (`state` `on` or `off`; `scope` `global` for `cpufreq/boost`, `per-cpu` for each policy's `boost`, `no-turbo` for the inverted `intel_pstate/no_turbo`), `presets` (each with the result it gives, `applied` or `equivalent`), `baseline`, `writable` |
| `NodeHardware.status.gpu.governor` | object | Agent | `mechanism`, `current`, `presets`, `clockRangesMHz` (`graphics`, `memory`, `fabric`), `baseline` (per GPU, keyed by PCI bus id), `writable`, `uniform` |

A preset a node cannot express is left out of its `presets`. Beside the governor block the agent publishes the cpufreq frequency landmarks in `NodeHardware.status.cpu.landmarks` (`minFreqMHz`, `nominalFreqMHz`, `maxBoostMHz`).

`kubectl get nodetwins` shows the presets in the columns `CPU-Gov` and `GPU-Gov`; an empty column means no preset, so the agent changes nothing there, or puts back its baseline if it changed something.

The result values are `applied`, `applied-unverified`, `equivalent`, `unsupported`, `not-applied`, `refused`, `drift`, `contended`, `disabled`, `restored` and `error` ([Governor control]({{< relref "/docs/architecture/agent.md#governor-control" >}}) says when each occurs). `ControlResult` is shared with the cap results, so the CRD does not enumerate them.

The twin and the scheduler extender read no governor field: a governor moves the measured power, which the twin reads.

## Hardware-discovery object: `NodeHardware`

`NodeHardware` is the agent-to-controller-manager contract for discovered node capabilities.

It is status-oriented and agent-owned.
Users should not normally create it by hand.

Main fields:

- `spec.nodeName`
- `status.cpu`:
  - raw model string
  - normalized inventory model when recognized
  - sockets / total cores / cores per socket
  - runtime CPU cap range when available
  - frequency landmarks from cpufreq (`landmarks`: minimum, nominal, maximum boost)
  - the cpufreq governor state, the presets the node accepts and the baseline the agent restores (`governor`)
  - control and telemetry availability
- `status.gpu`:
  - raw model string
  - normalized inventory model when recognized
  - GPU count
  - runtime GPU cap range when available
  - the GPU clock governor state, clock ranges, the presets the GPUs accept and the baseline per GPU (`governor`)
  - control and telemetry availability
- `status.quality`:
  - overall discovery quality
  - warnings

`status.quality.overall` is:

- `exact`: the CPU model, and the GPU model when the node has GPUs, are recognized against the inventory
- `heuristic`: a raw CPU or GPU model is present, but not recognized
- `unavailable`: no CPU or GPU model was found

Example, for an 8x H100 NVL node on a 2x Xeon Platinum 8260 host, with the governor blocks abridged. The object is named after the node:

```yaml
apiVersion: joulie.io/v1alpha1
kind: NodeHardware
metadata:
  name: kwok-gpu-nvidia-0
spec:
  nodeName: kwok-gpu-nvidia-0
status:
  cpu:
    rawModel: Intel(R) Xeon(R) Platinum 8260 CPU @ 2.40GHz
    model: INTEL_XEON_PLATINUM_8260
    vendor: GenuineIntel
    sockets: 2
    totalCores: 96
    coresPerSocket: 48
    driverFamily: intel_pstate
    controlAvailable: true
    telemetryAvailable: true
    governor:
      driver: intel_pstate
      current: powersave
      presets:
      - {preset: performance, native: performance, result: applied}
      - {preset: balanced, native: powersave epp=balance_performance, result: applied}
      - {preset: efficiency, native: powersave epp=balance_power, result: applied}
      writable: true
  gpu:
    rawModel: NVIDIA H100 NVL
    model: NVIDIA_H100_NVL
    vendor: nvidia
    count: 8
    capRangePerGpu:
      minWatts: 200
      maxWatts: 400
    controlAvailable: true
    telemetryAvailable: true
    governor:
      mechanism: nvidia-smi
      presets:
      - {preset: auto, native: unknown, result: equivalent}
      - {preset: efficient, native: "-lgc 345,1425", result: applied}
      writable: true
  capabilities:
    cpuControl: true
    gpuControl: true
    cpuTelemetry: true
    gpuTelemetry: true
  quality:
    overall: exact
```

The controller manager uses `NodeHardware` as the source of truth for:

- hardware recognition against the inventory,
- the hardware family of each node, used by the per-family performance split ([Policy Algorithms]({{< relref "/docs/architecture/policies.md" >}})),
- per-device fallback when only part of the node is recognized.

In simulator-first setups, the controller manager can fall back to node hardware labels when `NodeHardware` has not been published yet.
This keeps simulator examples lightweight while preserving the same architecture once the agent starts publishing discovered hardware.

## Telemetry/control backend selection

Telemetry and control backend selection is configured via environment variables on the agent, not through a CRD.

At the policy-model level, the important distinction is:

- `NodeTwin.spec` says what state a node should reach
- `NodeHardware` says what the node is and what it can do
- Agent environment variables say how the agent should observe and actuate that node

Key environment variables:

- `TELEMETRY_CPU_SOURCE`: telemetry backend for CPU (`host`, `http`)
- `TELEMETRY_CPU_CONTROL`: control backend for CPU (`host`, `http`)
- `TELEMETRY_GPU_SOURCE`: telemetry backend for GPU (`host`, `http`)
- `TELEMETRY_GPU_CONTROL`: control backend for GPU (`host`, `http`)

Default is `host` backends (real node interfaces). For simulator/KWOK setups, set these to `http` with the appropriate endpoint environment variables pointing at the simulator service (e.g., `http://joulie-telemetry-sim.joulie-sim-demo.svc.cluster.local`).

Control feedback is written to `NodeTwin.status.controlStatus` by the agent.

The full runtime contract, backend types, HTTP payloads, and status semantics are documented in [Input Telemetry and Actuation Interfaces]({{< relref "/docs/architecture/telemetry.md" >}}).

## End-to-end contract flow

1. User submits workload with Kubernetes scheduling constraints.
2. Scheduler places pods according to available node labels.
3. The controller manager observes demand/supply and computes new node targets.
4. Agent publishes `NodeHardware`.
5. The controller manager resolves discovered hardware against the inventory.
6. The controller manager writes `NodeTwin` and updates node supply labels.
7. Agent enforces controls and reports status/metrics.

## Next step

Read [Joulie Controller Manager]({{< relref "/docs/architecture/controller-manager.md" >}}) for reconcile behavior and transition guards.
