---
title: "CRD and Policy Model"
weight: 10
---

The contract between Joulie's components: pods state demand, node labels expose supply, `NodeHardware` publishes what a node is, and `NodeTwin` carries what it should be. Use this page to read or debug these objects.

## APIs

| | |
|---|---|
| Group and version | `joulie.io/v1alpha1` |
| CRDs | `NodeHardware` (`nodehardwares`), `NodeTwin` (`nodetwins`), both cluster-scoped |
| Definitions | `config/crd/bases/joulie.io_nodehardwares.yaml`, `config/crd/bases/joulie.io_nodetwins.yaml` |

## Demand model (workloads)

Demand comes from the `joulie.io/workload-class` pod annotation:

- `performance`: the pod carries `joulie.io/workload-class: performance` and is kept off eco and draining nodes.
- `standard` (default): no annotation, or `standard`. It can run on any node; scoring steers it toward eco when performance nodes are congested.

## Supply model (nodes)

| Signal | Values | Set by |
|---|---|---|
| node label `joulie.io/power-profile` | `performance`, `eco` | controller manager |
| `NodeTwin.status.schedulableClass` | `performance`, `eco`, `draining` | controller manager (twin) |

`draining` is the transition safeguard while a node moves toward eco with performance pods still on it; the scheduler extender filters it out for performance pods. `schedulableClass` is internal: users express placement intent only with the pod annotation.

## Desired-state + twin output: `NodeTwin`

`spec` carries the desired state (written by the controller manager), `status` the twin output and control feedback.

