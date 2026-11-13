---
title: "GPU Support (NVIDIA, AMD, Intel)"
weight: 10
---

How Joulie caps the GPUs of a node and sets their clock governor on NVIDIA, AMD and Intel, through `NodeTwin` intents the agent enforces.

## Validation status

GPU support has been validated in simulator mode only; the host code paths are written for bare-metal NVIDIA, AMD and Intel nodes, which have not run them yet.

## Contract model

`NodeTwin.spec.gpu.powerCap` holds a per-GPU cap with `scope: perGpu`. The first field present wins:

1. `capWattsPerGpu`: an absolute cap.
2. `capPctOfMax`: a percentage of the per-GPU maximum.

The same cap applies to every GPU of the node.

## Heterogeneous nodes

Profiles set percentages, so one profile fits every GPU family: `capPctOfMax=100` for performance, lower (for example 60) for eco. For deterministic simulator runs, the controller manager can resolve percentages to watts per GPU model (`GPU_MODEL_CAPS_JSON`) and write `capWattsPerGpu`.

## Shared and partitioned GPUs

Every component counts GPU resources the same way (`pkg/api.GPUResourceKind`):

| Resource | Exposure | One unit in a pod's GPU demand |
|---|---|---|
| `nvidia.com/gpu`, `amd.com/gpu`, `gpu.intel.com/i915`, `gpu.intel.com/xe`, any other `<vendor>/gpu` | whole GPUs, or MIG devices and replicas under the single MIG strategy and time-slicing without renaming, and AMD partitions under the single naming strategy of the AMD device plugin | 1 GPU |
| `nvidia.com/mig-<profile>` | MIG devices, mixed strategy | its compute slices out of 7: `1g.12gb` is 1/7, `3g.47gb` 3/7 |
| `amd.com/<compute>_<memory>`, such as `amd.com/cpx_nps4` | AMD compute partitions, mixed naming strategy of the AMD device plugin | 1 over the partitions of the compute mode: SPX 1, DPX 2, TPX 3, QPX 4, CPX 8 |
| `nvidia.com/gpu.shared`, `nvidia.com/mig-<profile>.shared` | time-slicing or MPS replicas renamed by the NVIDIA device plugin | the device it shares, because a replica can drive it alone while the other replicas idle |

How caps and estimates treat them:

| Case | Behaviour |
|---|---|
| Cap of a shared or partitioned GPU | one limit per physical GPU holds every MIG instance and replica on it; the node is planned like any GPU node |
| GPU count a cap multiplies | the agent's `NodeHardware` first, then the GPU count labels, then the units of the GPU resources |
| Extender estimate | weighs a pod's GPU requests by the table above ([Scheduler]({{< relref "/docs/architecture/scheduler.md" >}})); an estimate, so a MIG device of an A30 counts less than it is |
| MIG instances that must run at full clocks | keep the node out of Joulie with `joulie.io/reserved=true` |
| DRA node | a GPU node when GPU feature discovery labels its count or its agent lists GPUs |
| Intel monitoring and share resources (`gpu.intel.com/millicores` and the like) | not GPUs |

## Agent host backends

| Vendor | Interface | Files or tool | Backend name | Range and default |
|---|---|---|---|---|
| NVIDIA | `nvidia-smi` | `nvidia-smi -i <index> -pl <W>` per device | `host-nvidia` | `power.min_limit` to `power.max_limit`; `defaultWatts` from `power.default_limit` |
| AMD | amdgpu hwmon sysfs, as root, every write read back | `/sys/bus/pci/devices/<pci bus id>/hwmon/hwmon<N>/power1_cap` | `host-amd` | `power1_cap_min` to `power1_cap_max`; `defaultWatts` is the maximum |
| Intel | i915 or xe hwmon sysfs of each discrete GPU, as root, every write read back | PL1: `power1_max` (i915), the first of `power1_max` and `power2_max` (xe) | `host-intel` | up to `power<N>_rated_max`, no known minimum |

The agent runs no AMD or Intel tool to cap. A GPU it cannot cap, such as an NVIDIA GPU that prints `[N/A]` for its limits, ends `blocked` instead of failing the reconcile ([GPU power cap]({{< relref "/docs/architecture/agent.md#gpu-power-cap" >}})). On a two-die AMD package such as the MI250X, only the primary die holds the cap, and `NodeHardware` reports the dies that hold a limit in `gpu.powerDomains`.

## Governors

