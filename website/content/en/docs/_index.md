---
title: Documentation
linkTitle: Docs
menu:
  main:
    weight: 10
---

Joulie is a Kubernetes-native energy management system that uses per-node digital twins to optimize data center power consumption.
It ingests real-time telemetry from every node (CPU/GPU power draw, thermal state, per-pod utilization) to maintain a continuously updated model of the cluster's energy state.
That model drives two things: power cap enforcement (via RAPL and NVML) and scheduling decisions that steer workloads toward the most energy-efficient nodes.

If you are completely new, the smoothest path is:

1. [Getting Started]({{< relref "/docs/getting-started/_index.md" >}})
2. [Architecture]({{< relref "/docs/architecture/_index.md" >}})
3. [Hardware]({{< relref "/docs/hardware/_index.md" >}})
4. [Simulator]({{< relref "/docs/simulator/_index.md" >}})
5. [Experiments]({{< relref "/docs/experiments/_index.md" >}})

Core mental model:

- telemetry feeds the digital twin,
- the twin drives operator decisions (power caps, node profiles),
- the scheduler extender reads twin state to steer new pod placement,
- feedback from new placements updates telemetry, closing the loop.

## Section guide

- [Getting Started]({{< relref "/docs/getting-started/_index.md" >}})
  - concepts, install, workload compatibility, configuration reference
- [Architecture]({{< relref "/docs/architecture/_index.md" >}})
  - operator/agent/twin/scheduler roles, CRDs, policy algorithms, telemetry/control interfaces, kubectl plugin
- [Hardware]({{< relref "/docs/hardware/_index.md" >}})
  - CPU and GPU support model, heterogeneity strategy, runtime caveats
- [Simulator]({{< relref "/docs/simulator/_index.md" >}})
  - trace-driven workload simulation, power modeling, facility stress
- [Experiments]({{< relref "/docs/experiments/_index.md" >}})
  - benchmark design and measured outcomes

## What to expect

- **Per-node digital twins**: telemetry → twin state → cap decisions and scheduling.
- **Kubernetes-native contracts**: 2 user-facing CRDs (`NodeHardware`, `NodeTwin`) + scheduling constraints as intent/supply language.
- **Observability tooling**: `kubectl joulie` plugin, Grafana dashboard, Prometheus metrics.
- **Practical path to adoption**: quickstart first, then progressive deep dives.
