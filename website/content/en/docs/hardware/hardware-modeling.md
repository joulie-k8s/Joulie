---
title: "Hardware Modeling and Physical Power Model"
weight: 30
aliases: ["/docs/simulator/hardware-modeling/"]
---

This page says what Joulie models about CPUs and GPUs and where the numbers come from. The agent uses these facts to resolve caps; the simulator uses them to turn utilization and controls into power and slowdown.

In the simulator these models are a test bed for Joulie's behaviour, not evidence of energy savings: they rest on vendor figures, public measurements and assumptions. What meaningful experiments need is tracked in the milestone [Evidence-based evaluation](https://github.com/joulie-k8s/Joulie/milestone/1).

> **Validation status.** Some catalog CPUs run public measured curves. GPU support is validated in simulation only. For GPUs and the other CPUs, the model combines official documentation with literature-based priors until direct calibration is possible.

## Data provenance

Every modelled value has one of three kinds of provenance; a proxy is never presented as a measurement.

| Kind | Examples |
|---|---|
| Official or read at runtime | CPU base and boost clocks, TDP and cTDP; GPU board power; cap ranges the node reports; CPU frequency landmarks and GPU clock ranges, which the agent publishes in `NodeHardware` (`status.cpu.landmarks`, `status.gpu.governor.clockRangesMHz`) as facts, not telemetry |
| Public measured | SPECpower node-level CPU power curves; published GPU throughput under power limits |
| Proxy or inferred | EPYC 9534 (from the 9654), EPYC 9375F (from the 9655), Xeon Gold 6530 (from an Intel family proxy); the simulator borrows no curve from a proxy, so these nodes run the analytic model |

## Hardware inventory

The catalog (`pkg/hwinv/assets/hardware.yaml`, read through `pkg/hwinv/catalog.go`) holds CPU and GPU entries, and a node is a discovered CPU model and count plus a GPU model and count. The agent, the controller manager and the simulator all read the same embedded copy, unless `HARDWARE_CATALOG_PATH` (`SIM_HARDWARE_CATALOG_PATH` for the simulator) replaces it. Model names are matched by alias after normalization.

The cluster inventory the experiments target:

| Node | GPUs | CPUs |
|---|---|---|
| GPU nodes | NVIDIA H100 NVL | 2x AMD EPYC 9654 |
| | NVIDIA H100 SXM | 2x Intel Xeon Gold 6530 |
| | NVIDIA L40S | 2x AMD EPYC 9534 |
| | AMD Instinct MI300X | 2x AMD EPYC 9534 |
| | AMD Radeon Pro W7900 | 2x AMD EPYC 9534 |
| | Intel Data Center GPU Flex 170 | 2x Intel Xeon Gold 6530 |
| CPU-only nodes | none | 2x AMD EPYC 9965, 2x AMD EPYC 9375F, 2x AMD EPYC 9655 |

