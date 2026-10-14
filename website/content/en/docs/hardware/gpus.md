---
title: "GPU Support (NVIDIA + AMD)"
weight: 10
---

Joulie supports node-level GPU power-cap intents for NVIDIA and AMD.

## Validation status

GPU support has been validated in simulator mode only (no bare-metal GPU access yet).
The host code paths are designed to work on bare metal (NVIDIA + AMD) when GPU nodes are available.

## Contract model

`NodeTwin.spec.gpu.powerCap` defines a per-GPU cap intent:

- `scope: perGpu`
- `capWattsPerGpu` (absolute, optional)
- `capPctOfMax` (percentage, optional)

Precedence:

1. `capWattsPerGpu` if present
2. otherwise `capPctOfMax`

The same cap is applied uniformly to all GPUs on the node.

## Heterogeneous nodes

Joulie supports heterogeneous GPU fleets by profile percentages:

- performance profile: `capPctOfMax=100`
- eco profile: `capPctOfMax` lower than 100 (for example 60)

Optional deterministic mode (simulator-oriented): controller manager can resolve percentages to absolute watts using model mapping (`GPU_MODEL_CAPS_JSON`) and write `capWattsPerGpu`.

## Shared and partitioned GPUs

A node is a GPU node when its agent lists GPUs in `NodeHardware`, when its labels state a GPU count, or when it advertises a GPU resource. Every component reads these resources the same way (`pkg/api.GPUResourceKind`):

| Resource | Exposure | One unit in a pod's GPU demand |
|---|---|---|
| `nvidia.com/gpu`, `amd.com/gpu`, `gpu.intel.com/i915`, `gpu.intel.com/xe`, any other `<vendor>/gpu` | whole GPUs, or MIG devices and replicas under the single MIG strategy and time-slicing without renaming, and AMD partitions under the single naming strategy of the AMD device plugin | 1 GPU |
| `nvidia.com/mig-<profile>` | MIG devices, mixed strategy | its compute slices out of 7: `1g.12gb` is 1/7, `3g.47gb` 3/7 |
| `amd.com/<compute>_<memory>`, such as `amd.com/cpx_nps4` | AMD compute partitions, mixed naming strategy of the AMD device plugin | 1 over the partitions of the compute mode: SPX 1, DPX 2, TPX 3, QPX 4, CPX 8 |
| `nvidia.com/gpu.shared`, `nvidia.com/mig-<profile>.shared` | time-slicing or MPS replicas renamed by the NVIDIA device plugin | the device it shares, because a replica can drive it alone while the other replicas idle |

The Intel monitoring and share resources (`gpu.intel.com/i915_monitoring`, `gpu.intel.com/millicores` and the like) are not GPUs. The scheduler extender weighs a pod's GPU requests this way ([Scheduler]({{< relref "/docs/architecture/scheduler.md" >}})); the counts are an estimate, so a MIG device of a GPU with four slices, such as the A30, counts less than it is.

The cap is per physical GPU, so the count it multiplies comes, in this order, from the agent's `NodeHardware` (the NVIDIA inventory lists physical GPUs, not MIG devices; the AMD inventory lists one device per compute partition, so on a partitioned AMD node the count is its partitions), from `joulie.io/hw.gpu-count`, from GPU feature discovery's `nvidia.com/gpu.count`, which counts physical GPUs except when the single MIG strategy exposes MIG devices (product suffixed `-MIG-`), and last from the units of the node's GPU resources, which count MIG devices, partitions and replicas. On a node with two GPU models GPU feature discovery labels the count of one of them, so the agent's count comes first. A DRA node advertises no GPU resource: it is a GPU node when GPU feature discovery labels its count or its agent lists GPUs.

NVML sets a power limit per device and has none per MIG instance, so the cap of a shared or partitioned GPU holds every MIG instance and replica on it, as a RAPL cap holds every pod on a CPU. A shared or partitioned GPU node is planned like any GPU node: the GPU cap of its profile, no eco CPU cap, and the GPU governor when governor control is on. The extender keeps performance pods off eco nodes, so every tenant of an eco GPU accepted eco; keep a node whose MIG instances must run at full clocks out of Joulie with `joulie.io/reserved=true`. A MIG reconfiguration resets the GPU and its limit, and the agent sets the cap again at its next reconcile. The limit of a MIG-enabled GPU follows the NVML documentation; it has not been measured on hardware.

