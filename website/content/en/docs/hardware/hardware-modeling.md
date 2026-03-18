---
title: "Hardware Modeling and Physical Power Model"
weight: 30
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
- min/max/default power-cap ranges queried at runtime
- supported clocks or performance landmarks exposed by the driver

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

The inventory is now **compositional** rather than node-class-only.

It is organized around reusable:

- CPU model entries
- GPU model entries

Nodes are then understood as:

- discovered CPU model + count
- discovered GPU model + count

This is used consistently by:

- the agent, which discovers raw hardware facts,
- the operator, which resolves those facts against the inventory,
- the simulator, which composes node models from the same CPU/GPU entries.

In the codebase, the shared inventory shape lives in:

- `pkg/hwinv/catalog.go`
- embedded catalog data under `pkg/hwinv/assets/hardware.yaml`

The simulator also keeps its source catalog in:

- `simulator/catalog/hardware.yaml`

Matching is alias-based and normalization-based:

- raw model names discovered by the agent or provided by simulator labels are normalized,
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

If the platform is not in the inventory described on this page, the system still works.
What changes is the level of realism and how much hardware-specific information is available:

- no catalog-backed exact CPU curve,
- no catalog-backed official/proxy provenance for that platform,
- no catalog-backed GPU board-power metadata,
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
- **NVIDIA H100 NVL**: max TDP 400 W.
- **NVIDIA H100 SXM**: up to 700 W configurable.
- **NVIDIA L40S**: max power consumption 350 W.
- **AMD Instinct MI300X**: maximum TBP 750 W.
- **AMD Radeon Pro W7900**: total board power 295 W.

## 5. CPU physical model

At the CPU side, Joulie combines three layers:

- measured node-level curves where available,
- family-level proxy curves where necessary,
- and a generic fallback model when no better prior exists.

## 5.1 Exact measured node curves where available

For these catalog-backed platforms, Joulie uses public SPECpower measurements directly as the default node-level CPU power curve:

### 2x AMD EPYC 9654
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

### 2x AMD EPYC 9655
- idle: 138 W
- 10%: 297 W
- 20%: 367 W
- 30%: 438 W
- 40%: 515 W
- 50%: 593 W
- 60%: 661 W
- 70%: 710 W
- 80%: 771 W
- 90%: 812 W
- 100%: 861 W

### 2x AMD EPYC 9965
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

## 5.3 DVFS and power-cap semantics

The CPU control path matters because the simulator is trying to mirror the same qualitative behavior that the real agent/operator path expects:

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

## 5.4 CPU slowdown model
CPU slowdown depends on workload class:

- **compute-bound**: throughput tracks the effective performance/frequency state closely
- **memory-bound**: throughput degrades more slowly until the requested point crosses the non-linear knee
- **mixed**: weighted blend

This means the same DVFS/RAPL action can produce different throughput changes depending on workload characteristics.

## 5.5 CPU control dynamics, telemetry windows, and thermal state

Joulie now treats CPU control as a **time-dependent loop**, not as an instantaneous jump from one idealized state to another.

### Cap application dynamics

Requested CPU package caps settle toward their target with a first-order time constant:

```text
Cap_applied(t + dt) = Cap_applied(t) + (Cap_target - Cap_applied) * dt / tau_cap
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
T(t + dt) = T(t) + (T_target - T(t)) * dt / tau_thermal
```

where:

- `K_cpu` is represented by `cpuWattsPerDeltaC`
- `tau_thermal` is represented by `cpuThermalTauMs`

Once temperature crosses the configured thermal thresholds:

- `cpuThermalThrottleStartC`
- `cpuThermalThrottleFullC`

the simulator applies a thermal-throttle fraction that reduces the effective throughput multiplier.

This gives a more realistic distinction between:

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
- `PMaxW` is uncapped max CPU power
- `AlphaUtil` controls how aggressively power rises with utilization
- `BetaFreq` controls how aggressively power rises with frequency/performance state

This is the baseline physical model used for generic hardware.
In practice, it is most visible in the simulator, but it also captures the same high-level assumption the agent relies on: lower effective performance state generally means lower power and lower throughput.

### Power-cap handling

If simulated package power exceeds the requested CPU cap:

1. the simulator solves for a lower `freqScale` that satisfies the cap,
2. clamps that frequency to the minimum modeled frequency ratio,
3. recomputes power with the reduced `freqScale`,
4. marks the node as cap-saturated when even minimum frequency cannot respect the requested cap.

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

## 5.7 CPU idle power management (C-states)

On managed nodes (where the agent has set a DVFS throttle), the simulator models aggressive CPU C-state entry (C6/C10) at low utilization.

