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

> **Validation status**
>
> - CPU models already use public measured curves for some node classes.
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

## 3. Hardware inventory currently modeled

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

## 3.1 What happens for hardware not in the inventory

Joulie does **not** require every simulated platform to be present in the catalog.

Today, the simulator resolves hardware in this order:

1. start from a generic base model loaded from simulator env vars,
2. apply any matching node-class overrides from `SIM_NODE_CLASS_CONFIG`,
3. enrich the resulting model from the hardware catalog when the node class exists in `SIM_HARDWARE_CATALOG_PATH`.

If the platform is not in the inventory described on this page, the system still works.
What changes is the level of realism and how much hardware-specific information is available:

- no catalog-backed exact CPU curve,
- no catalog-backed official/proxy provenance for that node class,
- no catalog-backed GPU board-power metadata,
- therefore more reliance on the generic fallback model and any explicit overrides you provide.

So an unknown platform is treated as a **generic or manually overridden platform**, not as a calibrated inventory entry.

For the real agent, that means:

- control backends still try to use runtime-observed limits from the node,
- but there may be less model-specific information available for normalized cap resolution or for simulator-style deterministic experiments.

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

## 5.1 Exact measured node curves where available

For these node classes, Joulie uses public SPECpower measurements directly as the default node-level CPU power curve:

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

## 5.5 Generic fallback CPU model

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

## 6. GPU physical model

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

## 6.6 Generic fallback GPU model

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

## 7. Heterogeneous nodes and profile semantics

Because the cluster contains devices with very different power ranges, a single absolute cap cannot be applied everywhere.

Joulie therefore treats operator intent as **normalized** by default:
- CPU cap as a percentage of the attainable range
- GPU cap as a percentage of the per-device maximum

The simulator and the real agent resolve those normalized targets into absolute caps using node-specific hardware data.

For deterministic experiments, absolute per-device overrides remain possible.

## 8. What the simulator exports

The refined simulator exports:
- node-level CPU and GPU power
- per-device GPU caps
- effective performance multipliers
- integrated energy over time
- workload-completion statistics under throttling

This allows experiments to compare:
- makespan / completion time
- total energy
- class-specific slowdown
- cap saturation and profile behavior

## 9. Limitations

- GPU behavior is currently validated in **simulation first**; bare-metal GPU calibration is still pending.
- Some CPU and GPU models still rely on proxy curves rather than exact public measured curves.
- Vendor APIs expose min/max cap ranges, but exact internal PMU behavior can still depend on firmware and board design.

The implementation is designed so that any proxy can later be replaced with measured curves from bare-metal runs.
