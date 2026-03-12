---
title: "CPU Support and Power Capping"
weight: 5
---

Joulie supports node-level CPU power capping through `NodePowerProfile` intents enforced by the agent.

## Contract model

CPU intent is defined in `NodePowerProfile.spec.cpu`:

- `packagePowerCapPctOfMax` (preferred, normalized profile intent)
- `packagePowerCapWatts` (optional absolute override)

Precedence:

1. `packagePowerCapPctOfMax` for heterogeneous fleets
2. `packagePowerCapWatts` when explicit absolute capping is required

## Policy behavior

Operator profile assignment remains `performance` vs `eco`.
CPU cap values are generated per profile and written into `NodePowerProfile`:

- performance profile typically maps to a higher cap (often 100%)
- eco profile maps to a lower cap

For heterogeneous nodes, percentage-based intent is recommended so each node resolves caps according to its own hardware limits.

## Driver/back-end semantics

Joulie is designed around standard Linux CPU power/frequency controls:

- AMD platforms: `amd-pstate`/CPPC semantics
- Intel platforms: `intel_pstate` semantics

In simulator mode, CPU dynamics include utilization-dependent power curves, cap effects, and DVFS settling behavior.

## Measured vs proxy models

Joulie distinguishes:

- exact measured curves (for selected CPU node classes, e.g. SPECpower-backed)
- proxy/inferred curves where direct public measured curves are unavailable

See [Hardware Modeling]({{< relref "/docs/simulator/hardware-modeling.md" >}}) for full model provenance and references.

## Scheduling guidance

Workload intent classification is based on node selector/affinity using `joulie.io/power-profile`:

- performance-sensitive workloads: typically exclude `eco`
- eco-only workloads: explicitly require `eco`

This classification drives operator profile assignment; CPU capping is then enforced through `NodePowerProfile`.

## Related docs

- [CRD and Policy Model]({{< relref "/docs/architecture/policy.md" >}})
- [Joulie Agent]({{< relref "/docs/architecture/agent.md" >}})
- [Hardware Modeling]({{< relref "/docs/simulator/hardware-modeling.md" >}})