When a managed node has CPU utilization below 20%, the simulator reduces idle power by up to 50%, scaling linearly with the idle fraction:

```text
if throttlePct > 0 and util < 0.20:
    idleFrac = 1 - util / 0.20
    cstateReduction = 0.50 * idleFrac
    P_cpu = P_cpu * (1 - cstateReduction)
    P_cpu = max(20, P_cpu)
```

This models the real hardware behavior where cores on lightly-loaded nodes can enter deep sleep states (C6, C10) that significantly reduce package power. The key distinction is that only Joulie-managed nodes benefit from this: the agent's DVFS policy enables the OS to aggressively enter deep C-states, while unmanaged nodes (baseline without Joulie) remain at full idle power because their power governor is not optimized for idle efficiency.

The 20W floor prevents unrealistically low power values. In practice, even in the deepest sleep state, platform power (memory controllers, PCH, fans) prevents the package from reaching zero.

## 6. GPU physical model

At the GPU side, Joulie models both:

- how much power a workload would naturally like to consume,
- and how much throughput it loses when a power cap forces it away from that natural operating point.

## 6.1 Per-device, not only per-node
Joulie models GPUs **per physical GPU device** on the node.
A node-level "GPU profile" is implemented by applying the same per-device power cap to every GPU on the node.

This is the simplest robust strategy for heterogeneous clusters and matches the underlying vendor APIs well.

## 6.2 Vendor APIs used by the real agent

### NVIDIA
Joulie uses NVML semantics for:
- querying min/max/default power limits,
- applying a new per-device power limit,
- reading current power and (optionally) clocks.

### AMD
Joulie uses ROCm SMI / AMD SMI semantics for:
- querying power-cap ranges,
- applying per-device power caps,
- reading current power and related telemetry.

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

Temperature evolves toward a power-dependent equilibrium, and once the modeled GPU temperature crosses the configured thresholds the simulator applies an additional thermal-throttle factor to throughput.

This keeps the simulator honest about two things that often get confused in experiments:

- a device can be *within cap* and still lose performance because of thermal pressure
- a telemetry trace can look smoother and slower-moving than the underlying internal power changes because exported power is averaged

## 6.7 GPU idle power management (D3cold)

On managed eco-profile nodes, the simulator models GPU deep idle power states (D3cold via NVML).

When a GPU device is managed (power cap set below maximum) and has no active work (utilization near zero), the simulator reduces idle power by 90%:

```text
if capWatts < maxCapWatts * 0.99 and utilization < 0.01:
    idleFloor = idleWattsPerGPU * 0.10
```

This models the real hardware behavior where the agent can activate D3cold or similar deep power states on GPUs that have no active work. On a multi-GPU node where only some GPUs are active, the idle GPUs can enter deep sleep while the active ones continue at full power.

The simulator also distributes node-average GPU utilization across individual devices rather than applying a uniform average to all GPUs. Active GPUs run at their true utilization while idle GPUs sit at zero. This correctly models per-device idle power management and prevents the average-utilization approach from underestimating the savings on nodes with mixed active/idle GPUs.

Unmanaged GPUs (cap equal to maximum, i.e., no Joulie control) remain at their full idle power level.

## 6.8 Generic fallback GPU model

When a GPU platform is not represented by a catalog entry, Joulie falls back to the generic GPU model configured through the base profile.

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

Requested GPU power cap is then applied as a ceiling:

```text
P_gpu = min(P_nat, capWattsPerGpu)
```

with clamping to modeled minimum/maximum device limits.

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

## 7. Workload-boundness and slowdown semantics

The simulator is now explicit about the signals that decide whether a workload behaves like a compute-bound, memory-bound, or IO-/feed-limited regime.

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

That is also why Joulie’s workload generator now emits explicit workload signals such as:

- `cpuUtilization`
- `gpuUtilization`
- `memoryIntensity`
- `ioIntensity`
- `cpuFeedIntensityGpu`

rather than only a coarse workload label.

## 8. Heterogeneous nodes and profile semantics

Because the cluster contains devices with very different power ranges, a single absolute cap cannot be applied everywhere.

Joulie therefore treats operator intent as **normalized** by default:
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
  <https://www.spec.org/power_ssj2008/results/res2024q4/power_ssj2008-20241007-01464.html>
- [R20] SPECpower result for 2x AMD EPYC 9965.  
  <https://www.spec.org/power_ssj2008/results/res2025q2/power_ssj2008-20250407-01522.html>
- [R21] SPECpower_ssj2008 results repository index.  
  <https://www.spec.org/power_ssj2008/results/>

These SPECpower pages provide the detailed load-to-power points used for the measured node-level CPU curves currently encoded in the simulator catalog.

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
