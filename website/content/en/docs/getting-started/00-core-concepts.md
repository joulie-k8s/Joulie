+++
title = "Core Concepts"
linkTitle = "Core Concepts"
slug = "core-concepts"
weight = 1
+++

Before installing Joulie, understand the control model.

## Problem Joulie addresses

Clusters running AI/scientific workloads need better power control:

- reduce energy use and power spikes,
- keep workload performance predictable,
- provide a path to greener operation (power envelope and carbon-aware strategies).

Joulie is currently a PoC focused on Kubernetes-native control loops and simulation.

## Main components

- **Operator** (`cmd/operator`): cluster-level policy brain
  - decides desired node power profile/cap assignments
  - writes desired state as `NodePowerProfile`
- **Agent** (`cmd/agent`): node-level actuator
  - reads desired state and telemetry configuration
  - enforces power controls (CPU + GPU)
  - exports metrics/status
- **Simulator** (`simulator/`): digital-twin execution environment
  - keeps scheduling real, simulates telemetry/control behavior
  - enables repeatable experiments without requiring real hardware writes

## Key CRDs

- `NodePowerProfile` (`joulie.io/v1alpha1`)
  - desired node policy state (`performance` / `eco`, optional power cap)
- `TelemetryProfile` (`joulie.io/v1alpha1`)
  - where telemetry/control inputs come from (`host`, `http`, ...), and how controls are sent

## Policy states and intent

Node supply is represented through `joulie.io/power-profile`:

- `performance`
- `eco`

Transition state is exposed independently through:

- `joulie.io/draining=true|false`

Workload demand is inferred from pod scheduling constraints:

- workload constrained to performance nodes
- workload constrained to eco nodes
- unconstrained workload (can run on either)

## Energy policy in one paragraph

An energy policy decides how many nodes should stay in `performance` or move to `eco`, based on current demand and configured rules.
Today Joulie ships deterministic policies (static and queue-aware), plus a debug swap policy.
Policy algorithms are detailed in [Policy Algorithms]({{< relref "/docs/architecture/policies.md" >}}).

## Control loop in one minute

1. Operator observes cluster context and picks desired node states.
2. Operator writes/updates `NodePowerProfile`.
3. Agent reads desired state + telemetry/control profile.
4. Agent applies controls and reports status/metrics.
5. Operator reconciles again.

## Next step

Proceed to [Quickstart]({{< relref "/docs/getting-started/01-quickstart.md" >}}), then use Architecture pages for deeper details.
