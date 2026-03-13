---
title: "CPU Support and Power Capping"
weight: 5
---

Joulie supports node-level CPU power capping through `NodePowerProfile` intents enforced by the agent.

## Contract model

CPU intent is defined in `NodePowerProfile.spec.cpu`:

- `packagePowerCapWatts` (optional absolute cap)
- `packagePowerCapPctOfMax` (optional normalized profile intent)

Precedence:

1. `packagePowerCapWatts` if present
2. otherwise `packagePowerCapPctOfMax`

## Policy behavior

Operator profile assignment remains `performance` vs `eco`.
CPU cap values are generated per profile and written into `NodePowerProfile`:

- performance profile typically maps to a higher cap (often 100%)
- eco profile maps to a lower cap

For heterogeneous nodes, percentage-based intent remains useful because each node resolves normalized intent using node-local capabilities.
If percentage intent cannot be converted to watts (for example missing RAPL range), the agent applies a DVFS percent fallback path when possible.

## Driver/back-end semantics

Joulie is designed around standard Linux CPU power/frequency controls:

- AMD platforms: `amd-pstate`/CPPC semantics
- Intel platforms: `intel_pstate` semantics

In simulator mode, CPU dynamics include utilization-dependent power curves, cap effects, and DVFS settling behavior.
They now also include:

- CPU cap application settling (`cpuCapApplyTauMs`)
- exported telemetry averaging (`cpuTelemetryWindowMs`)
- first-order thermal behavior and thermal-throttle thresholds
- workload-sensitive slowdown based on `memoryIntensity` and `ioIntensity`

## Measured vs proxy models

Joulie distinguishes:

- exact measured curves (for selected inventory-backed CPU platforms, e.g. SPECpower-backed)
- proxy/inferred curves where direct public measured curves are unavailable

See [Hardware Modeling]({{< relref "/docs/hardware/hardware-modeling.md" >}}) for full model provenance and references.

## Scheduling guidance

Workload intent classification is based on node selector/affinity using `joulie.io/power-profile`:

- performance-sensitive workloads: typically exclude `eco`
- eco-only workloads: explicitly require `eco`

This classification drives operator profile assignment; CPU capping is then enforced through `NodePowerProfile`.

## Related docs

- [CRD and Policy Model]({{< relref "/docs/architecture/policy.md" >}})
- [Joulie Agent]({{< relref "/docs/architecture/agent.md" >}})
- [Hardware Modeling]({{< relref "/docs/hardware/hardware-modeling.md" >}})
