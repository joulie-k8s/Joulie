---
title: "CPU Support and Power Capping"
weight: 5
---

Joulie supports node-level CPU power capping through `NodeTwin` intents enforced by the agent.

## Contract model

CPU intent is defined in `NodeTwin.spec.cpu`:

- `packagePowerCapWatts` (optional absolute cap)
- `packagePowerCapPctOfMax` (optional normalized profile intent)

Precedence:

1. `packagePowerCapWatts` if present
2. otherwise `packagePowerCapPctOfMax`

## Policy behavior

Controller manager profile assignment remains `performance` vs `eco`.
CPU cap values are generated per profile and written into `NodeTwin.spec`:

- performance profile typically maps to a higher cap (often 100%)
- eco profile maps to a lower cap

For heterogeneous nodes, percentage-based intent remains useful because each node resolves normalized intent using node-local capabilities.
If percentage intent cannot be converted to watts (for example missing RAPL range), the agent applies a DVFS percent fallback path when possible.

## Driver/back-end semantics

Joulie is designed around standard Linux CPU power/frequency controls:

- AMD platforms: `amd-pstate`/CPPC semantics
- Intel platforms: `intel_pstate` semantics

In simulator mode, CPU dynamics include utilization-dependent power curves, cap effects, DVFS settling behavior, and:

- CPU cap application settling (`cpuCapApplyTauMs`)
- exported telemetry averaging (`cpuTelemetryWindowMs`)
- first-order thermal behavior and thermal-throttle thresholds
- workload-sensitive slowdown based on `memoryIntensity` and `ioIntensity`

## Governors

Next to the cap, the agent can set the CPU frequency governor of a node from the preset in `NodeTwin.spec.cpu.governor` (`performance`, `balanced` or `efficiency`, and optionally `boost` `on` or `off`). It is off by default; see [Governor presets]({{< relref "/docs/hardware/hardware-modeling.md#69-governor-presets" >}}) for what the presets mean and [Governor control]({{< relref "/docs/architecture/agent.md#governor-control" >}}) for how the agent writes and restores them.

The same preset goes to every cpufreq policy of the node. `pkg/governor` maps it to the native words of the node's driver:

| Driver (mode) | `performance` | `balanced` | `efficiency` |
|---|---|---|---|
| `intel_pstate` (active) with HWP | `performance` | `powersave`, EPP `balance_performance` | `powersave`, EPP `balance_power` |
| `intel_pstate` (active) without HWP | `performance` | `powersave` | `powersave` (`equivalent`: no EPP to bias) |
| `amd-pstate-epp` (active) | `performance` | `powersave`, EPP `balance_performance` | `powersave`, EPP `balance_power` |
| `intel_cpufreq` (intel_pstate passive) with HWP | `performance` | `schedutil`, EPP `balance_performance` | `conservative`, EPP `balance_power` |
| `intel_cpufreq` without HWP, `acpi-cpufreq`, `amd-pstate` (passive, guided), `cppc_cpufreq` and other generic drivers | `performance` | `schedutil` | `conservative` |
| no cpufreq (as on many VMs) | not listed | not listed | not listed |

Rules:

- A word is used only when `scaling_available_governors` (or `energy_performance_available_preferences`) lists it. `balanced` falls back to `ondemand` and `efficiency` to `schedutil`, and the result is then `equivalent` instead of `applied`; a preset with no usable word is left out of the node's `NodeHardware` `status.cpu.governor.presets`. One exception: under `performance`, amd-pstate-epp lists `performance` alone as an available preference, so there the agent takes the driver's full set (`default`, `performance`, `balance_performance`, `balance_power`, `power`, which it lists under `powersave`), and a preset's result does not change with the governor the node runs.
- Under `performance` no EPP is written: the kernel rejects a non-zero preference there.
- Boost is written only when the spec names it: the inverted `intel_pstate/no_turbo` on `intel_pstate` and `intel_cpufreq`, each policy's `boost` on `amd-pstate` and `amd-pstate-epp`, the global `cpufreq/boost` elsewhere, or each policy's `boost` where there is no global file. A node with none of them cannot take a `boost` word.
- On `acpi-cpufreq`, where the global `cpufreq/boost` and each `policyN/boost` both exist, every policy file reads 0 at boot while `cpufreq/boost` reads 1 and the CPUs boost: a policy file shows the core's per-policy flag, not the boost in effect. A policy write equal to what the file shows changes nothing, and a global write of a new state sets every policy. The agent writes the global file there.
- On drivers without an EPP file, `balanced` and `efficiency` differ only by `schedutil` against `conservative`.
- Joulie never writes the driver mode, `scaling_min_freq`, governor tunables, idle states, the uncore clock, EPB or HSMP. `scaling_max_freq` belongs to the DVFS fallback, which works under any governor.
- A GPU node gets no CPU governor unless the controller manager's `CPU_GOVERNOR_ON_GPU_NODES` is `true`, the rule the CPU cap follows.

The mapping is tested against emulated nodes of each driver (`simulator/pkg/hwemu` profiles for `acpi-cpufreq`, `intel_cpufreq`, `intel_pstate` with HWP and `amd-pstate-epp`), not yet on hardware.

## Measured vs proxy models

Joulie distinguishes:

- exact measured curves (for selected inventory-backed CPU platforms, e.g. SPECpower-backed)
- proxy/inferred curves where direct public measured curves are unavailable

See [Hardware Modeling]({{< relref "/docs/hardware/hardware-modeling.md" >}}) for full model provenance and references.

## Scheduling guidance

Workload placement intent is expressed via the `joulie.io/workload-class` pod annotation:

- `performance`: must run on full-power nodes (extender hard-rejects eco nodes)
- `standard`: default, can run on any node; adaptive scoring steers toward eco when performance nodes are congested

The scheduler extender enforces these classes; CPU capping is then enforced through `NodeTwin.spec`.

## Related docs

- [CRD and Policy Model]({{< relref "/docs/architecture/policy.md" >}})
- [Joulie Agent]({{< relref "/docs/architecture/agent.md" >}})
- [Configuration Reference]({{< relref "/docs/getting-started/05-configuration-reference.md#governor-control" >}}): the governor variables
- [Hardware Modeling]({{< relref "/docs/hardware/hardware-modeling.md" >}})
