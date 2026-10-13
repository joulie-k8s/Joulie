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

## Agent host backends

- NVIDIA: host backend uses NVIDIA tooling (power-limit set per device).
- AMD: host backend uses ROCm SMI tooling (`rocm-smi`) where supported.

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

A lock cannot be read back, so the result is `applied-unverified` until busy samples of the clock query show the clocks within it. An operator's own lock is never reset. Application clocks (`-ac`, `-rac`) and `--auto-boost-default` are never used. A GPU older than Ampere has no mapping and gets no governor.

No default selects `sustained`: opt a GPU model in through the controller manager's `GPU_GOVERNOR_MODEL_PRESETS_JSON` once a measurement on that model shows the gain. The mappings are tested against emulated GPUs (`simulator/pkg/hwemu` profiles and the fake `nvidia-smi` and `rocm-smi`), not yet on hardware.

## Simulator mode

Simulator control endpoint accepts `gpu.set_power_cap_watts` with `capWattsPerGpu`.

Simulator telemetry includes:

- `gpu.present`
- `gpu.vendor`
- `gpu.count`
- `gpu.powerWattsTotal`
- `gpu.avgPowerWattsTotal`
- `gpu.capWattsPerGpuApplied`
- `gpu.utilization`

Per-device entries also expose:

- `temperatureC`
- `thermalThrottle`
- `avgPowerWatts`

This makes it easier to compare:

- internal power-cap dynamics,
- exported averaged telemetry,
- and sustained thermal behavior during long runs.

## Scheduling guidance

Workload placement intent is expressed via the `joulie.io/workload-class` pod annotation:

- `performance`: must run on full-power nodes (extender hard-rejects eco nodes)
- `standard`: default, can run on any node; adaptive scoring steers toward eco when performance nodes are congested

GPU resource requests (`nvidia.com/gpu`, `amd.com/gpu`) are independent from Joulie workload classes.
Joulie GPU capping is node-level and not a GPU slicing API.

## Example

See:

- [Example 07: simulator-gpu-powercaps](https://github.com/joulie-k8s/Joulie/tree/main/examples/07%20-%20simulator-gpu-powercaps)