Next to the cap, the agent can set the clock governor of every GPU of a node from the preset in `NodeTwin.spec.gpu.governor` (`auto`, `sustained`, `efficient`, `low` or `peak`). It is off by default; see [Governor presets]({{< relref "/docs/hardware/hardware-modeling.md#69-governor-presets" >}}) for what the presets mean and [Governor control]({{< relref "/docs/architecture/agent.md#governor-control" >}}) for how the agent writes and restores them. A node lists a preset only when every GPU accepts it; GPUs are keyed by PCI bus id.

AMD (`amdgpu-sysfs`, the level in `power_dpm_force_performance_level`):

| Preset | Radeon PRO W7900 (Navi 31) | Instinct MI210, MI250, MI250X (Aldebaran) | Instinct MI300X |
|---|---|---|---|
| `auto` | level `auto` | `auto` | `auto` |
| `sustained` | `high` | `high` | not listed |
| `efficient` | not listed | `manual`, then `s 1 <MHz>` and `c` into `pp_od_clk_voltage` | same as Aldebaran |
| `low` | `low` | `low` | not listed |
| `peak` | `profile_peak` | `profile_peak` | not listed |

NVIDIA (`nvidia-smi -i <pci bus id>`, as root):

| Preset | Ampere and Ada (such as A40, L40S) | Hopper and Blackwell (such as H100, H200) |
|---|---|---|
| `auto` | `-rgc` and `-rmc`, only to undo the agent's own lock | `-rgc`, by the same rule |
| `sustained` | `-lmc <max>,<max>` | `equivalent`: memory runs at its rated clock |
| `efficient` | `-lgc <min>,<ceiling>` | same |
| `low` | `-lgc <min>,<min>` and `-lmc <min>,<min>` | `-lgc <min>,<min>` |
| `peak` | `-lgc <max>,<max>` and `-lmc <max>,<max>` | `-lgc <max>,<max>` |

Intel GPUs have no governor, and NVIDIA GPUs older than Ampere get none. No default selects `sustained`: opt a GPU model in through `GPU_GOVERNOR_MODEL_PRESETS_JSON` once a measurement shows the gain. The mappings are tested against emulated GPUs, not yet on hardware. Why a preset is not listed, and how results are read back: [Governors](https://github.com/joulie-k8s/Joulie/wiki/Governors#gpu-native-settings).

## Simulator mode

The simulator's `/control/{node}` accepts `gpu.set_power_cap_watts` with `capWattsPerGpu`, and `/otlp/telemetry/{node}` reports each GPU as an `hw.power` gauge with `hw.type=gpu` ([Power Simulator]({{< relref "/docs/simulator/power-simulator.md" >}})). In [emulate mode]({{< relref "/docs/simulator/emulate-mode.md" >}}) the agent caps emulated GPUs through the same `nvidia-smi` calls and sysfs writes as on a real node.

## Scheduling guidance

Placement intent is the `joulie.io/workload-class` pod annotation: `performance` runs on full-power nodes only (the extender rejects eco nodes), `standard`, the default, on any node. GPU resource requests (any resource of [Shared and partitioned GPUs](#shared-and-partitioned-gpus)) are independent of the workload class; Joulie's GPU capping is node-level, not a GPU slicing API.

## Known limitations

- **Intel GPUs without a default limit in sysfs are not capped**: no `power<N>_rated_max` (xe mailbox platforms, Flex under the out-of-tree i915) leaves the GPU cap `blocked`.
- **A GPU node whose GPUs cannot be capped runs uncapped in eco**, with no eco CPU cap either, unless `CPU_ECO_CAP_ON_UNCAPPABLE_GPU_NODES` is true.

Check the GPU control status of every GPU node:

```bash
kubectl get nodetwins -o custom-columns=NODE:.spec.nodeName,GPU:.status.controlStatus.gpu.result,MESSAGE:.status.controlStatus.gpu.message
```

## Example

- [Simulated cluster tutorial](https://github.com/joulie-k8s/Joulie/tree/main/examples/01-simulated-cluster): GPU caps on emulated NVIDIA and AMD nodes, applied by the real agent
- [`experiments/kwok-cluster`](https://github.com/joulie-k8s/Joulie/tree/main/experiments/kwok-cluster): GPU caps on 33 KWOK H100 NVL nodes, applied through the simulator's HTTP control

How sharing, partitioning, DRA and each backend work in detail: [GPU sharing and backends](https://github.com/joulie-k8s/Joulie/wiki/GPU-Sharing-and-Backends).