An Intel GPU node (`gpu.intel.com/i915`, `gpu.intel.com/xe`) is a GPU node too, but the agent has no Intel GPU backend: its GPU cap ends `blocked` with `no supported GPU backend detected`, and in eco it gets no eco CPU cap either, so it runs uncapped.

GPU feature discovery appends `-MIG-<profile>`, `-MIG-INVALID` or `-SHARED` to `nvidia.com/gpu.product`. Joulie reads the product without them, for the catalog, the GPU family and the `GPU_MODEL_CAPS_JSON` and `GPU_GOVERNOR_MODEL_PRESETS_JSON` keys.

The emulated nodes of `simulator/pkg/hwemu` render every NVIDIA exposure (`gpus.exposure`: `whole`, `mig-single`, `mig-mixed`, `timeslice-rename`, `timeslice-norename`, `dra`); AMD partitions and Intel GPUs are covered by unit and contract tests only.

## Agent host backends

- NVIDIA: host backend uses NVIDIA tooling (power-limit set per device).
- AMD: the agent reads the cap range and writes the cap through the amdgpu hwmon device of each GPU, `/sys/bus/pci/devices/<pci bus id>/hwmon/hwmon<N>/power1_cap` (with `power1_cap_min` and `power1_cap_max`), as root, and reads every write back; it runs no AMD tool. The range is the driver's, `power1_cap_min` to `power1_cap_max`, so a 100 % cap is `power1_cap_max`; on a Radeon with overdrive on, that is above the board's default limit. The agent never writes a device whose `power1_cap` reads 0, the secondary die of a package whose primary holds the cap, and does not count an amdgpu device without `power1_cap`, such as an APU's integrated GPU. A GPU that is runtime suspended keeps the cap the agent last set ([GPU power cap]({{< relref "/docs/architecture/agent.md#gpu-power-cap" >}})). The NFD labels, `rocm-smi --showproductname` or the amdgpu hwmon devices themselves identify an AMD node.

When capabilities are unavailable/unsupported, status is reported as `blocked` rather than failing the whole reconcile loop.

## Governors

Next to the cap, the agent can set the clock governor of every GPU of a node from the preset in `NodeTwin.spec.gpu.governor` (`auto`, `sustained`, `efficient`, `low` or `peak`). It is off by default; see [Governor presets]({{< relref "/docs/hardware/hardware-modeling.md#69-governor-presets" >}}) for what the presets mean and [Governor control]({{< relref "/docs/architecture/agent.md#governor-control" >}}) for how the agent writes and restores them. Every GPU of the node gets the same preset, and a node lists a preset in `NodeHardware` `status.gpu.governor.presets` only when every GPU accepts it. GPUs are keyed by PCI bus id.

AMD GPUs (`amdgpu-sysfs`): the performance level in `/sys/bus/pci/devices/<pci bus id>/power_dpm_force_performance_level`, written as root and read back; `rocm-smi` and `amd-smi` are never used to set a governor.

| Preset | Radeon PRO W7900 (Navi 31) | Instinct MI210, MI250, MI250X (Aldebaran) | Instinct MI300X |
|---|---|---|---|
| `auto` | level `auto` | `auto` | `auto` |
| `sustained` | `high` | `high` | not listed: the level file accepts only `auto`, `manual` and `perf_determinism` |
| `efficient` | not listed: its overdrive file needs a non-default `ppfeaturemask` bit, and its fine-grained sclk takes only two levels | `manual`, then `s 1 <MHz>` and `c` into `pp_od_clk_voltage`, one command per write, waiting for the table to print again before `c` | same as Aldebaran |
| `low` | `low` | `low` | not listed |
| `peak` | `profile_peak` | `profile_peak` | not listed |

