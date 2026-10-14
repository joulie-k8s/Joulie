---
title: "Hardware Modeling and Physical Power Model"
weight: 30
aliases: ["/docs/simulator/hardware-modeling/"]
---

This page documents how Joulie models CPUs and GPUs across the project using a mix of:

- **official vendor specifications and management APIs**,
- **public measured power curves**, and
- **explicit proxy models** where public exact curves are not yet available.

It serves two closely related purposes:

- for the **agent**, it describes the hardware assumptions used to resolve caps, interpret device limits, and reason about how throttling affects attainable performance
- for the **simulator**, it describes the physical model used to turn utilization and control actions into simulated power and slowdown

## Quick summary

If you want the short version before the details:

- Joulie does **not** use a single flat `utilization -> watts` rule for every platform.
- When public measured curves exist, Joulie uses them directly.
- When they do not, Joulie falls back to explicit generic or proxy models.
- CPU and GPU throttling are modeled as **time-dependent control loops**, not instantaneous jumps.
- Frequency governors are a control dimension of their own, next to power caps: one vendor-neutral preset per component, which the agent maps to the node's cpufreq driver or GPU family (see [Governor presets](#69-governor-presets)).
- Slowdown depends on workload regime:
  - compute-bound workloads are more sensitive to throttling,
  - memory-/bandwidth-bound workloads are often less sensitive,
  - GPU jobs can also be affected by CPU-side feeding pressure.
- Exported telemetry is intentionally modeled as what operators really see:
  - often averaged,
  - sometimes delayed,
  - and not always identical to the simulator's internal instantaneous state.

> **Validation status**
>
> - CPU models already use public measured curves for some catalog-backed platforms.
> - GPU support has been validated in **simulation mode only** so far; bare-metal GPU access was not available yet.
> - For GPUs, and for some CPU SKUs, the current model combines official documentation with literature-based priors until direct calibration becomes possible.

## 1. Why this page exists

The default "power = idle + dynamic(util)" model is useful for quick experimentation, but it is not enough for a realistic digital twin of a heterogeneous cluster or for an agent that must make sense of heterogeneous device limits.

Real hardware behavior depends on:

- vendor-specific control interfaces (RAPL, `intel_pstate`, `amd-pstate`, NVML, ROCm SMI),
- device-specific limits (TDP/TBP, cap range, turbo/headroom),
- non-linear utilization-power curves,
- workload class (compute-bound vs memory-bound),
- and control settling behavior.

This page explains how Joulie represents those effects.

## 1.1 How to read this page

The page is organized from highest-level to most implementation-specific:

1. data provenance and inventory philosophy,
2. CPU model,
3. GPU model,
4. workload-boundness and heterogeneous-cap semantics,
5. realism caveats and references.

If you are new to Joulie, the most important sections are:

- [Data provenance model](#2-data-provenance-model)
- [CPU physical model](#5-cpu-physical-model)
- [GPU physical model](#6-gpu-physical-model)
- [Measurement philosophy and realism](#10-measurement-philosophy-and-realism)

## 2. Data provenance model

Joulie distinguishes three kinds of hardware data:

### 2.1 Official / runtime-exact
Examples:
- CPU base and boost frequencies
- CPU TDP / cTDP
- GPU board/TDP/TBP values
- min/max power-cap ranges queried at runtime
- CPU frequency landmarks (`cpuinfo_min_freq`, `cpuinfo_max_freq`, `base_frequency`) and GPU clock ranges (the lowest and highest levels of the amdgpu `pp_dpm_*` tables; `nvidia-smi -q -d SUPPORTED_CLOCKS` and the `clocks.max.*` fields of the clock query), which the agent reads at discovery for the governor presets and publishes in `NodeHardware` (`status.cpu.landmarks`, `status.gpu.governor.clockRangesMHz`). They are facts about the hardware, not telemetry: no component reads a running clock as an input to the twin or the policy.

### 2.2 Public measured
Examples:
- SPECpower node-level CPU power curves
- public measurements of GPU power-cap behavior and slowdown

### 2.3 Proxy / inferred
Used only when no exact public curve is available.

Examples:
- EPYC 9534 curve derived from the public EPYC 9654 Genoa curve
- EPYC 9375F curve derived from the public EPYC 9655 Turin curve
- Xeon Gold 6530 curve derived from official specs plus a family-level Intel proxy

Proxy entries are explicitly marked as such in the hardware catalog.

In practice, this gives Joulie a clear rule:

- prefer exact runtime or official information,
- use public measured curves when available,
- fall back to explicit proxies only when needed,
- and never pretend a proxy is a direct measurement.

## 3. Hardware inventory currently modeled

The inventory is **compositional** rather than node-class-only.

It is organized around reusable:

- CPU model entries
- GPU model entries

Nodes are then understood as:

- discovered CPU model + count
- discovered GPU model + count

This is used consistently by:

- the agent, which discovers raw hardware facts,
- the controller manager, which resolves those facts against the inventory,
- the simulator, which composes node models from the same CPU/GPU entries.

In the codebase, the shared inventory shape lives in:

- `pkg/hwinv/catalog.go`
- embedded catalog data under `pkg/hwinv/assets/hardware.yaml`

All three binaries read that embedded copy unless `HARDWARE_CATALOG_PATH`
(or `SIM_HARDWARE_CATALOG_PATH` for the simulator) points at a file that
replaces it. The experiments read the embedded copy too: a model their
inventories name needs an entry, with an alias for each spelling the
simulator sees (the raw name and its label-safe form), or it falls back to
the generic model described in section 3.1.

Matching is alias-based and normalization-based:

- raw model names discovered by the agent or provided by simulator labels are normalized: lower case, `(R)` and `(TM)` dropped in both their raw and label-safe spellings, and a trailing clock such as `CPU @ 2.40GHz` dropped,
- then matched against canonical CPU/GPU inventory keys and aliases,
- and unresolved subsystems fall back independently to generic modeling or no-throttle behavior, depending on what capability exists.

The heterogeneous cluster inventory currently targeted includes:

### GPU-equipped nodes
- NVIDIA H100 NVL + 2x AMD EPYC 9654
- NVIDIA H100 SXM + 2x Intel Xeon Gold 6530
- NVIDIA L40S + 2x AMD EPYC 9534
- AMD Instinct MI300X + 2x AMD EPYC 9534
- AMD Radeon Pro W7900 + 2x AMD EPYC 9534

### CPU-only nodes
- 2x AMD EPYC 9965
- 2x AMD EPYC 9375F
- 2x AMD EPYC 9655

The goal of the inventory is therefore **compositional realism**, not an exhaustive list of every node SKU that might ever appear in a cluster.

## 3.1 What happens for hardware not in the inventory

Joulie does **not** require every simulated platform to be present in the catalog.

Today, hardware is resolved in this order:

1. discover or provide raw CPU/GPU hardware facts,
2. match those facts against the inventory when possible,
3. start from a generic base model when exact inventory coverage is missing,
4. apply any explicit simulator overrides from `SIM_NODE_CLASS_CONFIG`,
5. enrich with measured or proxy catalog data where available.

In the simulator, five hardware facts follow a fixed precedence: the node class key, else the environment variable, else the catalog entry the node matches, else a built-in constant. A value of 0 or less counts as unset.

| Field (class key) | Variable | From the catalog | Constant |
|---|---|---|---|
| GPU maximum per GPU (`gpu.maxWattsPerGpu`) | `SIM_GPU_MAX_WATTS_PER_GPU` | `maxBoardPowerW` | 300 W |
| GPU minimum cap per GPU (`gpu.minCapWattsPerGpu`) | `SIM_GPU_MIN_CAP_WATTS_PER_GPU` | `minBoardPowerW`, else half the maximum | 80 W |
| CPU socket cap maximum (`cpuSocketCapMaxW`) | `SIM_CPU_SOCKET_CAP_MAX_W` | `tdpW` | 400 W |
| CPU socket cap minimum (`cpuSocketCapMinW`) | `SIM_CPU_SOCKET_CAP_MIN_W` | the low end of `cTdpRangeW`, else 0.55 x `tdpW` | 120 W |
| Peak CPU power of the generic model (`pMaxW`) | `SIM_P_MAX_W` | sockets x `tdpW`, for a CPU without a measured curve (assumed: a package draws its TDP at full load) | 420 W |

A catalog minimum above a maximum set by a class or a variable is lowered to that maximum. A measured `node2S` curve was taken on a two-socket server; on another socket count the simulator scales the whole curve, idle included, by sockets/2 (assumed: no public curve of such a node exists). A curve from a node class is used as given.

If the platform is not in the inventory described on this page, the system still works.
What changes is the level of realism and how much hardware-specific information is available:

- no catalog-backed exact CPU curve,
- no catalog-backed official/proxy provenance for that platform,
- no catalog-backed GPU board-power metadata, so the simulator caps such a GPU between 80 and 300 W unless a class or a variable says otherwise,
- therefore more reliance on the generic fallback model and any explicit overrides you provide.

So an unknown platform is treated as a **generic or manually overridden platform**, not as a calibrated inventory entry.

For the real agent, that means:

- control backends still try to use runtime-observed limits from the node,
- but there may be less model-specific information available for normalized cap resolution or for simulator-style deterministic experiments.

This is an important design choice: unknown hardware should degrade realism, not break the control loop.

## 4. Official hardware facts used in the catalog

### 4.1 CPUs
- **AMD EPYC 9654**: 96 cores, base 2.4 GHz, max boost up to 3.7 GHz, default TDP 360 W, cTDP 320-400 W.
- **AMD EPYC 9534**: 64 cores, base 2.45 GHz, max boost up to 3.7 GHz, default TDP 280 W, cTDP 240-300 W.
- **AMD EPYC 9965**: 192 cores, base 2.25 GHz, max boost up to 3.7 GHz, default TDP 500 W, cTDP 450-500 W.
- **AMD EPYC 9375F**: 32 cores, base 3.8 GHz, max boost up to 4.8 GHz, default TDP 320 W, cTDP 320-400 W.
- **AMD EPYC 9655**: 96 cores, base 2.6 GHz, max boost up to 4.5 GHz, default TDP 400 W, cTDP 320-400 W.
- **Intel Xeon Gold 6530**: 32 cores, base 2.1 GHz, max turbo 4.0 GHz, TDP 270 W.

### 4.2 GPUs
- **NVIDIA H100 NVL**: max TDP 400 W. Minimum cap 200 W: no minimum is published for the NVL, so the catalog uses the 200 W minimum of the [H100 PCIe product brief](https://www.nvidia.com/content/dam/en-zz/Solutions/gtcs22/data-center/h100/PB-11133-001_v01.pdf).
- **NVIDIA H100 SXM**: up to 700 W configurable. Minimum cap 300 W: the lowest limit NVIDIA set itself, in the DGX-H100 MaxQ configurations of its [MLPerf Inference v4.0 submission](https://github.com/mlcommons/inference_results_v4.0/tree/main/closed/NVIDIA) (`power_limit = 300`). It is not a datasheet minimum.
- **NVIDIA L40S**: max power consumption 350 W. Minimum cap 200 W, with no public source: the [L40S product brief](https://www.pny.com/File%20Library/Company/Support/product-briefs/data-center-gpus/NVIDIA-L40S-Product-Brief.pdf) gives the minimum as "TBD".
- **AMD Instinct MI300X**: maximum TBP 750 W. Minimum cap 400 W: [a public study](https://arxiv.org/html/2603.16164v1) found that limits below 400 W were not reliably enforced, and the GPUs drew more power than configured.
- **AMD Radeon Pro W7900**: total board power 295 W. Minimum cap 200 W, with no public source: AMD publishes no cap range.

The minimum cap is `minBoardPowerW` in the catalog. When a node reports no GPU cap range, the controller manager takes the range from the catalog: the NodeTwin's GPU cap range starts at this minimum, and a percent cap turned into watts is raised to it.

## 5. CPU physical model

At the CPU side, Joulie combines three layers:

- measured node-level curves where available,
- family-level proxy curves where necessary,
- and a generic fallback model when no better prior exists.

## 5.1 Exact measured node curves where available

For these catalog-backed platforms, Joulie uses public SPECpower_ssj2008 measurements directly as the default node-level CPU power curve. Each curve gives the result it comes from, the test sponsor, the overall ssj_ops/watt that SPEC requires next to any figure taken from a result, and the tuning that sets it apart from a typical Kubernetes node. The results were retrieved on 2026-10-07; section 12.2 links them.

### 2x AMD EPYC 9654

SPECpower_ssj2008 result 01203 [R18]: ASUSTeK Computer Inc. RS720A-E12-RS12, tested Nov 23 2022, 30,064 overall ssj_ops/watt. The BIOS ran in Efficiency Mode with memory at DDR3600.

- idle: 128 W
- 10%: 257 W
- 20%: 300 W
- 30%: 340 W
- 40%: 378 W
- 50%: 410 W
- 60%: 442 W
- 70%: 498 W
- 80%: 577 W
- 90%: 697 W
- 100%: 817 W

The emulated 2x EPYC 9654 profiles, `amd-epyc-2s-energy-only` and `amd-instinct-mi300x-8gpu`, run this curve too (`physics.cpu.model: catalog-curve`), split over their two packages.

### 2x AMD EPYC 9655

SPECpower_ssj2008 result 01457 [R19]: Lenovo Global Technology ThinkSystem SR645 V3, tested Sep 18 2024, 34,402 overall ssj_ops/watt. The BIOS capped boost at 2.75 GHz (`BoostFmax:2750`), so full load draws less than on a node that boosts to 4.5 GHz.

- idle: 126 W
- 10%: 273 W
- 20%: 329 W
- 30%: 383 W
- 40%: 433 W
- 50%: 482 W
- 60%: 531 W
- 70%: 579 W
- 80%: 625 W
- 90%: 675 W
- 100%: 751 W

The catalog names `AMD_EPYC_9655` as the `proxyFrom` family of the EPYC 9375F, but the simulator builds no proxy curve from it (`cpuProxyFrom` is only a telemetry label), so 9375F nodes do not run on this curve.

### 2x AMD EPYC 9965

SPECpower_ssj2008 result 01522 [R20]: ASUSTeK Computer Inc. RS720A-E13-RS8U, tested Apr 7 2025, 44,168 overall ssj_ops/watt. Windows ran the Power Saver power plan.

- idle: 157 W
- 10%: 265 W
- 20%: 314 W
- 30%: 362 W
- 40%: 412 W
- 50%: 461 W
- 60%: 503 W
- 70%: 544 W
- 80%: 587 W
- 90%: 661 W
- 100%: 800 W

These curves are interpolated with a monotone spline so that simulated power remains monotonic with load.

## 5.2 Proxy CPU curves
For CPU SKUs without exact public full curves, Joulie uses explicit family-level proxies:

- **EPYC 9534** <- Genoa proxy derived from EPYC 9654
- **EPYC 9375F** <- Turin proxy derived from EPYC 9655
- **Xeon Gold 6530** <- Intel Emerald Rapids proxy derived from official specs and a family-level curve

Proxy entries remain easy to replace once direct measurements become available.

The simulator does not borrow the proxy family's curve: a proxy entry has no curve of its own, so its nodes run the generic model of section 5.6 with a peak of sockets x TDP (assumed: a package draws its TDP at full load). Two EPYC 9534 sockets peak at 560 W, where the generic default is 420 W.

## 5.3 DVFS and power-cap semantics

The CPU control path matters because the simulator is trying to mirror the same qualitative behavior that the real agent/controller manager path expects:

- performance hints and DVFS affect attainable throughput,
- package power caps constrain average power rather than clipping instantaneous power,
- and the response depends on workload class and platform behavior.

### AMD CPUs
On modern AMD servers, `amd-pstate` uses **CPPC** and supports finer-grain performance control than legacy ACPI P-states. The Linux kernel documentation also exposes useful landmarks such as:

- `highest_perf`
- `nominal_perf`
- `lowest_nonlinear_perf`
- `min_freq`

This matters because the performance drop is not always proportional to the requested cap: memory-bound workloads often remain insensitive until the control point falls below the non-linear knee.

### Intel CPUs
`intel_pstate` does not generally expose a full public frequency table. Without HWP, the driver's utilization callback does not run more often than every 10 ms.

So the simulator does **not** assume that all Intel server parts expose a clean public list of "frequency slices". Instead, it models:
- a continuous requested performance state,
- optional quantization when the runtime actually exposes a table,
- driver update and cap-application delay.

### RAPL / average power caps
For CPUs, Joulie models average package power caps, not instantaneous power clipping. Requested caps are clamped to the available hardware range and translated into an attainable performance state with a time constant.

On an emulated node a limit is held in its register field (`limitBits`: 15 bits for PL1 and PL2, 17 on psys, 13 for PL4, 18 on TPMI), in the zone's power units, so a value past the field wraps to a small cap, as on hardware; the agent clamps what it writes to `constraint_N_max_power_uw` for that reason.

## 5.4 CPU slowdown model
CPU slowdown depends on workload class:

- **compute-bound**: throughput tracks the effective performance/frequency state closely
- **memory-bound**: throughput degrades more slowly until the requested point crosses the non-linear knee
- **mixed**: weighted blend

This means the same DVFS/RAPL action can produce different throughput changes depending on workload characteristics.

## 5.5 CPU control dynamics, telemetry windows, and thermal state

Joulie treats CPU control as a **time-dependent loop**, not as an instantaneous jump from one idealized state to another.

### How the model advances in time

The model of a node outside emulate mode advances only in a physics step, by a stretch of simulated time `dt`, and nothing else moves it:

- In Kubernetes mode a physics tick steps every modelled node by the wall time since the last tick: every `SIM_PHYSICS_TICK` (default 1s), or on the emulator's tick in emulate mode.
- In standalone mode each tick steps every node by `SIM_SA_DT` seconds of virtual time.
- A read (`/telemetry/`, `/otlp/telemetry/`, `/state/`, the gauges) reports the node as the last step left it. A control (`/control/`) only sets the targets that the next step moves toward. How often a node is read therefore never changes what it does.

A step splits `dt` into equal substeps `h` of at most 100 ms in Kubernetes mode, or of at most `SIM_SA_SUBSTEP` (default `SIM_SA_DT`) in standalone mode. Each substep runs, in order: the frequency ramp, the CPU cap lag, the GPU cap lag (section 6.5), the CPU power with the frequency solved down to the cap, the CPU temperature, the GPU power and temperature (section 6.6), the telemetry averages, and the energy. Every lag below is one clamped Euler step per substep: a substep as long as the time constant reaches the target at once, so a short substep is what keeps a long tick from snapping a lag.

The energy a node books is the integral of its **instantaneous** power over the substeps, the CPU and the GPUs apart, not the averaged telemetry signal times `dt`.

### Cap application dynamics

Requested CPU package caps settle toward their target with a first-order time constant, one step per substep `h`:

```text
Cap_applied(t + h) = Cap_applied(t) + (Cap_target - Cap_applied(t)) * min(1, h / tau_cap)
```

where `tau_cap` is configured by `cpuCapApplyTauMs`.

This better matches how real CPU package-power control behaves in practice:

- power limits are average-power constraints, not instantaneous clipping
- the final frequency/performance state is reached through firmware and scheduler feedback loops
- short transients may therefore look quite different from steady state

This is also consistent with recent public work on Intel RAPL behavior, which reports
platform-dependent settling times ranging from sub-second to multi-second behavior and
shows that the effective response depends on workload type, not only on the configured
limit (see [R28] in References).

### Telemetry windows

The simulator also exports an averaged CPU power signal, using `cpuTelemetryWindowMs`, in addition to the underlying instantaneous modeled power.

This is intentional. In the real system, what an operator or a benchmark harness sees is often an *observed* power signal with a window or update cadence, not the hidden instantaneous internal device power.

That distinction is part of the realism goal of Joulie: the simulator should not be easier to interpret than the real cluster.

### Thermal evolution

CPU temperature is modeled as a first-order lag toward a power-dependent equilibrium:

```text
T_target = T_ambient + P_cpu / K_cpu
T(t + h) = T(t) + (T_target - T(t)) * min(1, h / tau_thermal)
```

where:

- `K_cpu` is represented by `cpuWattsPerDeltaC`
- `tau_thermal` is represented by `cpuThermalTauMs`

Thermal throttling is opt-in. Two thresholds turn it on, as profile keys or as environment variables:

- `cpuThermalThrottleStartC` (`SIM_CPU_THERMAL_THROTTLE_START_C`)
- `cpuThermalThrottleFullC` (`SIM_CPU_THERMAL_THROTTLE_FULL_C`)

Both default to 0, which means off: temperature is still modeled and reported in the telemetry response, but it never reduces throughput. With both set, the throttle fraction rises linearly from 0 at the start temperature to 1 at the full temperature and scales the throughput multiplier down by that fraction. On a measured curve the lower throughput moves the operating point down the curve; the generic fallback model (section 5.6) also lowers power by up to 35%. Set both thresholds, the full one above the start. As environment variables, one is enough: the other takes 85 °C (start) or 97 °C (full). As node class keys, a threshold the class leaves unset comes from the base profile, which is 0 unless the environment sets the pair. Setting only one in a class is therefore a known sharp edge:

- A full threshold alone is accepted and throttles linearly from 0 °C. With `cpuThermalThrottleFullC: 97`, an idle 2x AMD EPYC 9654 node on its measured curve (128 W, so 24 + 128 / 8 = 40 °C) already has a throttle fraction of 40 / 97 = 0.41.
- A start threshold with no full threshold puts the start above the full threshold, which validation rejects: every class in `SIM_NODE_CLASS_CONFIG` is ignored, with only a warning in the log, and all nodes run on the base profile.

Throttling is off by default because the thermal constants are not calibrated per platform. With the default 24 °C ambient and 8 W/°C, a 2x AMD EPYC 9654 node at full load on its measured curve (817 W, section 5.1) heads for 24 + 817 / 8 = 126.1 °C. With the former defaults of 85 and 97 °C it throttled fully: throughput fell to 0 and power to 141 W, the node cooled, recovered and heated again. That limit cycle came from the model, not from the hardware.

When enabled, throttling distinguishes between:

- short capped bursts,
- long steady capped runs,
- and thermally limited sustained operation.

## 5.6 Generic fallback CPU model

When no measured or proxy CPU curve is available for a platform, Joulie falls back to the older generic CPU model.

### Power from utilization and frequency

For CPU package power, the fallback model is:

```text
P_cpu = BaseIdleW + (PMaxW - BaseIdleW) * util^AlphaUtil * freqScale^BetaFreq
```

where:

- `util` is normalized CPU utilization in `[0, 1]`
- `freqScale` is normalized effective frequency/performance state in `[0, 1]`
- `BaseIdleW` is idle node CPU power
- `PMaxW` is uncapped max CPU power: sockets x TDP for a catalog CPU without a measured curve, otherwise `SIM_P_MAX_W` (default 420 W); a node class `pMaxW` or the variable wins over the catalog (section 3.1)
- `AlphaUtil` controls how aggressively power rises with utilization
- `BetaFreq` controls how aggressively power rises with frequency/performance state

Power never depends on whether Joulie manages the node: at the same utilization and frequency, a node with a throttle target draws what a node without one draws, idle included.

This is the baseline physical model used for generic hardware.
In practice, it is most visible in the simulator, but it also captures the same high-level assumption the agent relies on: lower effective performance state generally means lower power and lower throughput.

### Power-cap handling

If simulated package power exceeds the requested CPU cap:

1. the simulator solves for a lower `freqScale` that satisfies the cap,
2. clamps that frequency to the minimum modeled frequency ratio,
3. recomputes power with the reduced `freqScale`,
4. marks the node as cap-saturated when even minimum frequency cannot respect the requested cap.

The reported power is the recomputed power, clipped to the cap plus a small headroom (`SIM_RAPL_HEADROOM_W`, default 5 W) but never below the power at the minimum frequency:

```text
P_reported = min(P, max(cap + headroom, P_at_minimum_frequency))
```

A cap below that floor cannot pull the package any lower, so a cap-saturated node reports, and books as energy, what it draws at its lowest frequency. With the default model (80 W idle, 420 W maximum, minimum frequency ratio 1200/3200), util 1 under a 100 W cap reports 170.5 W, not 105 W. A measured curve floors the same way, at the curve's own power at the minimum frequency.

So CPU caps reduce power mainly by reducing the effective frequency/performance state.

### Throttling to frequency

The DVFS fallback path uses a normalized throttle percentage:

```text
targetScale = 1 - throttlePct / 100
```

The node does not jump to that scale instantly.
Instead it ramps toward it using a first-order settling model controlled by `DvfsRampMS`.

### Frequency to slowdown

For generic hardware, throughput slowdown depends on workload class:

- `cpu.compute_bound`: throughput multiplier tracks `freqScale` closely
- `cpu.memory_bound`: slowdown is softer above the non-linear knee
- `cpu.mixed`: blend of the two behaviors

So in the fallback model:

- throttling lowers `freqScale`,
- lower `freqScale` lowers power,
- and lower `freqScale` also reduces throughput according to workload class.

## 6. GPU physical model

At the GPU side, Joulie models both:

- how much power a workload would naturally like to consume,
- and how much throughput it loses when a power cap forces it away from that natural operating point.

## 6.1 Per-device, not only per-node
Joulie models GPUs **per physical GPU device** on the node.
A node-level "GPU profile" is implemented by applying the same per-device power cap to every GPU on the node.

This is the simplest robust strategy for heterogeneous clusters and matches the underlying vendor APIs well.

The simulator distributes the node-average GPU utilization across the devices rather than giving every device the average: as many devices as the average needs run at the utilization that gives it, and the rest sit at zero. A device never draws less than its idle power, whatever its cap: a power limit caps the active draw, and the model has no deeper idle state for capped devices.

## 6.2 Vendor APIs used by the real agent

The agent calls the vendor command-line tools and reads sysfs; it links no vendor library. Measured GPU power reaches the twin through Prometheus (`hw.power`, `hw.energy`), never through these calls.

### NVIDIA
- Caps: `nvidia-smi --query-gpu=index,power.min_limit,power.max_limit,power.limit,power.draw,name` for the cap range and the current limit, and `nvidia-smi -i <index> -pl <W>` to set a per-device limit, run only for a GPU whose current limit differs from the cap.
- Governors: `nvidia-smi --query-gpu=pci.bus_id,clocks.sm,clocks.mem,clocks.max.sm,clocks.max.mem,pstate,clocks_throttle_reasons.active` (again with `clocks_event_reasons.active` when a driver rejects that field) and `nvidia-smi -q -d SUPPORTED_CLOCKS` for discovery, and `nvidia-smi -i <pci bus id>` with `-lgc`, `-lmc`, `-rgc` or `-rmc` to set or reset clock locks. A lock has no getter, so the clock query is also how the agent sees a lock in use (see [Governor control]({{< relref "/docs/architecture/agent.md#governor-control" >}})).

### AMD
- Caps: amdgpu sysfs, the hwmon device named `amdgpu` under `/sys/bus/pci/devices/<pci bus id>/hwmon/`, as root, each write read back: `power1_cap_min` and `power1_cap_max` for the cap range, and `power1_cap` for the current cap, written in whole watts as microwatts only for a GPU whose cap differs. `power1_cap_default` is not read, because the driver sets it from the cap in force whenever it initialises the GPU. A device whose `power1_cap` reads 0, the secondary die of a package, is never written, and one without `power1_cap`, such as an APU's integrated GPU, is not counted as a GPU. `rocm-smi` and `amd-smi` are never used to read or set a cap; `rocm-smi --showproductname` is only a vendor probe.
- Governors: amdgpu sysfs under `/sys/bus/pci/devices/<pci bus id>/`, as root, each write read back: `power_dpm_force_performance_level`, the `pp_dpm_sclk`, `pp_dpm_mclk` and `pp_dpm_fclk` tables for the clock ranges, and `pp_od_clk_voltage` for a compute clock ceiling. `rocm-smi` and `amd-smi` are never used to set a governor.

## 6.3 Natural power envelope
For GPU workloads, Joulie uses the concept of a **natural power envelope**:
- the power that a workload would draw if not artificially capped.

This is important because many workloads are not naturally power-saturating:
- memory-bound jobs may draw much less than TBP,
- so moderate caps above their natural envelope may have almost no performance effect.

This is one of the most important ideas in the whole page. A cap only matters when it is low enough to be active for the workload in question.

## 6.4 Compute-bound vs memory-bound slowdown
Public measurements show very different behavior across workload classes:

- **compute-bound kernels** often scale close to linearly with clock frequency and become sensitive to power limits once the cap reduces achievable clocks.
- **memory-bound kernels** saturate and often stay insensitive until the cap falls below their natural power envelope.

Joulie therefore models at least:
- `gpu.compute_bound`
- `gpu.memory_bound`
- `gpu.mixed`

with different cap->throughput curves.

## 6.5 Cap settling delay
GPU capping is not always instantaneous. Public MI300X measurements report settling delays in the **hundreds of milliseconds** after a large power-cap reduction. The simulator includes per-vendor/device settling delays for this reason.

At a larger scale, recent HPC-center evidence also shows that GPU capping should be
reasoned about as a system-level control mechanism, not only as a single-device
microbenchmark knob: reduced temperatures and power draw can coexist with modest
performance impact depending on the workload mix and operating point (see [R29] in
References).

## 6.6 GPU telemetry windows and thermal state

The GPU model also distinguishes between:

- **instantaneous internal modeled power**
- **exported averaged power telemetry**

This is important because real GPU telemetry is not always instantaneous. In particular, NVML documents that on many modern NVIDIA GPUs the power reading exposed by `nvmlDeviceGetPowerUsage` is averaged over a 1-second window rather than being a pure instantaneous sample.

Joulie therefore models a GPU telemetry window with:

- `gpu.telemetryWindowMs`

and a thermal state with:

- `gpu.ambientTempC`
- `gpu.thermalTauMs`
- `gpu.wattsPerDeltaC`
- `gpu.thermalThrottleStartC`
- `gpu.thermalThrottleFullC`

Temperature evolves toward a power-dependent equilibrium. Thermal throttling is opt-in, as for CPUs (section 5.5): `gpu.thermalThrottleStartC` and `gpu.thermalThrottleFullC` (`SIM_GPU_THERMAL_THROTTLE_START_C`, `SIM_GPU_THERMAL_THROTTLE_FULL_C`) default to 0, which means off. With the default 24 °C ambient and 12 W/°C, a 750 W board running compute-bound work draws about 720 W and heads for 24 + 720 / 12 = 84 °C, above the former 82 °C default start.

Set both thresholds, as for CPUs. As environment variables one is enough, and the other takes its former default (82 °C start, 92 °C full). As node class keys, a threshold the class leaves unset comes from the base profile. A class full threshold alone then throttles linearly from 0 °C (an idle 30 W board at 26.5 °C has a fraction of 26.5 / 92 = 0.29 with `gpu.thermalThrottleFullC: 92`). A class start threshold alone is rejected as section 5.5 describes, but only for a profile with GPUs, because validation checks the GPU keys only when `gpu.count` is above 0: a class whose profile ends up with GPUs, from its own `gpu.count` or from `SIM_GPU_COUNT`, makes every class in `SIM_NODE_CLASS_CONFIG` ignored. With no GPU count the start threshold is accepted and throttling stays off.

With both thresholds set, the throttle fraction rises linearly between them and scales down the throughput a device reports (the GPU `perfMultiplier` of the telemetry response) when its cap binds. A device running below its cap keeps its full throughput whatever its temperature, because the throughput model returns 1 before it applies the thermal factor. Job progress does not use the GPU fraction yet: the workload engine passes the node's CPU throttle fraction to the GPU throughput model, so GPU thresholds alone never slow a GPU job, and CPU thresholds slow the GPU jobs whose cap binds.

The telemetry window keeps the simulator honest about something that often gets confused in experiments: a telemetry trace can look smoother and slower-moving than the underlying internal power changes because exported power is averaged.

## 6.7 Generic fallback GPU model

When a GPU platform is not represented by a catalog entry, Joulie falls back to the generic GPU model configured through the base profile.

Its board power range is `SIM_GPU_MIN_CAP_WATTS_PER_GPU` to `SIM_GPU_MAX_WATTS_PER_GPU`, 80 to 300 W by default. A catalog GPU uses the same equations with its own board power range from the catalog, unless a node class or a variable sets it (section 3.1). The former `gpu.powerModel` parameters (`alphaUtil`, `betaCap`) are removed: no equation below read them.

### Natural power envelope

The generic GPU model first computes a workload-dependent natural power draw.

For compute-bound workloads:

```text
P_nat = IdleW + (MaxW - IdleW) * util
```

For memory-bound workloads:

```text
P_nat = IdleW + (MaxW - IdleW) * 0.65 * sqrt(util)
```

For mixed workloads, the simulator blends the compute-bound and memory-bound curves.

This captures the idea that memory-bound GPU jobs may draw well below max board power even before capping.

### Cap to power

Requested GPU power cap is then applied as a ceiling, and the board's idle power as a floor:

```text
P_gpu = max(IdleW, min(P_nat, capWattsPerGpu))
```

A requested cap is first clamped to the board power range (`SIM_GPU_MIN_CAP_WATTS_PER_GPU` to `SIM_GPU_MAX_WATTS_PER_GPU`, or the catalog range). The idle floor is the only floor: a power limit caps the active draw and has no deeper idle state, so a board never draws less than `IdleW`, whatever its cap.

### Cap to slowdown

Throughput slowdown is based on the ratio:

```text
ratio = capWattsPerGpu / P_nat
```

If the cap is above the natural envelope, throughput stays at `1.0`.
If the cap is below it:

- `gpu.compute_bound`: throughput drops approximately like `ratio^ComputeGamma`
- `gpu.memory_bound`: throughput drops more gently
- `gpu.mixed`: blend of compute-bound and memory-bound response

So the generic fallback GPU model preserves the same core behavior as the richer catalog-backed model:

- compute-bound workloads are sensitive to caps earlier,
- memory-bound workloads often remain insensitive until caps fall below their natural draw.

## 6.8 Emulated nodes: governor and clock physics

An emulated node ([emulate mode]({{< relref "/docs/simulator/emulate-mode.md" >}})) runs the CPU and GPU governors its profile describes. The rules below apply to a profile with the governor facts, which every builtin profile has; a profile without them uses a simple rule (performance at the maximum, generic powersave at the minimum, any other governor linear in load). The standalone and Kubernetes-mode models of the rest of this page keep the implied performance governor.

### CPU governors

At every 100 ms substep each cpufreq policy's governor sets a frequency (`phys.GovernorStep`, Linux v6.12 rules):

- `performance` holds `scaling_max_freq`; generic `powersave` holds `scaling_min_freq`.
- `schedutil` asks 1.25 x the demand in kHz and takes the lowest table entry at or above it; on a three-entry acpi-cpufreq table a light load sits at the lowest entry.
- `ondemand` jumps to the maximum above `up_threshold`, else runs in proportion to the load; `conservative` steps by `freq_step` per sampling period between `down_threshold` and `up_threshold`; `userspace` holds `scaling_setspeed`.
- `powersave` on intel_pstate with HWP and on amd-pstate-epp is the hardware's own algorithm, shaped by EPP (assumed): `f = min + (max - min) x c(epp) x min(1, u x (1 + eppGain x (255 - epp) / 255))` with a full-load ceiling `c(epp) = max(0.5, 1 - eppCeilingDrop x epp / 128)`. At the shipped EPP 128 a fully loaded CPU runs at 0.8 of its range; EPP 0 reaches the maximum. Without an EPP file it is intel_pstate's own rule, 1.25 x the maximum x the busy share.
- A table driver with boost runs above its top entry, up to `cpuinfo_max_freq`, only when the governor sets that entry.

The governor reads the busy share of the substep that ended, measured at the frequency the CPU ran at, so it reacts one substep late, which stands in for `rate_limit_us`. A new load moves it at once; the running frequency then ramps toward its choice with `freqRampMS`.

**Work in cycles.** A load's CPU utilization is the share of time the work keeps a CPU busy at the frequency performance would run it at under the present limits. At a lower frequency the same work keeps the CPU busy longer: `u_eff = min(1, u x f_performance / f_governor)`. Package power uses `u_eff` in place of `u` (`P = idle + (max - idle) x (u_eff x act)^alpha x f^beta` on an analytic profile; a `catalog-curve` profile, such as the 2x EPYC 9654 ones, reads its measured curve), State reports it as the package utilization, and the governor sees it, so schedutil at a demand of 0.3 on a 2x EPYC 9654 runs at 1.5 GHz with the CPUs 74 % busy. Under performance `u_eff = u`. A binding RAPL or HSMP cap wins over every governor's choice.

**Throughput reference.** A CPU job's speed is `S(f_eff) / S(f_ref)` for its class, where `S` is the knee model of section 5.4, `f_eff` the frequency `scaling_cur_freq` shows (governor, boost and cap included) and `f_ref` what performance runs at with `scaling_max_freq` at `cpuinfo_max_freq` and the profile's boost, whatever the governor. Performance uncapped gives 1; generic powersave at full load on the 9654 gives `S(1.5 GHz) / S(3.7 GHz)`. A governor that runs slower draws less power for longer, so its energy per unit of work can go either way. Whether a fixed job uses less energy under performance (race to idle) depends on the idle share of the power; on the shipped 2x Xeon 8260 profile (idle 40 of 165 W per package, `betaFreq` 1.35) EPP 255 uses about 2 % less energy than performance for a compute-bound job, and with a package idle of 140 W performance wins.

**Idle cost.** `physics.cpu.idleGovernorDeltaW` is added to an idle package (`u_eff` below 0.05) whose governor holds the top frequency; it is 0 in every builtin profile, so idle power is the same under every governor.

### GPU clocks

The AMD and NVIDIA lanes keep each GPU's clock state: the compute ceiling a level, an overdrive range or an `-lgc` lock sets, the data-path level (AMD fclk, socclk and uclk, NVIDIA memory clock) and whether idle gating is off (AMD `profile_*` levels). The physics turns it into power and speed through one clock scale shared with the cap (`phys.GPUPowerWithClock`, `phys.GPUThroughputWithClock`).

The cap and the governor are two limits on one clock. The cap implies a compute clock, the compute scale of the capped board model of section 6.4, `(cap / P_natural)^computeGamma`; the GPU runs at the lower of that clock and the governor's, never at their product. Lowering either limit never raises the power or the throughput of any workload class.

- Below the clock the cap allows, dynamic power scales with `(f_c / f_c,max)^clockBeta`; compute-bound work runs at `f_c / f_c,max`, memory-bound work keeps its speed above `memoryKneeFrac` of the maximum clock and slows as `(f_c / knee)^memoryKneeGamma` below it. A data path forced to its lowest level costs memory-bound work `dataPathGamma` of its speed. On NVIDIA the scale is taken against the sustained maximum, so a lock above it changes nothing.
- An idle GPU (utilization below 0.05) draws `dataPathHoldW` more while its data path is at its maximum, and `gatingOffIdleW` more with idle gating off. Under `auto` the data path rises while the GPU's own memory-active work runs and holds `autoHoldS` after it; the GPU's own work always meets it raised.
- Power is never more than the governor's clock draws without a cap, `idle + (P_natural - idle) x (f_c / f_c,max)^clockBeta`. The two exponents describe one clock when `computeGamma x clockBeta x (1 - idle / max)` is at most 1: then this bound never binds, a lock at the cap-implied clock draws the cap and a lock above it changes nothing. Beyond that the board's cap response would draw more at a clock than the clock curve does, and this bound decides, so a cap never raises the power of a locked GPU.
- The memory share of the throughput is the lower of two limits, the capped board's memory scale and the memory knee at the governor's clock; the compute share is the run clock.
- At the maximum clock, with the data path at its maximum and gating on, power and speed are those of the board model of sections 6.3 and 6.4.

### Preset names

The emulator names the preset a CPU package or a GPU runs (the `joulie.hw.governor.preset` attribute of its [metrics]({{< relref "/docs/simulator/metrics.md#metrics" >}})) with the reverse mapping of `pkg/governor` (`ReverseCPU`, `ReverseAMD`, `ReverseNvidia`), on the facts the agent's discovery reads for that component, so the name is the preset whose plan the agent would write for that state:

- A preset that sets the state as its own wins over one that falls back to it: no NVIDIA lock reads `auto`, also where `sustained` changes nothing.
- A CPU state that no preset, or more than one, maps to has no name, such as a hand-written `ondemand` on a driver that has `schedutil`, which `balanced` picks first.
- An NVIDIA graphics lock from the lowest supported clock up to a supported clock, with no memory lock, reads `efficient` unless a fixed preset sets that same lock (the lowest clock alone is `low`).
- An amdgpu device at `manual` whose overdrive sclk range starts at the table's lowest clock and ends within the table reads `efficient`, whatever the ceiling: on MI300X, `manual` with any ceiling.
- A GPU of an architecture or family without a mapping table reports no preset.

### Peer transfers

GPU to GPU copies of a load share each receiver's `gpus.transfer.peerLinkGBps`; State counts the bytes each GPU received and sent. Copies from a NIC or from host memory, and copies to the host, take their demand. A family can have a receiver gate (`gpus.transfer.receiverGate`), which bounds inbound copies by the receiver's data-path state: a receiver forced to its lowest level, or idle under `auto`, lets through only a fraction of the link, in total over all its sources, whatever the sources do. It is off in every builtin profile; `hwemu.Options.ReceiverGate` can force it on or off.

### Parameters

Every value is assumed and to be calibrated; the sweep ranges are the ones experiments should cover before trusting a sign.

| Fact | Builtin value | Sweep |
|---|---|---|
| `physics.cpu.idleGovernorDeltaW` | 0 W | 0 to 5 % of idle |
| `physics.cpu.eppGain` | 1.0 | 0.5 to 2 |
| `physics.cpu.eppCeilingDrop` | 0.2 | 0 to 0.25 |
| `physics.gpu.clockBeta` | 1.5 | 1.0 to 2.5 |
| `physics.gpu.memoryKneeFrac` | 0.65 | 0.4 to 0.87 |
| `physics.gpu.memoryKneeGamma` | 1.0 | |
| `physics.gpu.dataPathHoldW` | 10 W; 0 W on the H100 profiles, whose HBM runs at one clock at idle | 1 to 50 W |
| `physics.gpu.gatingOffIdleW` | 20 W on AMD, 0 W on NVIDIA, where no preset turns gating off | 5 to 50 W |
| `physics.gpu.dataPathGamma` | 0.3 | |

## 6.9 Governor presets

A frequency governor is a control dimension of its own, next to the power cap. A cap bounds the power a CPU package or a GPU may draw; a governor chooses the frequency the hardware runs at below that bound, from the load it sees. Both act on one clock: the hardware runs at the lower of what the cap allows and what the governor chooses (section 6.8 gives the simulator's form for GPUs, and a binding RAPL cap wins over every CPU governor's choice).

Joulie expresses a governor as one vendor-neutral preset per component in `NodeTwin.spec`, because the native words mean different things on different drivers: `powersave` under `intel_pstate` in active mode is the driver's own P-state algorithm, not the generic governor that holds the minimum frequency, and an NVIDIA GPU has no performance level file at all, only clock locks. `pkg/governor` is the only place a preset becomes a native setting; every other component speaks presets. The mapping per driver is in [CPU support]({{< relref "/docs/hardware/cpus.md#governors" >}}), per GPU family in [GPU support]({{< relref "/docs/hardware/gpus.md#governors" >}}).

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

A demand-driven GPU governor raises the clocks for the device's own kernels. Data another device writes into its memory runs no kernel on it, so the governor can leave the data-path clocks low while the copy runs; `sustained` holds them up and keeps idle gating, at some idle power. Whether those clocks bound a transfer depends on the family, so no default selects `sustained`: an operator opts a GPU model in once a measurement on that model shows the gain.

What Joulie writes, and what it leaves alone:

- CPU: each policy's `scaling_governor`, then `energy_performance_preference` where the driver has one (never under `performance`, where the kernel rejects it), then boost when the spec names it. Never the driver mode (switching it resets every setting), `scaling_min_freq`, `scaling_max_freq` (the DVFS fallback owns it), governor tunables, idle states, the uncore clock, EPB or HSMP.
- AMD GPU: sysfs as root, each write read back, because a tool's exit code proves nothing; `rocm-smi` and `amd-smi` are never used to set a governor.
- NVIDIA GPU: `nvidia-smi -i <pci bus id>` clock locks, and a reset only of a lock the agent set itself; never application clocks (`-ac`, `-rac`) or `--auto-boost-default`.
- GPUs are keyed by PCI bus id, never by a tool's index, because tools enumerate GPUs in different orders.
- A GPU node gets no CPU governor by default (`CPU_GOVERNOR_ON_GPU_NODES`), the rule the CPU cap follows.

Governor control is off by default behind two gates, the controller manager's `GOVERNOR_CONTROL` and the agent's `GOVERNOR_WRITES`; discovery is always on. The agent records the state it found before its first write and restores it when a preset disappears, when writes are turned off, and on exit ([Governor control]({{< relref "/docs/architecture/agent.md#governor-control" >}})). Which node gets which preset is under [Governor policy]({{< relref "/docs/architecture/policy.md#governor-policy" >}}); its defaults are provisional. The values that decide their energy effect (section 6.8, "Parameters") are assumed until a clock sweep on each family measures them; the [governor exploration]({{< relref "/docs/experiments/governor-exploration.md" >}}) studies them in the simulator.

## 7. Workload-boundness and slowdown semantics

The simulator is explicit about the signals that decide whether a workload behaves like a compute-bound, memory-bound, or IO-/feed-limited regime.

Each job can carry:

- `cpuUtilization`
- `gpuUtilization`
- `memoryIntensity`
- `ioIntensity`
- `cpuFeedIntensityGpu`

Those job-level signals are aggregated onto the hosting node and passed into the CPU and GPU physical models.

The practical effect is:

- high CPU utilization with low memory/IO intensity behaves like a compute-bound CPU workload
- high memory intensity softens CPU sensitivity to DVFS/RAPL and reduces switching-activity-driven power growth
- high IO intensity softens CPU slowdown further
- GPU compute-bound jobs lose throughput earlier under power caps
- GPU memory-/bandwidth-bound jobs can remain close to peak throughput until the cap drops below their natural power envelope
- GPU jobs with high CPU feed intensity also slow down when the CPU side is throttled

This is not a full cycle-accurate roofline simulator, but it is a much better approximation than a single "one slowdown curve for everything" model and is directly inspired by roofline-style reasoning about bottlenecks.

That is also why Joulie’s workload generator emits explicit workload signals such as:

- `cpuUtilization`
- `gpuUtilization`
- `memoryIntensity`
- `ioIntensity`
- `cpuFeedIntensityGpu`

rather than only a coarse workload label.

## 8. Heterogeneous nodes and profile semantics

Because the cluster contains devices with very different power ranges, a single absolute cap cannot be applied everywhere.

Joulie therefore treats controller manager intent as **normalized** by default:
- CPU cap as a percentage of the attainable range
- GPU cap as a percentage of the per-device maximum

The simulator and the real agent resolve those normalized targets into absolute caps using node-specific hardware data.

For deterministic experiments, absolute per-device overrides remain possible.

## 9. What the simulator exports

The refined simulator exports:
- node-level CPU and GPU power
- averaged and instantaneous power views
- node-level thermal state and thermal-throttle fractions
- per-device GPU caps
- per-device GPU averaged power and temperature
- effective performance multipliers
- integrated energy over time
- workload-completion statistics under throttling

This allows experiments to compare:
- makespan / completion time
- total energy
- class-specific slowdown
- cap saturation and profile behavior
- the distinction between internal power dynamics and exported averaged telemetry

## 10. Measurement philosophy and realism

Joulie is intentionally trying to sit in the useful middle ground between:

- a toy "utilization in, watts out" simulator, and
- a cycle-accurate architectural simulator.

The design goal is not to reproduce transistor-level behavior. The goal is to reproduce the operational behaviors that matter for cluster control:

- realistic cap ranges
- non-linear load-to-power response
- different slowdown curves for different workload regimes
- non-zero settling time for controls
- thermal effects in sustained runs
- averaged telemetry that resembles what operators really observe

That is why the model blends:

- official vendor/runtime constraints,
- public benchmark priors such as SPECpower,
- and explicit proxy assumptions when exact curves are unavailable.

The practical standard Joulie is aiming for is:

- realistic enough to study control-loop behavior, policy tradeoffs, and heterogeneous-cluster dynamics,
- transparent enough that readers can trace each assumption back to either code, documentation, or published sources,
- and honest enough to say when a result depends on a proxy rather than a direct calibration.

## 10.1 Known caveats

There are a few caveats we want readers to keep in mind when interpreting simulator output or designing calibration protocols:

- **GPU power telemetry is not always instantaneous.** On many modern NVIDIA parts, NVML power usage is averaged over a 1-second interval. The simulator reflects this by exporting an averaged power view alongside the internal instantaneous model.
- **GPU sampling tools can miss short behavior.** Public measurement work has shown that naive `nvidia-smi`-style sampling can miss meaningful parts of runtime on some accelerators. This is one reason the simulator distinguishes between internal dynamics and exported telemetry.
- **CPU package power limits are windowed averages, not hard instantaneous clamps.** The effective result depends on time windows, scheduler callbacks, and platform-specific control loops.
- **Cap dynamics depend on workload and platform generation.** Public RAPL studies show that settling times and frequency/uncore responses differ across systems and between CPU-bound and memory-bound workloads (see [R28] in References).
- **Thermal throttling is workload- and environment-dependent.** The current thermal model is intentionally first-order and useful for cluster-control realism, but it is not a CFD model of the chassis.
- **Emulated overdrive accepts a combined write.** The emulated `pp_od_clk_voltage` takes `s 1 <MHz>` and `c` in one write, one command per line; the kernel takes one command per write and fails that combined write with `EINVAL`. The agent writes each command on its own and waits for the file to print its table again before `c`, so it works on both.
- **Unknown hardware falls back gracefully, but with less realism.** The system still works, but predictions are less trustworthy when the platform is not represented by measured or proxy catalog data.

For this reason, Joulie should be read as:

- highly useful for realistic control-loop experiments,
- increasingly grounded in public measurements and vendor semantics,
- but still designed to be calibrated further on real hardware when those nodes are available.

## 11. Limitations

- GPU behavior is currently validated in **simulation first**; bare-metal GPU calibration is still pending.
- Some CPU and GPU models still rely on proxy curves rather than exact public measured curves.
- Vendor APIs expose min/max cap ranges, but exact internal PMU behavior can still depend on firmware and board design.
- The current thermal model is still a first-order approximation; it does not attempt to model fan curves, hotspot sensors, package-to-package coupling, or chassis airflow in detail.
- The current workload-boundness model is still a mixture-of-experts approximation; it is not yet calibrated from per-application traces for every workload family.
- The governor and clock physics of emulated nodes (section 6.8) uses assumed parameters; no governor or clock sweep has been measured on hardware yet, so the governor presets' energy effects are directions to test, not results.

The implementation is designed so that any proxy can later be replaced with measured curves from bare-metal runs.

## 12. References

The current catalog and model assumptions are grounded in the following concrete sources. Where possible, the list below points to the exact public page, paper, or API reference that informed a catalog entry or modeling choice.

The list is intentionally grouped by role:

- **official references** for limits, APIs, and platform semantics,
- **public measured CPU curves** for direct node-level power priors,
- **research literature** for slowdown, settling-time, and telemetry caveats.

### 12.1 Official hardware and API references

- [R1] NVIDIA H100 NVL product brief, NVIDIA.  
  <https://www.nvidia.com/content/dam/en-zz/Solutions/Data-Center/h100/PB-11773-001_v01.pdf>
- [R2] NVIDIA H100 product page, NVIDIA.  
  <https://www.nvidia.com/en-us/data-center/h100/>
- [R3] NVIDIA L40S product page, NVIDIA.  
  <https://www.nvidia.com/en-us/data-center/l40s/>
- [R4] AMD Instinct MI300X data sheet, AMD.  
  <https://www.amd.com/content/dam/amd/en/documents/instinct-tech-docs/data-sheets/amd-instinct-mi300x-data-sheet.pdf>
- [R5] AMD Radeon PRO W7900 product page, AMD.  
  <https://www.amd.com/en/products/graphics/workstations/radeon-pro/w7900.html>
- [R6] AMD EPYC 9654 product page, AMD.  
  <https://www.amd.com/en/products/processors/server/epyc/4th-generation-9004-and-8004-series/amd-epyc-9654.html>
- [R7] AMD EPYC 9534 product page, AMD.  
  <https://www.amd.com/en/products/processors/server/epyc/4th-generation-9004-and-8004-series/amd-epyc-9534.html>
- [R8] AMD EPYC 9965 product page, AMD.  
  <https://www.amd.com/en/products/processors/server/epyc/5th-generation-epyc/amd-epyc-9965.html>
- [R9] AMD EPYC 9375F product page, AMD.  
  <https://www.amd.com/en/products/processors/server/epyc/5th-generation-epyc/amd-epyc-9375f.html>
- [R10] AMD EPYC 9655 product page, AMD.  
  <https://www.amd.com/en/products/processors/server/epyc/5th-generation-epyc/amd-epyc-9655.html>
- [R11] Intel Xeon Gold 6530 specifications, Intel ARK.  
  <https://www.intel.com/content/www/us/en/products/sku/236251/intel-xeon-gold-6530-processor-160m-cache-2-10-ghz/specifications.html>
- [R12] Linux `amd-pstate` documentation.  
  <https://docs.kernel.org/admin-guide/pm/amd-pstate.html>
- [R13] Linux `intel_pstate` documentation.  
  <https://docs.kernel.org/admin-guide/pm/intel_pstate.html>
- [R14] NVML API reference, power-management/device command section.  
  <https://docs.nvidia.com/deploy/nvml-api/group__nvmlDeviceCommands.html>
- [R15] ROCm SMI power-control documentation.  
  <https://rocm.docs.amd.com/projects/rocm_smi_lib/en/docs-6.1.2/doxygen/html/group__PowerCont.html>
- [R16] AMD EPYC 9004/8004 CPU Power Management white paper.  
  <https://www.amd.com/content/dam/amd/en/documents/products/processors/server/epyc/epyc-8004-and-9004-series-cpu-power-management-white-paper.pdf>
- [R17] Linux `powercap` framework documentation.  
  <https://docs.kernel.org/power/powercap/powercap.html>

### 12.2 Public measured CPU curves

- [R18] SPECpower result for 2x AMD EPYC 9654.  
  <https://www.spec.org/power_ssj2008/results/res2022q4/power_ssj2008-20221204-01203.html>
- [R19] SPECpower result for 2x AMD EPYC 9655.  
  <https://www.spec.org/power_ssj2008/results/res2024q4/power_ssj2008-20240923-01457.html>
- [R20] SPECpower result for 2x AMD EPYC 9965.  
  <https://www.spec.org/power_ssj2008/results/res2025q2/power_ssj2008-20250407-01522.html>
- [R21] SPECpower_ssj2008 results repository index.  
  <https://www.spec.org/power_ssj2008/results/>

These SPECpower pages provide the detailed load-to-power points used for the measured node-level CPU curves currently encoded in the simulator catalog. Each page links the full disclosure (`.txt`), which holds the BIOS and operating system tuning quoted in section 5.1. All three were retrieved on 2026-10-07.

### 12.3 Public GPU power/performance and measurement literature

- [R22] Zeus: Understanding and Optimizing GPU Energy Consumption of DNN Training, NSDI 2023.  
  <https://www.usenix.org/system/files/nsdi23-you.pdf>
- [R23] MI300X power-cap behavior study used as a prior for cap settling and slowdown assumptions.  
  <https://arxiv.org/html/2601.12241v1>
- [R24] GROMACS Unplugged: How Power Capping and Frequency Shapes Performance on GPUs.  
  <https://arxiv.org/pdf/2510.06902>
- [R25] Part-time Power Measurements on GPUs: sampling caveats for `nvidia-smi`-style telemetry.  
  <https://arxiv.org/abs/2502.16242>
- [R26] Roofline: an insightful visual performance model for multicore architectures.  
  <https://dl.acm.org/doi/10.1145/1498765.1498785>
- [R27] NVML API reference, energy-consumption function semantics.  
  <https://docs.nvidia.com/deploy/nvml-api/group__nvmlDeviceQueries.html>
- [R28] Vladimir Ostapenco, Laurent Lefevre, Anne-Cecile Orgerie, Benjamin Fichel. Exploring RAPL as a Power Capping Leverage for Power-Constrained Infrastructures, ICA3PP 2024.  
  <https://perso.ens-lyon.fr/laurent.lefevre/pdf/ICA3PP2024_Ostapenco_Lefevre.pdf>
- [R29] Dan Zhao, Siddharth Samsi, Joseph McDonald, Baolin Li, David Bestor, Michael Jones, Devesh Tiwari, Vijay Gadepally. Sustainable Supercomputing for AI: GPU Power Capping at HPC Scale, arXiv 2024.  
  <https://arxiv.org/abs/2402.18593>

These GPU references are used as modeling priors for:

- cap-to-throughput sensitivity,
- compute-bound vs memory-bound differentiation,
- non-zero control settling time after large cap changes,
- averaged-vs-instantaneous telemetry caveats,
- and the roofline-style intuition behind workload-boundness classification.

They also anchor two important realism checks for Joulie:

- short-timescale control/measurement caveats from device-level studies such as FinGraV and Part-time Power Measurements,
- and cluster-level interpretation of power-capping effects from RAPL and HPC-scale GPU studies (see [R28] and [R29] in References).

These references define a mix of:

- official/runtime-exact values,
- public measured curves,
- and family-level proxy assumptions where exact measured curves are not yet available.
