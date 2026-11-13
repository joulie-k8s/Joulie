---
title: "Simulator"
linkTitle: "Simulator"
weight: 30
aliases:
  - "/docs/simulator/model-changes/"
---

The simulator lets you run Joulie's real components without the hardware they manage: it provides the hardware, its telemetry and the progress of the workload. It is a test bed for Joulie's behaviour (caps applied, placement, policies, governors), not evidence of energy savings: its models rest on the assumptions in [Hardware Modeling]({{< relref "/docs/hardware/hardware-modeling.md" >}}).

| Mode | What it runs | Use it to |
|---|---|---|
| [Simulated cluster]({{< relref "/docs/simulator/simulated-cluster.md" >}}) | k3s in Docker with emulated nodes and the real Joulie chart, on one machine | try Joulie, or develop it, with nothing but Docker |
| [Emulate mode]({{< relref "/docs/simulator/emulate-mode.md" >}}) | emulated `/sys` files and GPU tools, one real agent per node | test discovery, capping and governors as on bare metal |
| [KWOK mode]({{< relref "/docs/simulator/simulator.md" >}}) | many [KWOK](https://kwok.sigs.k8s.io/) nodes, the agent in pool mode over HTTP | run benchmark experiments at cluster scale |
| [Standalone]({{< relref "/docs/simulator/simulator.md#standalone-mode" >}}) | no Kubernetes; the simulator places jobs and runs the policy baselines | sweep policies fast in simulated time |

Read [Getting Started]({{< relref "/docs/getting-started/_index.md" >}}) first.

## Read in this order

1. [Simulated Cluster]({{< relref "/docs/simulator/simulated-cluster.md" >}}): Joulie on one machine with Docker
2. [Workload Scenarios]({{< relref "/docs/simulator/scenarios.md" >}}): run and check a workload
3. [Installation]({{< relref "/docs/simulator/installation.md" >}}): the simulator in your own cluster
4. [Emulate Mode]({{< relref "/docs/simulator/emulate-mode.md" >}}): node trees, hardware profiles, the cluster spec
5. [KWOK and Standalone Modes]({{< relref "/docs/simulator/simulator.md" >}}): label-modelled nodes and the HTTP API
6. [Workload Generation]({{< relref "/docs/simulator/workload-generation.md" >}}): synthetic traces
7. [Workload Simulator]({{< relref "/docs/simulator/workload-simulator.md" >}}): how a trace runs under caps
8. [Power Simulator]({{< relref "/docs/simulator/power-simulator.md" >}}): how a modelled node advances
9. [Hardware Modeling]({{< relref "/docs/hardware/hardware-modeling.md" >}}): the power models and their sources
10. [Hardware Calibration]({{< relref "/docs/simulator/calibration.md" >}}): fit a profile to your node
11. [Simulator Metrics]({{< relref "/docs/simulator/metrics.md" >}}): every metric and event

What meaningful experiments need is tracked in the milestone [Evidence-based evaluation](https://github.com/joulie-k8s/Joulie/milestone/1); [`simulator/CHANGES.md`](https://github.com/joulie-k8s/Joulie/blob/main/simulator/CHANGES.md) lists every change that moved the simulator's numbers.