Every entry, its vendor figures and its source are on the wiki page [Hardware catalog](https://github.com/joulie-k8s/Joulie/wiki/Hardware-Catalog).

## Hardware not in the catalog

Unknown hardware degrades realism; it does not break the control loop.

- The CPU and the GPU fall back independently to the generic model, or to no throttling.
- The simulator fills four facts in a fixed order: the node class key, else the environment variable, else the catalog, else a built-in constant (the variables are in the [configuration reference]({{< relref "/docs/getting-started/05-configuration-reference.md" >}})).
- An unknown GPU is capped between 80 and 300 W unless a class or a variable says otherwise.
- On a real node the agent still uses the cap limits the node reports.

## CPU model

A CPU with a public SPECpower curve (2x EPYC 9654, 9655 and 9965) runs that curve; any other runs an analytic model, `idle + (max - idle) x util^alpha x f^beta`, with a peak of sockets x TDP for a catalog CPU. A cap lowers the frequency until the package meets it, settling as a first-order lag. Compute-bound work slows with the frequency; memory-bound work slows little until the frequency falls below a knee. Telemetry is averaged, and thermal throttling is opt-in. The equations, defaults and sources are on [CPU power model](https://github.com/joulie-k8s/Joulie/wiki/CPU-Power-Model).

## GPU model

Each physical GPU is modelled on its own, and a node's GPU cap is the same per-device cap on every GPU. A workload has a natural power draw, lower for memory-bound work; a cap only matters once it falls below that draw. Under the cap, compute-bound work slows as `ratio^computeGamma`, fitted per family to published throughput (H100 SXM 0.43, H100 NVL 0.50, MI300X 0.62, L40S 1.20, others 1.0); memory-bound work slows less. Caps settle over a lag of 150 ms (350 ms on MI300X), and exported power is averaged over 1 s, as NVML reports it on NVIDIA, except on MI300X, whose driver reports instantaneous power. The equations, per-family values and sources are on [GPU power model](https://github.com/joulie-k8s/Joulie/wiki/GPU-Power-Model).

## Workload boundness

Each job carries `cpuUtilization`, `gpuUtilization`, `memoryIntensity`, `ioIntensity` and `cpuFeedIntensityGpu`, rather than a single label. Memory and IO intensity soften the slowdown a CPU cap or DVFS causes. A GPU job with a high CPU feed intensity also slows when the CPU is throttled.

## Heterogeneous caps

The controller manager writes caps as percentages by default (CPU of its cap range, GPU of the per-device maximum), and the agent or the simulator resolves them into watts from the node's own hardware data. Absolute per-device caps remain possible for deterministic experiments.

## What the simulator exports

Node CPU and GPU power (averaged and instantaneous), per-GPU caps, power and temperature, thermal-throttle fractions, performance multipliers, integrated energy and job completion statistics. See [Power Simulator]({{< relref "/docs/simulator/power-simulator.md" >}}).

## 6.8 Emulated nodes: governor and clock physics

An emulated node ([emulate mode]({{< relref "/docs/simulator/emulate-mode.md" >}})) runs the CPU and GPU governors its profile describes; the other modes keep an implied performance governor.

- **CPU.** Each cpufreq policy's governor sets a frequency every 100 ms substep, following the Linux v6.12 rules (`phys.GovernorStep`); `powersave` under intel_pstate with HWP and amd-pstate-epp follows an assumed EPP-shaped curve. Slower clocks keep the CPU busy longer for the same work, and a binding RAPL or HSMP cap wins over every governor.
- **GPU.** The cap and the governor are two limits on one clock: the GPU runs at the lower of the two, never at their product (`phys.GPUPowerWithClock`, `phys.GPUThroughputWithClock`).
- **Parameters.** Every value is assumed and to be calibrated.

The rules per governor are on [Governors](https://github.com/joulie-k8s/Joulie/wiki/Governors); the GPU clock rules and parameters on [GPU power model](https://github.com/joulie-k8s/Joulie/wiki/GPU-Power-Model#the-clock-model-of-emulated-nodes).

### Preset names

The emulator names the preset a CPU package or a GPU runs (the `joulie.hw.governor.preset` attribute of its [metrics]({{< relref "/docs/simulator/metrics.md#metrics" >}})) with the reverse mapping of `pkg/governor` (`ReverseCPU`, `ReverseAMD`, `ReverseNvidia`): the name is the preset whose plan the agent would write for that state. A state that no preset, or more than one, maps to has no name, and a GPU without a mapping table reports none. The tie-breaking rules are on [Governors](https://github.com/joulie-k8s/Joulie/wiki/Governors#emulated-nodes-preset-names).

### Peer transfers

GPU to GPU copies share each receiver's `gpus.transfer.peerLinkGBps`. A family can have a receiver gate (`gpus.transfer.receiverGate`): a receiver whose data path is at its lowest level, or idle under `auto`, lets through only a fraction of the link. The gate is off in every builtin profile; `SIM_GPU_RECEIVER_GATE=on` turns it on for what-if runs. Details are on [GPU power model](https://github.com/joulie-k8s/Joulie/wiki/GPU-Power-Model#peer-transfers).

## 6.9 Governor presets

A frequency governor is a control dimension of its own, next to the power cap: the cap bounds the power, the governor chooses the frequency below that bound. Joulie writes one vendor-neutral preset per component in `NodeTwin.spec`, and `pkg/governor` is the only place a preset becomes a native setting. The mapping is per driver in [CPU support]({{< relref "/docs/hardware/cpus.md#governors" >}}) and per GPU family in [GPU support]({{< relref "/docs/hardware/gpus.md#governors" >}}).

| Component | Preset | Intent |
|---|---|---|
| CPU | `performance` | highest frequency within `scaling_max_freq` at any load: race to idle |
| CPU | `balanced` | demand-driven, quick ramp |
| CPU | `efficiency` | demand-driven with an energy bias |
| CPU | `boost` `on` or `off` | frequency boost (turbo), changed only when the spec names it |
| GPU | `auto` | the vendor default |
| GPU | `sustained` | data-path clocks (memory, fabric) at their maximum, idle gating kept, the compute clock free to idle |
| GPU | `efficient` | the compute clock under a ceiling, `computeClockCeilingPctOfMax` of its maximum (80 % by default), the data path dynamic |
| GPU | `low` | every clock at its minimum; for experiments |
| GPU | `peak` | every clock at its maximum, idle gating off on AMD; for experiments |

- **Off by default.** It needs both the controller manager's `GOVERNOR_CONTROL` and the agent's `GOVERNOR_WRITES`; discovery is always on.
- **Restored.** The agent records the state it found before its first write and restores it when a preset disappears, when writes are turned off, and on exit ([Governor control]({{< relref "/docs/architecture/agent.md#governor-control" >}})).
- **GPU nodes.** A GPU node gets no CPU governor unless `CPU_GOVERNOR_ON_GPU_NODES` is true.
- **`sustained`.** No default selects it; opt a GPU model in once a measurement shows the gain.
- **Policy.** Which node gets which preset is under [Governor policy]({{< relref "/docs/architecture/policy.md#governor-policy" >}}); its defaults are provisional, and the [governor exploration]({{< relref "/docs/experiments/governor-exploration.md" >}}) studies them in the simulator.

What Joulie writes and never writes on each driver is on [Governors](https://github.com/joulie-k8s/Joulie/wiki/Governors#what-joulie-writes-and-what-it-leaves-alone).

Caveats, limitations and what happens on unknown hardware in detail: [Hardware model limitations](https://github.com/joulie-k8s/Joulie/wiki/Hardware-Model-Limitations). Sources: [Sources](https://github.com/joulie-k8s/Joulie/wiki/Sources).
