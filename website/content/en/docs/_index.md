---
title: Documentation
linkTitle: Docs
menu:
  main:
    weight: 10
---

Joulie is a Kubernetes-native digital twin for energy-efficient data centers.
It ingests real-time telemetry from every node — CPU/GPU power draw, thermal state, per-pod utilization — to maintain a continuously updated model of the cluster's energy state.
That model drives two things: power cap enforcement (via RAPL and NVML) and scheduling decisions that steer workloads toward the most energy-efficient nodes.

If you are completely new, the smoothest path is:

1. [Getting Started]({{< relref "/docs/getting-started/_index.md" >}})
2. [Architecture]({{< relref "/docs/architecture/_index.md" >}})
3. [Hardware]({{< relref "/docs/hardware/_index.md" >}})
4. [Simulator]({{< relref "/docs/simulator/_index.md" >}})
5. [Experiments]({{< relref "/docs/experiments/_index.md" >}})

Core mental model:

- telemetry feeds the digital twin,
- the twin drives operator decisions (power caps, migration triggers),
- the scheduler extender reads twin state to steer new pod placement,
- feedback from new placements updates telemetry, closing the loop.

## Section guide

- [Getting Started]({{< relref "/docs/getting-started/_index.md" >}})
  - concepts, install, runtime modes, workload compatibility
- [Architecture]({{< relref "/docs/architecture/_index.md" >}})
  - operator/agent/twin roles, CRDs, policy model, telemetry/control interfaces
- [Hardware]({{< relref "/docs/hardware/_index.md" >}})
  - CPU and GPU support model, heterogeneity strategy, runtime caveats
- [Simulator]({{< relref "/docs/simulator/_index.md" >}})
  - digital-twin behavior, algorithms, runtime flow
- [Experiments]({{< relref "/docs/experiments/_index.md" >}})
  - benchmark design and measured outcomes

## What to expect

- **Digital twin model**: telemetry → twin state → cap decisions and scheduling.
- **Kubernetes-native contracts**: CRDs + scheduling constraints as intent/supply language.
- **Practical path to adoption**: quickstart first, then progressive deep dives.
