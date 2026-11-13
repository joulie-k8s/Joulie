---
title: Documentation
linkTitle: Docs
menu:
  main:
    weight: 10
---

Joulie is a Kubernetes-native energy management system that uses per-node digital twins to optimize data center power consumption.
The twins, fed by real-time telemetry, drive power caps (RAPL, NVML) and scheduling decisions that steer workloads toward the most energy-efficient nodes.

## How it works

- Telemetry feeds the digital twin.
- The twin drives the controller manager's decisions: power caps and node profiles.
- The scheduler extender reads the twin to steer new pods.
- New placements change the telemetry, closing the loop.

## What can Joulie do?

- **Trade power for throughput**: on 33 simulated H100 NVL nodes, 6 to 9 % less power for 6 to 10 % less GPU work, at about the same energy per unit.
- **Keep performance pods uncapped**: a workload-class annotation keeps performance-critical pods on uncapped nodes, where they wait when those nodes are full.
- **Show the energy state**: `kubectl joulie`, a Grafana dashboard and Prometheus metrics.

Results so far come from the simulator, whose energy figures do not show what Joulie saves on real hardware: see [Experiments]({{< relref "/docs/experiments/_index.md" >}}) and the milestone [Evidence-based evaluation](https://github.com/joulie-k8s/Joulie/milestone/1).

## Where to start

1. [Getting Started]({{< relref "/docs/getting-started/_index.md" >}}): concepts, install, configuration
2. [Architecture]({{< relref "/docs/architecture/_index.md" >}}): how the components work
3. [Hardware]({{< relref "/docs/hardware/_index.md" >}}): CPU and GPU support
4. [Simulator]({{< relref "/docs/simulator/_index.md" >}}): a cluster without the hardware
5. [Experiments]({{< relref "/docs/experiments/_index.md" >}}): what the simulator measures
