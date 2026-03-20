---
title: "Simulator"
linkTitle: "Simulator"
weight: 30
---

The Joulie simulator lets you evaluate energy management policies without physical hardware. It provides a controlled, reproducible environment for benchmarking power savings and scheduling strategies.

The simulator keeps scheduling behavior real while simulating telemetry, control dynamics, and workload progression.

The simulator mirrors the real Joulie architecture. The operator, agent, and scheduler extender are the real components running against simulated hardware:
- the real operator computes desired state (`NodeTwin.spec`, `NodeTwin.status`),
- the real agent (pool mode) realizes caps via HTTP against the simulator,
- the real scheduler extender reads `NodeTwin.status` and applies workload-class-aware scoring,
- workloads carry trace-defined fields (criticality, cap sensitivity),
- facility stress model (`simulator/pkg/facility`) provides PSU and cooling stress signals.

The heterogeneous benchmark (`experiments/02-heterogeneous-benchmark/`) demonstrates the full architecture across three baselines: no Joulie, static partition, and queue-aware policy with scheduler extender steering.

Use simulator docs after you are familiar with the core operator/agent control loop in Getting Started + Architecture.

## Read in this order

1. [Installation]({{< relref "/docs/simulator/installation.md" >}}) -- deploy the simulator stack locally or in CI
2. [Workload and Power Simulator]({{< relref "/docs/simulator/simulator.md" >}}) -- architecture overview and how simulated components interact
3. [Workload Generation]({{< relref "/docs/simulator/workload-generation.md" >}}) -- trace-driven job creation and replay
4. [Workload Distributions]({{< relref "/docs/simulator/workload-distributions.md" >}}) -- arrival rates, duration profiles, resource demand curves
5. [Kubernetes AI Workloads]({{< relref "/docs/simulator/kubernetes-ai-workloads.md" >}}) -- GPU-heavy training and inference workload models
6. [Workload Simulator]({{< relref "/docs/simulator/workload-simulator.md" >}}) -- per-pod lifecycle simulation and utilization dynamics
7. [Power Simulator]({{< relref "/docs/simulator/power-simulator.md" >}}) -- node-level power draw and thermal response modeling
8. [Hardware Modeling]({{< relref "/docs/hardware/hardware-modeling.md" >}}) -- CPU/GPU power curves used by the simulator
9. [Simulator Metrics]({{< relref "/docs/simulator/metrics.md" >}}) -- Prometheus metrics exported by simulated nodes

## Simulator architecture diagram

<img src='{{< relURL "images/joulie-arch-simulator.png" >}}' alt="Joulie simulator architecture overview">

See detailed explanation in:

- [Workload and Power Simulator]({{< relref "/docs/simulator/simulator.md" >}})
