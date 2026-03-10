---
title: "CRD and Policy Model"
weight: 10
---

This page defines Joulie's core contract:

- **demand** comes from pod scheduling constraints,
- **supply** is exposed by node power-profile labels,
- **desired state** is published through `NodePowerProfile`.

## APIs

Group/version:

- `joulie.io/v1alpha1`

CRDs:

- `NodePowerProfile` (`nodepowerprofiles`, cluster-scoped)
- `TelemetryProfile` (`telemetryprofiles`, cluster-scoped)

CRD definitions live in:

- `config/crd/bases/joulie.io_nodepowerprofiles.yaml`
- `config/crd/bases/joulie.io_telemetryprofiles.yaml`

## Demand model (workloads)

Workload class is inferred from Kubernetes scheduling constraints on key:

- `joulie.io/power-profile`

Classification:

- `performance` demand:
  - pod requires `joulie.io/power-profile=performance`
- `eco` demand:
  - pod requires `joulie.io/power-profile=eco`
- `general` demand:
  - no explicit power-profile requirement (unconstrained)

Classification source is affinity/selector, not a custom intent label.

## Supply model (nodes)

Node supply is represented by label:

- `joulie.io/power-profile=performance|draining-performance|eco`

Semantics:

- `performance`: full-performance supply
- `draining-performance`: temporary transition supply during safe downgrade
- `eco`: low-power supply

## Desired-state object: `NodePowerProfile`

`NodePowerProfile` is the operator-to-agent contract for one node.

Main fields:

- `spec.nodeName` (required)
- `spec.profile` (required, `performance|eco`)
- `spec.cpu.packagePowerCapWatts` (optional)
- `spec.policy.name` (optional, provenance/debug)

Example:

```yaml
apiVersion: joulie.io/v1alpha1
kind: NodePowerProfile
metadata:
  name: node-worker-01
spec:
  nodeName: worker-01
  profile: eco
  cpu:
    packagePowerCapWatts: 180
  policy:
    name: static_partition
```

## Telemetry/control routing: `TelemetryProfile`

`TelemetryProfile` defines how the agent reads telemetry and sends controls (`host`, `http`, ...).

In short:

- `NodePowerProfile` = what target a node should have
- `TelemetryProfile` = how telemetry/control IO is wired

Details are in [Input Telemetry and Actuation Interfaces]({{< relref "/docs/architecture/telemetry.md" >}}).

## End-to-end contract flow

1. User submits workload with Kubernetes scheduling constraints.
2. Scheduler places pods according to available node labels.
3. Operator observes demand/supply and computes new node targets.
4. Operator writes `NodePowerProfile` and updates node supply labels.
5. Agent enforces controls and reports status/metrics.

## Next step

Read [Joulie Operator]({{< relref "/docs/architecture/operator.md" >}}) for reconcile behavior and transition guards.
