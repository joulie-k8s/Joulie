---
title: "CPU Support and Power Capping"
weight: 5
---

How Joulie caps the CPU packages of a node and sets their frequency governor, through `NodeTwin` intents the agent enforces.

## Contract model

CPU intent is in `NodeTwin.spec.cpu`. The first field present wins:

1. `packagePowerCapWatts`: an absolute cap.
2. `packagePowerCapPctOfMax`: a percentage of the node's cap range.

## Policy behavior

| Profile | CPU cap | CPU governor on a CPU-only node, when governor control is on |
|---|---|---|
| `performance` (or draining) | high, often 100 % | `CPU_GOVERNOR_PERFORMANCE_PRESET`, default `performance` |
| `eco` | lower | `CPU_GOVERNOR_ECO_PRESET`, default `efficiency` (provisional) |

See [Governor policy]({{< relref "/docs/architecture/policy.md#governor-policy" >}}).

A percentage resolves against the node's own range, so one profile fits heterogeneous nodes:

| Node | Range a percentage resolves against |
|---|---|
| with RAPL files | the package cap range the files report, clamped to it |
| without RAPL files, capped through an HTTP control backend (the simulator's KWOK nodes) | the hardware catalog's range for its CPU, times its sockets: TDP at the top, 0.55 x TDP at the bottom per socket |
| neither | the DVFS percent fallback, when possible ([CPU enforcement algorithm]({{< relref "/docs/architecture/agent.md#cpu-enforcement-algorithm" >}})) |

On two AMD EPYC 9654 sockets (TDP 360 W) a 95 % cap is 684 W and a 60 % cap 432 W. A cap the CPU cannot reach holds it at its lowest-frequency power. Why the floor is 0.55 x TDP and not the cTDP range is on the wiki's [CPU power model](https://github.com/joulie-k8s/Joulie/wiki/CPU-Power-Model#the-cap-range-and-the-055-x-tdp-floor).

## Driver/back-end semantics

| Platform | Linux interface | Simulator counterpart |
|---|---|---|
| AMD | `amd-pstate` (CPPC) | continuous frequency scale, cap settling (`cpuCapApplyTauMs`) |
| Intel | `intel_pstate` | as AMD |
| any | RAPL package limits | averaged telemetry (`cpuTelemetryWindowMs`), first-order thermal model with opt-in throttling, slowdown that depends on `memoryIntensity` and `ioIntensity` |

## Governors

Next to the cap, the agent can set every cpufreq policy of a node from the preset in `NodeTwin.spec.cpu.governor` (`performance`, `balanced` or `efficiency`, and optionally `boost` `on` or `off`). It is off by default; see [Governor presets]({{< relref "/docs/hardware/hardware-modeling.md#69-governor-presets" >}}) for what the presets mean and [Governor control]({{< relref "/docs/architecture/agent.md#governor-control" >}}) for how the agent writes and restores them. `pkg/governor` maps a preset to the node's driver:

| Driver (mode) | `performance` | `balanced` | `efficiency` |
|---|---|---|---|
| `intel_pstate` (active) with HWP | `performance` | `powersave`, EPP `balance_performance` | `powersave`, EPP `balance_power` |
| `intel_pstate` (active) without HWP | `performance` | `powersave` | `powersave` (`equivalent`: no EPP to bias) |
| `amd-pstate-epp` (active) | `performance` | `powersave`, EPP `balance_performance` | `powersave`, EPP `balance_power` |
| `intel_cpufreq` (intel_pstate passive) with HWP | `performance` | `schedutil`, EPP `balance_performance` | `conservative`, EPP `balance_power` |
| `intel_cpufreq` without HWP, `acpi-cpufreq`, `amd-pstate` (passive, guided), `cppc_cpufreq` and other generic drivers | `performance` | `schedutil` | `conservative` |
| no cpufreq (as on many VMs) | not listed | not listed | not listed |

- A word is used only when the driver lists it; otherwise a fallback gives `equivalent`, or the preset is not listed.
- No EPP is written under `performance`, and boost only when the spec names it.
- Joulie never writes the driver mode, `scaling_min_freq`, governor tunables, idle states, the uncore clock, EPB or HSMP; `scaling_max_freq` belongs to the DVFS fallback.
- A GPU node gets no CPU governor unless `CPU_GOVERNOR_ON_GPU_NODES` is `true`.
- The mapping is tested against emulated nodes of each driver, not yet on hardware.

## Measured vs proxy models

Some catalog CPUs run a measured SPECpower curve; the others, proxies included, run an analytic model.
See [Hardware Modeling]({{< relref "/docs/hardware/hardware-modeling.md" >}}).

## Scheduling guidance

Placement intent is the `joulie.io/workload-class` pod annotation, which the scheduler extender enforces:

| Class | Placement |
|---|---|
| `performance` | full-power nodes only; the extender rejects eco nodes |
| `standard` (default) | any node; adaptive scoring steers toward eco when performance nodes are congested |

## Related docs

- [CRD and Policy Model]({{< relref "/docs/architecture/policy.md" >}})
- [Joulie Agent]({{< relref "/docs/architecture/agent.md" >}})
- [Configuration Reference]({{< relref "/docs/getting-started/05-configuration-reference.md#governor-control" >}}): the governor variables
- [Hardware Modeling]({{< relref "/docs/hardware/hardware-modeling.md" >}})

The fallbacks, the boost files per driver and the amd-pstate-epp exception: [Governors](https://github.com/joulie-k8s/Joulie/wiki/Governors#cpu-native-settings).
