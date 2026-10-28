---
title: "Simulator"
linkTitle: "Simulator"
weight: 30
aliases:
  - "/docs/simulator/model-changes/"
---

The Joulie simulator lets you evaluate energy management policies without the hardware they manage. Kubernetes scheduling and the Joulie components stay real; the simulator provides the hardware, its telemetry and the progress of the workload.

The simulator is Joulie's test bed for its behaviour (caps applied, placement, policies, governors), not evidence of energy savings: its hardware models rest on the assumptions listed in [Hardware Modeling]({{< relref "/docs/hardware/hardware-modeling.md" >}}). What meaningful experiments need is tracked in the milestone [Evidence-based evaluation](https://github.com/joulie-k8s/Joulie/milestone/1).

The real controller manager plans, the real scheduler extender scores, and the real agent caps, in one of two ways:

- **Emulated agents** read emulated `/sys` files and GPU tools, one agent per node, as on bare metal. This is [emulate mode]({{< relref "/docs/simulator/emulate-mode.md" >}}), which the [simulated cluster]({{< relref "/docs/simulator/simulated-cluster.md" >}}) runs on one machine with Docker.
- **The agent in pool mode** reads telemetry from and sends caps to the simulator's HTTP API, for many [KWOK](https://kwok.sigs.k8s.io/) nodes at once. This is [KWOK mode]({{< relref "/docs/simulator/simulator.md" >}}), which the benchmark experiments run.

Read the [Getting Started]({{< relref "/docs/getting-started/_index.md" >}}) pages first: the simulator pages assume the controller manager and agent loop.

## Read in this order

1. [Simulated Cluster]({{< relref "/docs/simulator/simulated-cluster.md" >}}): Joulie on emulated nodes, on one machine with Docker
2. [Workload Scenarios]({{< relref "/docs/simulator/scenarios.md" >}}): run a workload on the simulated cluster and check placement, slowdown and energy
3. [Installation]({{< relref "/docs/simulator/installation.md" >}}): install the simulator into your own cluster, or run it from a workstation
4. [Emulate Mode]({{< relref "/docs/simulator/emulate-mode.md" >}}): node trees, governors, hardware profiles, the cluster spec
5. [KWOK and Standalone Modes]({{< relref "/docs/simulator/simulator.md" >}}): label-modelled nodes, the facility model and the HTTP API
6. [Workload Generation]({{< relref "/docs/simulator/workload-generation.md" >}}): synthetic AI workload traces
7. [Workload Simulator]({{< relref "/docs/simulator/workload-simulator.md" >}}): how a trace runs and how caps slow it
8. [Power Simulator]({{< relref "/docs/simulator/power-simulator.md" >}}): how a modelled node's power, caps and temperature advance
9. [Hardware Modeling]({{< relref "/docs/hardware/hardware-modeling.md" >}}): the CPU and GPU power models and their sources
10. [Hardware Calibration]({{< relref "/docs/simulator/calibration.md" >}}): fit a hardware profile to a sweep measured on your node
11. [Simulator Metrics]({{< relref "/docs/simulator/metrics.md" >}}): every metric and event the simulator emits

Comparing with results of an older simulator: [`simulator/CHANGES.md`](https://github.com/joulie-k8s/Joulie/blob/main/simulator/CHANGES.md) lists every change that moved the numbers of modelled nodes, and how far.
