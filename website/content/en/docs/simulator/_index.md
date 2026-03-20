---
title: "Simulator"
linkTitle: "Simulator"
weight: 30
---

Joulie simulator provides a digital-twin path for controlled evaluation.

It keeps scheduling behavior real while simulating telemetry/control dynamics and workload progression.

The simulator mirrors the real Joulie architecture. The operator, agent, and scheduler extender are the real components running against simulated hardware:
- the real operator computes desired state (`NodeTwin.spec`, `NodeTwin.status`),
- the real agent (pool mode) realizes caps via HTTP against the simulator,
- the real scheduler extender reads `NodeTwin.status` and applies workload-class-aware scoring,
- workloads carry trace-defined fields (criticality, cap sensitivity),
- facility stress model (`simulator/pkg/facility`) provides PSU and cooling stress signals.

The heterogeneous benchmark (`experiments/02-heterogeneous-benchmark/`) demonstrates the full architecture across three baselines: no Joulie, static partition, and queue-aware policy with scheduler extender steering.

Use simulator docs after you are familiar with the core operator/agent control loop in Getting Started + Architecture.

## Read in this order

1. [Workload and Power Simulator]({{< relref "/docs/simulator/simulator.md" >}})
2. [Workload Generation]({{< relref "/docs/simulator/workload-generation.md" >}})
3. [Workload Distributions]({{< relref "/docs/simulator/workload-distributions.md" >}})
4. [Kubernetes AI Workloads]({{< relref "/docs/simulator/kubernetes-ai-workloads.md" >}})
5. [Workload Simulator]({{< relref "/docs/simulator/workload-simulator.md" >}})
6. [Power Simulator]({{< relref "/docs/simulator/power-simulator.md" >}})
7. [Hardware Modeling]({{< relref "/docs/hardware/hardware-modeling.md" >}})
8. [Simulator Metrics]({{< relref "/docs/simulator/metrics.md" >}})

## Simulator architecture diagram

<img src='{{< relURL "images/joulie-arch-simulator.png" >}}' alt="Joulie simulator architecture overview">

See detailed explanation in:

- [Workload and Power Simulator]({{< relref "/docs/simulator/simulator.md" >}})