| Field | Writer | Meaning |
|---|---|---|
| `spec.nodeName`, `spec.profile` | Controller manager | the node; `performance` or `eco` (both required) |
| `spec.cpu.packagePowerCapWatts` | Controller manager | absolute CPU package cap; wins over the percentage |
| `spec.cpu.packagePowerCapPctOfMax` | Controller manager | CPU cap as a percentage of the cap range's maximum |
| `spec.cpu.governor` | Controller manager | CPU governor preset and boost ([Governor policy](#governor-policy)) |
| `spec.gpu.powerCap` | Controller manager | `scope` (`perGpu`), `capWattsPerGpu` (wins), `capPctOfMax` |
| `spec.gpu.governor` | Controller manager | GPU governor preset and ceiling |
| `spec.policy.name` | Controller manager | provenance and debugging |
| `status.controlStatus.*` | Agent | what was applied, per component |
| `status.conditions` | Controller manager | `AgentReporting`: `True` while an agent publishes the node's `NodeHardware`; `False` (`NoNodeHardware`, `NodeHardwareStale` after 15 minutes) means the caps are not enforced. `kubectl get nodetwins` shows it as `Agent` |
| every other `status` field | Controller manager | the twin's output ([Digital Twin]({{< relref "/docs/architecture/digital-twin.md" >}})) |

A GPU node's CPU always gets `PERFORMANCE_CAP_WATTS`, whatever its profile, unless `CPU_ECO_CAP_ON_UNCAPPABLE_GPU_NODES=true` and its GPUs cannot be capped. A GPU spec may carry both `capPctOfMax` and the resolved `capWattsPerGpu`; the agent applies the watts.

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

### Why a node got its state

Five conditions in `status.conditions` say why the node got its profile, caps and governors. Each is `True` while its decision is in force, its `reason` names the branch that decided it, and `lastTransitionTime` moves only when the status or the reason changes. `kubectl joulie node <node>` shows them in its `Why` panel.

`EcoProfile`: `True` while the node runs eco with eco caps.

| Status | Reason | Why |
|---|---|---|
| `False` | `PerformanceSlot` | The policy keeps the node in one of the performance slots of its hardware family (`static_partition`, and `queue_aware_v1` at its base fraction, `QUEUE_HP_MIN` or the one slot of each hardware family) |
| `False` | `PerformanceDemand` | `queue_aware_v1` keeps more nodes in performance than its base fraction, `QUEUE_HP_MIN` and the one slot of each hardware family would, for the performance pods that run or wait, and this node is one of them |
| `True` | `NoPerformanceSlot` | Other nodes of its hardware family hold the performance slots of `static_partition` |
| `True` | `NoPerformanceDemand` | No performance pod runs or waits, so `queue_aware_v1` keeps only its base fraction in performance |
| `True` | `PerformanceDemandMet` | The performance pods that run or wait fit on the nodes `queue_aware_v1` keeps in performance |
| `True` | `PerformanceNodesAtMax` | The performance pods need more performance nodes than `QUEUE_HP_MAX` allows |
| either | `RuleSwapPhase` | `rule_swap_v1` alternates eco between nodes on a timer, and the current phase sets this node's profile |
| `False` | `DrainingPerformancePods` | The policy planned eco, but performance pods still run on the node, so it keeps the performance caps and governors until they finish |

`CPUEcoCap`: `True` while the node's CPU takes the eco cap (`CPU_ECO_CAP_PCT_OF_MAX`, or `ECO_CAP_WATTS` with absolute caps).

| Status | Reason | Why |
|---|---|---|
| `True` | `EcoProfile` | The node runs eco |
| `True` | `CPUEcoCapOnUncappableGPUNode` | The node's GPUs cannot be capped and `CPU_ECO_CAP_ON_UNCAPPABLE_GPU_NODES` is on |
| `False` | `PerformanceProfile` | The node runs performance |
| `False` | `DrainingPerformancePods` | Performance pods still run on the node |
| `False` | `GPUNodeCPUUncapped` | A GPU node's CPU keeps the performance cap in eco, since a CPU cap slows the GPU data feed by more than it saves |

`GPUEcoCap`: `True` while the node's GPUs take an eco cap that applies, as the twin's GPU budget counts it.

| Status | Reason | Why |
|---|---|---|
| `True` | `EcoProfile` | The node runs eco |
| `False` | `PerformanceProfile` | The node runs performance |
| `False` | `DrainingPerformancePods` | Performance pods still run on the node |
| `False` | `NoGPU` | The node has no GPU |
| `False` | `GPUCapNotConfigured` | `GPU_ECO_CAP_PCT_OF_MAX` is 0 or below, so no GPU cap is written |
| `False` | `GPUCapBlocked` | The agent's last GPU cap result, `status.controlStatus.gpu.result`, is `blocked`: the node offers no way to apply the cap |
| `False` | `GPUCapFailed` | That result is `error`: the agent's last GPU cap write failed |
| `False` | `GPUCapNotApplicable` | `NodeHardware` reports the GPUs without power control (`status.gpu.controlAvailable` false) and no cap was `applied` |

`CPUGovernorPreset` and `GPUGovernorPreset`: `True` while `spec.cpu.governor` or `spec.gpu.governor` holds a preset; the message names it. [Governor policy](#governor-policy) below has the rules.

| Status | Reason | Why |
|---|---|---|
| `True` | `PerformanceProfile` | The node runs performance, and the performance preset is written |
| `True` | `EcoProfile` | The node runs eco, and the eco preset is written |
| `True` | `DrainingPerformancePods` | Performance pods still run on the node, and it keeps the performance preset |
| `True` | `ModelPreset` | GPU only: `GPU_GOVERNOR_MODEL_PRESETS_JSON` sets the preset for the node's GPU model |
| `True` | `MultiGPUPodRunning` | GPU only: the `sustained` preset is written while a pod using several GPUs runs (`GPU_GOVERNOR_SUSTAINED_ONLY_MULTI_GPU`) |
| `True` | `GovernorDiscoveryGap` | GPU only: the agent's last GPU governor discovery listed no presets, so the configured preset is written and the agent holds it until it can plan |
| `False` | `GovernorControlOff` | `GOVERNOR_CONTROL` leaves this component's governor alone |
| `False` | `GPUNodeCPUGovernorUnset` | CPU only: a GPU node's CPU governor is left alone unless `CPU_GOVERNOR_ON_GPU_NODES` is on |
| `False` | `NoGPU` | GPU only: the node has no GPU |
| `False` | `NoPresetConfigured` | No preset is configured for the profile the node runs at (a GPU preset variable or model entry set to empty) |
| `False` | `PresetNotListed` | The node does not list the configured preset in `NodeHardware` `status.{cpu,gpu}.governor.presets`, so none is written |
| `False` | `NoMultiGPUPod` | GPU only: the `sustained` preset is written only while a pod using several GPUs runs, and none runs |

## Governor policy

A governor decides how eagerly a CPU or GPU raises its clocks: it saves less than a cap and slows work less. Joulie writes one vendor-neutral preset per component, chosen by the node's profile (a draining node keeps the performance presets); the agent maps it to native settings.

Off by default. Turn it on in both places:

| Component | Setting | Effect |
|---|---|---|
| Controller manager | `GOVERNOR_CONTROL=cpu`, `gpu` or `all` | writes `spec.cpu.governor`, `spec.gpu.governor` |
| Agent | `GOVERNOR_WRITES=true` | applies them and reports `status.controlStatus.cpuGovernor`, `gpuGovernor` |

| Field | Values |
|---|---|
| `spec.cpu.governor.preset` | `performance`, `balanced`, `efficiency` |
| `spec.cpu.governor.boost` | `on`, `off`, absent to leave it |
| `spec.gpu.governor.preset` | `auto`, `sustained`, `efficient`, `low`, `peak` |
| `spec.gpu.governor.computeClockCeilingPctOfMax` | (0, 100], only with `efficient` |

| Node state | CPU-only node | GPU node CPU | GPU |
|---|---|---|---|
| performance or draining | `CPU_GOVERNOR_PERFORMANCE_PRESET` (`performance`) | not written | `GPU_GOVERNOR_PERFORMANCE_PRESET` (empty: none) |
| eco | `CPU_GOVERNOR_ECO_PRESET` (`efficiency`) | not written | `GPU_GOVERNOR_ECO_PRESET` (`efficient`, ceiling 80 %) |

A node gets only presets its agent lists in `NodeHardware`. Both eco defaults are provisional. The variables are under [Governor control]({{< relref "/docs/getting-started/05-configuration-reference.md#governor-control" >}}), the presets under [Governor presets]({{< relref "/docs/hardware/hardware-modeling.md#69-governor-presets" >}}), the results under [Governor control]({{< relref "/docs/architecture/agent.md#governor-control" >}}). The full rules (per-model presets, multi-GPU `sustained`, restore) and the evidence behind the defaults are on the wiki pages [CRD contracts](https://github.com/joulie-k8s/Joulie/wiki/CRD-Contracts#governor-policy-rules) and [Governors](https://github.com/joulie-k8s/Joulie/wiki/Governors).

## Hardware-discovery object: `NodeHardware`

Written by the agent; you do not create it.

| Field | Meaning |
|---|---|
| `spec.nodeName` | the node |
| `status.cpu` | raw and recognized model, sockets and cores, cap range, frequency `landmarks`, `governor`, control and telemetry availability |
| `status.gpu` | raw and recognized model, `count`, `capRangePerGpu`, `powerDomains`, `governor`, control and telemetry availability |
| `status.capabilities` | `cpuControl`, `gpuControl`, `cpuTelemetry`, `gpuTelemetry` |
| `status.quality.overall` | `exact` (models recognized), `heuristic` (raw model only), `unavailable` (no model) |

The controller manager uses it to recognize hardware against the inventory and to group nodes into hardware families ([Policy Algorithms]({{< relref "/docs/architecture/policies.md" >}})). Before an agent publishes it, the controller manager falls back to node hardware labels, which keeps simulator setups light.

```bash
kubectl get nodehardware <node> -o yaml
```

## Telemetry/control backend selection

The agent picks its backends from environment variables, not from a CRD: `TELEMETRY_CPU_SOURCE`, `TELEMETRY_CPU_CONTROL`, `TELEMETRY_GPU_CONTROL`, each `host` (default) or `http` (the simulator). See [Input Telemetry and Actuation Interfaces]({{< relref "/docs/architecture/telemetry.md" >}}).

## End-to-end contract flow

1. A user submits a workload, with or without the workload-class annotation.
2. The agent publishes `NodeHardware`.
3. The controller manager resolves hardware, plans profiles, writes `NodeTwin` and the node labels.
4. The scheduler extender places pods by those labels and twin scores.
5. The agent enforces `NodeTwin.spec` and reports status and metrics.

Field-by-field detail, an annotated `NodeHardware` example and the governor rules: [CRD contracts](https://github.com/joulie-k8s/Joulie/wiki/CRD-Contracts) on the wiki.