The `efficient` ceiling is a whole number of MHz, never below the lowest sclk level. A write the driver refuses (`EINVAL`, `EOPNOTSUPP` or `EBUSY`) is reported `unsupported`, and one without the privilege (`EACCES`, `EPERM` or `EROFS`) `refused`.

NVIDIA GPUs (`nvidia-smi`): every command is `nvidia-smi -i <pci bus id>`, as root. The clocks come from `nvidia-smi -q -d SUPPORTED_CLOCKS`.

| Preset | Ampere and Ada (such as A40, L40S) | Hopper and Blackwell (such as H100, H200) |
|---|---|---|
| `auto` | `-rgc` and `-rmc`, only to undo a lock in the agent's own record; with none, nothing is issued and the result is `equivalent` | `-rgc`, by the same rule |
| `sustained` | `-lmc <max>,<max>`; `equivalent` when the GPU lists one memory clock, as HBM parts do | `equivalent`: Hopper's memory runs at its rated clock and `-lmc` does not work on Hopper; Blackwell is mapped like Hopper until measured |
| `efficient` | `-lgc <min>,<ceiling>`, the ceiling being the highest supported graphics clock at or below the percentage of the maximum | same |
| `low` | `-lgc <min>,<min>` and `-lmc <min>,<min>` | `-lgc <min>,<min>` |
| `peak` | `-lgc <max>,<max>` and `-lmc <max>,<max>` | `-lgc <max>,<max>` |

A memory clock lock also bounds the graphics clock: at a lower memory clock a GPU offers only the lower graphics clocks (`SUPPORTED_CLOCKS` lists them per memory clock), so a lock at a low memory clock also holds the graphics clock at or below that memory clock's ceiling. The emulated GPUs model it (`gpus.clocks.supported.smMaxByMem`), in their power and speed and in `clocks.sm`.

A lock cannot be read back, so the result is `applied-unverified` until busy samples of the clock query show the clocks within it. An operator's own lock is never reset. Application clocks (`-ac`, `-rac`) and `--auto-boost-default` are never used. A GPU older than Ampere has no mapping and gets no governor.

No default selects `sustained`: opt a GPU model in through the controller manager's `GPU_GOVERNOR_MODEL_PRESETS_JSON` once a measurement on that model shows the gain. The mappings are tested against emulated GPUs (`simulator/pkg/hwemu` profiles and the fake `nvidia-smi` and `rocm-smi`), not yet on hardware.

## Simulator mode

The simulator's `/control/{node}` accepts `gpu.set_power_cap_watts` with `capWattsPerGpu`, and `/otlp/telemetry/{node}` reports each GPU as an `hw.power` gauge with `hw.type=gpu` (see [Power Simulator]({{< relref "/docs/simulator/power-simulator.md" >}})). In [emulate mode]({{< relref "/docs/simulator/emulate-mode.md" >}}) the agent caps emulated GPUs through the same `nvidia-smi` calls and amdgpu sysfs writes it makes on a real node.

## Scheduling guidance

Workload placement intent is expressed via the `joulie.io/workload-class` pod annotation:

- `performance`: must run on full-power nodes (extender hard-rejects eco nodes)
- `standard`: default, can run on any node; adaptive scoring steers toward eco when performance nodes are congested

GPU resource requests (any resource of [Shared and partitioned GPUs](#shared-and-partitioned-gpus)) are independent from Joulie workload classes.
Joulie GPU capping is node-level and not a GPU slicing API.

## Known limitations

- **Intel GPUs are not capped.** The agent has no Intel GPU backend, so an Intel GPU node runs uncapped ([Shared and partitioned GPUs](#shared-and-partitioned-gpus)).

Check the GPU control status of every GPU node with:

```bash
kubectl get nodetwins -o custom-columns=NODE:.spec.nodeName,GPU:.status.controlStatus.gpu.result,MESSAGE:.status.controlStatus.gpu.message
```

## Example

- [Example 07: simulator-gpu-powercaps](https://github.com/joulie-k8s/Joulie/tree/main/examples/07-simulator-gpu-powercaps)
